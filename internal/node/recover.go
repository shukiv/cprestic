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
