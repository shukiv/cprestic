package node

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/resticrun"
)

// AttachRequest points a fresh server at backups that already exist.
//
// It is the other half of the recovery key. A server that has burned down
// is replaced by one that knows nothing; given where the backups are and
// the password that reads them, this makes that server able to restore
// them without inventing a new repository.
type AttachRequest struct {
	// Destination is where the backups are, filled in as it would be for
	// a new one.
	Destination nodestore.Destination
	Secrets     map[string]string
	// RepositoryPath is the folder inside the destination — the same one
	// the old server wrote to, usually its hostname.
	RepositoryPath string
	// Password is the recovery key: the restic password for that
	// repository. Nothing here can read it without this.
	Password string
}

// Contents is what an attached repository turns out to hold.
type Contents struct {
	Accounts []AccountBackups
	// System is true when the repository holds a backup of a server's own
	// settings, which is what a replacement machine needs first.
	System     bool
	SystemAt   time.Time
	Snapshots  int
	Repository nodestore.Repository
}

// AccountBackups is one account as the repository knows it.
type AccountBackups struct {
	Account   string
	Snapshots int
	Latest    time.Time
	// LatestID is the newest backup of this account in this repository.
	// The page that offers to recover an account has to say which backup
	// it means, and this is the one an operator rebuilding a server
	// wants unless they say otherwise.
	LatestID string
}

// Attach registers an existing repository on this server.
//
// The password is proved before anything is stored: a repository this
// server cannot read is not a destination, it is a typo, and saving it
// would leave an operator in a disaster believing they had their backups
// back.
func (e *Engine) Attach(ctx context.Context, req AttachRequest) (Contents, error) {
	if strings.TrimSpace(req.Password) == "" {
		return Contents{}, fmt.Errorf(
			"node: the recovery key is required — without it these backups cannot be read")
	}
	if strings.TrimSpace(req.RepositoryPath) == "" {
		return Contents{}, fmt.Errorf(
			"node: which folder inside that destination? It is usually the old server's hostname")
	}

	spec := destination.Spec{
		Type:    destination.Type(req.Destination.Type),
		Config:  req.Destination.Config,
		Secrets: req.Secrets,
	}
	built, err := destination.Build(spec)
	if err != nil {
		return Contents{}, err
	}

	// Read it before writing anything down.
	snapshots, err := e.runner.Snapshots(ctx, resticrun.Repository{
		Dest: built, Path: req.RepositoryPath, Password: req.Password,
	}, resticrun.SnapshotFilter{})
	if err != nil {
		return Contents{}, fmt.Errorf(
			"node: could not read that repository: %w. Check the folder and the recovery key", err)
	}

	secretID, err := SealCredentials(e.store, e.vault, req.Secrets)
	if err != nil {
		return Contents{}, err
	}
	req.Destination.CredentialsSecretID = secretID
	storedDest, err := e.store.PutDestination(req.Destination)
	if err != nil {
		return Contents{}, err
	}

	passwordID, err := SealRepositoryPassword(e.store, e.vault, req.Password)
	if err != nil {
		return Contents{}, err
	}
	now := time.Now().UTC()
	repo, err := e.store.PutRepository(nodestore.Repository{
		DestinationID:    storedDest.ID,
		Path:             req.RepositoryPath,
		PasswordSecretID: passwordID,
		// It exists already: this server did not create it and must not
		// try to, and restic init on an existing repository fails anyway.
		InitialisedAt: &now,
		// The key came from somewhere else, which is the only reason this
		// server can read anything. Nothing to warn about.
		RecoveryNotedAt: &now,
	})
	if err != nil {
		return Contents{}, err
	}

	contents := Summarise(snapshots)
	contents.Repository = repo
	return contents, nil
}

// Contents lists what a repository this server already knows about holds.
func (e *Engine) Contents(ctx context.Context, repositoryID string) (Contents, error) {
	repo, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return Contents{}, err
	}
	snapshots, err := e.runner.Snapshots(ctx, repo, resticrun.SnapshotFilter{})
	if err != nil {
		return Contents{}, err
	}
	contents := Summarise(snapshots)
	stored, err := e.store.Repository(repositoryID)
	if err != nil {
		return Contents{}, err
	}
	contents.Repository = stored
	return contents, nil
}

