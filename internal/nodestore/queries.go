package nodestore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shuki/cprest/internal/job"
)

// NewID returns a random identifier for a stored record.
func NewID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing means the system is in no state to be
		// taking backups either.
		panic("nodestore: no randomness available: " + err.Error())
	}
	return hex.EncodeToString(raw)
}

// --- settings ---

const settingsKey = "node"

// Settings reads the node's configuration, returning defaults when it has
// not been configured yet.
func (s *Store) Settings() (Settings, error) {
	var settings Settings
	err := s.get(bucketSettings, settingsKey, &settings)
	if errors.Is(err, ErrNotFound) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// DefaultSettings matches fleet mode's defaults exactly. Snapshot paths
// embed the staging root, so a standalone server that later joins a fleet
// must have been using the same one all along.
func DefaultSettings() Settings {
	return Settings{
		StagingRoot:   "/var/lib/cprest/staging",
		MaxConcurrent: 1,
		SafetyMargin:  0.2,
		ResticBinary:  "restic",
		ResticCache:   "/var/cache/cprest/restic",
		ConfigDir:     "/etc/cprest",
	}
}

// SaveSettings writes the node's configuration.
func (s *Store) SaveSettings(settings Settings) error {
	return s.put(bucketSettings, settingsKey, settings)
}

// --- secrets ---

// PutSecret stores a sealed credential and returns its id.
func (s *Store) PutSecret(kind string, ciphertext []byte, keyID string) (string, error) {
	secret := Secret{
		ID: NewID(), Kind: kind, Ciphertext: ciphertext,
		KeyID: keyID, CreatedAt: time.Now().UTC(),
	}
	if err := s.put(bucketSecrets, secret.ID, secret); err != nil {
		return "", err
	}
	return secret.ID, nil
}

// Secret reads a sealed credential.
func (s *Store) Secret(id string) (Secret, error) {
	var secret Secret
	if err := s.get(bucketSecrets, id, &secret); err != nil {
		return Secret{}, err
	}
	return secret, nil
}

// --- destinations ---

// PutDestination stores a destination, assigning an id when it has none.
func (s *Store) PutDestination(dest Destination) (Destination, error) {
	if dest.ID == "" {
		dest.ID = NewID()
	}
	if dest.CreatedAt.IsZero() {
		dest.CreatedAt = time.Now().UTC()
	}
	if dest.Config == nil {
		dest.Config = map[string]string{}
	}
	return dest, s.put(bucketDestinations, dest.ID, dest)
}

// Destination reads one destination.
func (s *Store) Destination(id string) (Destination, error) {
	var dest Destination
	if err := s.get(bucketDestinations, id, &dest); err != nil {
		return Destination{}, err
	}
	return dest, nil
}

// Destinations lists every destination, by name.
func (s *Store) Destinations() ([]Destination, error) {
	var destinations []Destination
	err := s.forEach(bucketDestinations, func(_ string, raw []byte) error {
		var dest Destination
		if err := json.Unmarshal(raw, &dest); err != nil {
			return err
		}
		destinations = append(destinations, dest)
		return nil
	})
	sort.Slice(destinations, func(i, j int) bool {
		return destinations[i].Name < destinations[j].Name
	})
	return destinations, err
}

// DeleteDestination removes a destination and the repository record that
// belongs to it.
//
// The backups already stored there are not touched — this only forgets how
// to reach them. It is refused while a schedule still points at the
// repository, because that schedule would then silently stop making one of
// the copies it promises.
func (s *Store) DeleteDestination(id string) error {
	repos, err := s.Repositories()
	if err != nil {
		return err
	}
	policies, err := s.Policies()
	if err != nil {
		return err
	}

	var owned []Repository
	for _, repo := range repos {
		if repo.DestinationID == id {
			owned = append(owned, repo)
		}
	}
	for _, repo := range owned {
		for _, policy := range policies {
			for _, target := range policy.RepositoryIDs {
				if target == repo.ID {
					return fmt.Errorf(
						"nodestore: the schedule %q still sends backups here; "+
							"remove it from that schedule first", policy.Name)
				}
			}
		}
	}

	for _, repo := range owned {
		if err := s.delete(bucketRepositories, repo.ID); err != nil {
			return err
		}
	}
	return s.delete(bucketDestinations, id)
}

// --- repositories ---

// PutRepository stores a repository, filling in the chunker source.
//
// Chunker parameters are fixed when a repository is created and can never
// change, so every repository after the first must copy them from the
// first. Fleet mode enforces this with a database trigger; standalone mode
// has to do it here. See docs/DESIGN.md §7.
func (s *Store) PutRepository(repo Repository) (Repository, error) {
	if repo.ID != "" {
		return repo, s.put(bucketRepositories, repo.ID, repo)
	}

	existing, err := s.Repositories()
	if err != nil {
		return Repository{}, err
	}
	repo.ID = NewID()
	repo.CreatedAt = time.Now().UTC()
	if repo.ChunkerSourceRepoID == "" && len(existing) > 0 {
		repo.ChunkerSourceRepoID = chunkerSource(existing)
	}
	return repo, s.put(bucketRepositories, repo.ID, repo)
}

// chunkerSource picks the server's first repository, which is the one every
// later repository copies its chunker parameters from.
func chunkerSource(existing []Repository) string {
	oldest := existing[0]
	for _, repo := range existing[1:] {
		if repo.CreatedAt.Before(oldest.CreatedAt) {
			oldest = repo
		}
	}
	// Follow the chain to its root, so a third repository points at the
	// same source as the second rather than at the second itself.
	byID := map[string]Repository{}
	for _, repo := range existing {
		byID[repo.ID] = repo
	}
	for seen := 0; oldest.ChunkerSourceRepoID != "" && seen < len(existing); seen++ {
		parent, present := byID[oldest.ChunkerSourceRepoID]
		if !present {
			break
		}
		oldest = parent
	}
	return oldest.ID
}

// Repository reads one repository.
func (s *Store) Repository(id string) (Repository, error) {
	var repo Repository
	if err := s.get(bucketRepositories, id, &repo); err != nil {
		return Repository{}, err
	}
	return repo, nil
}

// Repositories lists every repository, oldest first.
func (s *Store) Repositories() ([]Repository, error) {
	var repos []Repository
	err := s.forEach(bucketRepositories, func(_ string, raw []byte) error {
		var repo Repository
		if err := json.Unmarshal(raw, &repo); err != nil {
			return err
		}
		repos = append(repos, repo)
		return nil
	})
	sort.Slice(repos, func(i, j int) bool { return repos[i].CreatedAt.Before(repos[j].CreatedAt) })
	return repos, err
}

// MarkRepositoryInitialised records that "restic init" succeeded.
func (s *Store) MarkRepositoryInitialised(id string) error {
	repo, err := s.Repository(id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	repo.InitialisedAt = &now
	return s.put(bucketRepositories, id, repo)
}

// --- policies ---

// PutPolicy stores a policy.
func (s *Store) PutPolicy(policy Policy) (Policy, error) {
	if policy.ID == "" {
		policy.ID = NewID()
		policy.CreatedAt = time.Now().UTC()
	}
	if policy.PayloadMode == "" {
		policy.PayloadMode = "split"
	}
	if policy.Compression == "" {
		policy.Compression = "auto"
	}
	return policy, s.put(bucketPolicies, policy.ID, policy)
}

// Policy reads one policy.
func (s *Store) Policy(id string) (Policy, error) {
	var policy Policy
	if err := s.get(bucketPolicies, id, &policy); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// Policies lists every policy, by name.
func (s *Store) Policies() ([]Policy, error) {
	var policies []Policy
	err := s.forEach(bucketPolicies, func(_ string, raw []byte) error {
		var policy Policy
		if err := json.Unmarshal(raw, &policy); err != nil {
			return err
		}
		policies = append(policies, policy)
		return nil
	})
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	return policies, err
}

// DeletePolicy removes a policy. Its job history is kept.
func (s *Store) DeletePolicy(id string) error { return s.delete(bucketPolicies, id) }

// SetPolicyLastRun records a firing, so a restart neither skips a window
// nor replays past ones.
func (s *Store) SetPolicyLastRun(id string, at time.Time) error {
	policy, err := s.Policy(id)
	if err != nil {
		return err
	}
	at = at.UTC()
	policy.LastRunAt = &at
	return s.put(bucketPolicies, id, policy)
}

// --- notification channels ---

// PutChannel stores somewhere to send notifications.
func (s *Store) PutChannel(channel Channel) (Channel, error) {
	if channel.ID == "" {
		channel.ID = NewID()
		channel.CreatedAt = time.Now().UTC()
	}
	return channel, s.put(bucketChannels, channel.ID, channel)
}

// Channel reads one back.
func (s *Store) Channel(id string) (Channel, error) {
	var channel Channel
	return channel, s.get(bucketChannels, id, &channel)
}

// Channels lists them, oldest first, so the order does not move about.
func (s *Store) Channels() ([]Channel, error) {
	var channels []Channel
	err := s.forEach(bucketChannels, func(_ string, raw []byte) error {
		var channel Channel
		if err := json.Unmarshal(raw, &channel); err != nil {
			return err
		}
		channels = append(channels, channel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(channels, func(i, j int) bool {
		return channels[i].CreatedAt.Before(channels[j].CreatedAt)
	})
	return channels, nil
}

// DeleteChannel removes one.
func (s *Store) DeleteChannel(id string) error { return s.delete(bucketChannels, id) }

// --- jobs ---

// PutJob stores a job.
func (s *Store) PutJob(j Job) (Job, error) {
	if j.ID == "" {
		j.ID = NewID()
		j.QueuedAt = time.Now().UTC()
	}
	return j, s.put(bucketJobs, j.ID, j)
}

// Job reads one job.
// SetJobProgress records how far a running job has got.
//
// It is a read-modify-write of one record rather than a field update,
// which bbolt has no notion of. Progress arrives about once a second per
// job and the caller throttles it further, so this stays cheap. A job that
// has already finished is left alone: a late status line must not reopen
// a closed record.
func (s *Store) SetJobProgress(id string, progress JobProgress) error {
	stored, err := s.Job(id)
	if err != nil {
		return err
	}
	if stored.Status.Terminal() {
		return nil
	}
	stored.Progress = &progress
	_, err = s.PutJob(stored)
	return err
}

func (s *Store) Job(id string) (Job, error) {
	var j Job
	if err := s.get(bucketJobs, id, &j); err != nil {
		return Job{}, err
	}
	return j, nil
}

// Jobs lists job history, newest first, capped at limit (zero means all).
func (s *Store) Jobs(limit int) ([]Job, error) {
	var jobs []Job
	err := s.forEach(bucketJobs, func(_ string, raw []byte) error {
		var j Job
		if err := json.Unmarshal(raw, &j); err != nil {
			return err
		}
		jobs = append(jobs, j)
		return nil
	})
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].QueuedAt.After(jobs[j].QueuedAt) })
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, err
}

// RunningJobFor reports whether an account already has work in flight.
// Backups and restores of one account stage in the same place, so they
// cannot overlap.
func (s *Store) RunningJobFor(account string) (bool, error) {
	jobs, err := s.Jobs(0)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.Account == account && !j.Status.Terminal() {
			return true, nil
		}
	}
	restores, err := s.Restores(0)
	if err != nil {
		return false, err
	}
	for _, restore := range restores {
		if restore.Account == account && !restore.Status.Terminal() {
			return true, nil
		}
	}
	return false, nil
}

// --- restores ---

// PutRestore stores a restore.
func (s *Store) PutRestore(restore Restore) (Restore, error) {
	if restore.ID == "" {
		restore.ID = NewID()
		restore.QueuedAt = time.Now().UTC()
	}
	if restore.Status == "" {
		restore.Status = job.StatusPending
	}
	return restore, s.put(bucketRestores, restore.ID, restore)
}

// Restore reads one restore.
func (s *Store) Restore(id string) (Restore, error) {
	var restore Restore
	if err := s.get(bucketRestores, id, &restore); err != nil {
		return Restore{}, err
	}
	return restore, nil
}

// Restores lists restore history, newest first, capped at limit.
func (s *Store) Restores(limit int) ([]Restore, error) {
	var restores []Restore
	err := s.forEach(bucketRestores, func(_ string, raw []byte) error {
		var restore Restore
		if err := json.Unmarshal(raw, &restore); err != nil {
			return err
		}
		restores = append(restores, restore)
		return nil
	})
	sort.Slice(restores, func(i, j int) bool {
		return restores[i].QueuedAt.After(restores[j].QueuedAt)
	})
	if limit > 0 && len(restores) > limit {
		restores = restores[:limit]
	}
	return restores, err
}

// PendingWork returns the queued backup and restore, if any, oldest first.
// Restores come back first: someone is usually waiting for one.
func (s *Store) PendingWork() (*Restore, *Job, error) {
	restores, err := s.Restores(0)
	if err != nil {
		return nil, nil, err
	}
	for i := len(restores) - 1; i >= 0; i-- {
		if restores[i].Status == job.StatusPending {
			return &restores[i], nil, nil
		}
	}

	jobs, err := s.Jobs(0)
	if err != nil {
		return nil, nil, err
	}
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].Status == job.StatusPending {
			return nil, &jobs[i], nil
		}
	}
	return nil, nil, nil
}

// --- account identities ---

// PutIdentity records which unix account a cPanel name means.
func (s *Store) PutIdentity(identity AccountIdentity) (AccountIdentity, error) {
	now := time.Now().UTC()
	if identity.CreatedAt.IsZero() {
		identity.CreatedAt = now
	}
	identity.LastSeen = now
	return identity, s.put(bucketIdentities, identity.Account, identity)
}

// Identity reads one back.
func (s *Store) Identity(account string) (AccountIdentity, error) {
	var identity AccountIdentity
	return identity, s.get(bucketIdentities, account, &identity)
}

// Identities lists them all.
func (s *Store) Identities() ([]AccountIdentity, error) {
	var identities []AccountIdentity
	err := s.forEach(bucketIdentities, func(_ string, raw []byte) error {
		var identity AccountIdentity
		if err := json.Unmarshal(raw, &identity); err != nil {
			return err
		}
		identities = append(identities, identity)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(identities, func(i, j int) bool {
		return identities[i].Account < identities[j].Account
	})
	return identities, nil
}