// Summarise turns a list of snapshots into what an operator is deciding
// from: which accounts are in there, how many backups each has, and when
// the last one was taken.
func Summarise(snapshots []resticrun.Snapshot) Contents {
	byAccount := map[string]*AccountBackups{}
	contents := Contents{Snapshots: len(snapshots)}

	for _, snapshot := range snapshots {
		account := snapshot.Account()
		if account == "" {
			continue
		}
		if account == cpanel.SystemAccount {
			contents.System = true
			if snapshot.Time.After(contents.SystemAt) {
				contents.SystemAt = snapshot.Time
			}
			continue
		}
		found, seen := byAccount[account]
		if !seen {
			found = &AccountBackups{Account: account}
			byAccount[account] = found
		}
		found.Snapshots++
		if snapshot.Time.After(found.Latest) {
			found.Latest = snapshot.Time
			found.LatestID = snapshot.ID
		}
	}

	for _, account := range byAccount {
		contents.Accounts = append(contents.Accounts, *account)
	}
	sort.Slice(contents.Accounts, func(i, j int) bool {
		return contents.Accounts[i].Account < contents.Accounts[j].Account
	})
	return contents
}

// LatestSnapshot is the newest backup of one account in one repository.
//
// Recovering an account is chosen by account, not by snapshot: an
// operator rebuilding a lost server picks the customer, and means the
// last good backup of them. Resolving it here rather than in a form also
// means a page that has been open since before tonight's backup cannot
// quietly restore yesterday's.
func (e *Engine) LatestSnapshot(ctx context.Context, repositoryID, account string) (string, error) {
	return e.SnapshotAsOf(ctx, repositoryID, account, time.Time{})
}

// SnapshotAsOf is the newest backup of one account taken at or before a
// moment, or the newest of all when that moment is zero.
//
// Restoring is not always "put back the last one". An account that was
// broken this morning wants last night's, and an operator rebuilding
// several accounts means the same date for all of them rather than a
// snapshot id each. So a date is what the page asks for, and each account
// resolves it to its own backup: they are not taken at the same minute,
// and a run that was skipped for one account must not silently give it a
// different day's data than the others without saying so.
func (e *Engine) SnapshotAsOf(ctx context.Context, repositoryID, account string,
	asOf time.Time) (string, error) {

	repo, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return "", err
	}
	snapshots, err := e.runner.Snapshots(ctx, repo, resticrun.SnapshotFilter{})
	if err != nil {
		return "", err
	}
	chosen, partial, held := newestForRecovery(snapshots, account, asOf)
	switch {
	case chosen.ID != "":
		return chosen.ID, nil
	case partial.ID != "":
		return "", fmt.Errorf(
			"node: every backup of %s here was taken without %s, so none of them "+
				"holds the whole account -- the newest is %s from %s, and restoring "+
				"it would give back an account missing that",
			account, saidList(partial.Skipped()), snapshotName(partial),
			partial.Time.Format("2006-01-02 15:04"))
	case held > 0:
		return "", fmt.Errorf(
			"node: no backup of %s from %s or earlier -- the oldest here is newer",
			account, asOf.Format("2006-01-02"))
	default:
		return "", fmt.Errorf("node: this destination holds no backup of %s", account)
	}
}

// newestForRecovery separates the newest backup that holds the whole
// account from the newest that does not.
//
// A schedule may leave the databases, the home directory or the mail out.
// Putting an account back from one of those hands the customer an account
// missing whatever was skipped, and the only sign of it is that the
// restore finished. So whole-account recovery uses the complete one, and
// the partial one is carried back only to be named.
func newestForRecovery(snapshots []resticrun.Snapshot, account string,
	asOf time.Time) (chosen, partial resticrun.Snapshot, held int) {

	for _, snapshot := range snapshots {
		if snapshot.Account() != account {
			continue
		}
		held++
		if !asOf.IsZero() && snapshot.Time.After(asOf) {
			continue
		}
		if !snapshot.Complete() {
			if partial.ID == "" || snapshot.Time.After(partial.Time) {
				partial = snapshot
			}
			continue
		}
		if chosen.ID == "" || snapshot.Time.After(chosen.Time) {
			chosen = snapshot
		}
	}
	return chosen, partial, held
}

// snapshotName is what to call a snapshot in a sentence: restic's short
// id when it gave one, and the full id otherwise.
func snapshotName(snapshot resticrun.Snapshot) string {
	if snapshot.ShortID != "" {
		return snapshot.ShortID
	}
	return snapshot.ID
}

// saidList joins names the way a sentence does.
func saidList(names []string) string {
	switch len(names) {
	case 0:
		return "anything"
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}
