// Package node runs cprest on a single cPanel server with no controller.
//
// It is the same machinery as fleet mode with the control plane removed:
// state lives in a local bbolt file instead of PostgreSQL, scheduling
// happens here instead of on a controller, and configuration comes from the
// WHM plugin instead of a CLI. The parts that make backups correct —
// payload planning, staging, the restic runner, reassembly — are the fleet
// implementations, reused rather than rewritten.
// See docs/adr/0007-standalone-mode.md.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/shuki/cprest/internal/agent"
	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/staging"
	"github.com/shuki/cprest/internal/vault"
)

// Engine is the standalone node.
type Engine struct {
	store    *nodestore.Store
	vault    *vault.Vault
	provider cpanel.Provider
	runner   *resticrun.Runner
	worker   *agent.Agent

	// progressMu guards lastProgress, which throttles how often a running
	// job's percentage is written.
	progressMu   sync.Mutex
	lastProgress map[string]progressMark
	staging      *staging.Manager
	log          *slog.Logger

	settings nodestore.Settings
}

// Config assembles an Engine.
type Config struct {
	Store    *nodestore.Store
	Vault    *vault.Vault
	Provider cpanel.Provider
	Log      *slog.Logger
}

// New builds an Engine from stored settings.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil || cfg.Vault == nil || cfg.Provider == nil {
		return nil, errors.New("node: store, vault and provider are required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	settings, err := cfg.Store.Settings()
	if err != nil {
		return nil, err
	}
	if settings.ConfigDir == "" {
		// State written before this setting existed.
		settings.ConfigDir = nodestore.DefaultSettings().ConfigDir
	}
	for _, dir := range []string{settings.StagingRoot, settings.ResticCache, settings.ConfigDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("node: create %s: %w", dir, err)
		}
	}

	runner := resticrun.New(resticrun.Config{
		Binary:     settings.ResticBinary,
		RuntimeDir: settings.StagingRoot,
		CacheDir:   settings.ResticCache,
		CACertPath: settings.ResticCACert,
	}, nil)

	stagingManager := &staging.Manager{
		Root:              settings.StagingRoot,
		SafetyMarginRatio: settings.SafetyMargin,
		MaxConcurrent:     settings.MaxConcurrent,
	}

	// The worker is the fleet agent with no controller attached: RunJob and
	// RunRestore never touch its client, so the same tested code path runs
	// here.
	worker := agent.New(agent.Config{
		Provider:      cfg.Provider,
		Staging:       stagingManager,
		Runner:        runner,
		Log:           log,
		Hostname:      settings.Hostname,
		ResticVersion: "",
	})

	engine := &Engine{
		store: cfg.Store, vault: cfg.Vault, provider: cfg.Provider,
		runner: runner, worker: worker, staging: stagingManager,
		log: log, settings: settings, lastProgress: map[string]progressMark{},
	}
	// A backup of a large account takes minutes, and an operator watching
	// it deserves to see it move. restic reports about once a second per
	// repository; the engine writes far less often than that.
	worker.OnProgress = engine.recordProgress
	if err := engine.RecoverFromRestart(); err != nil {
		return nil, err
	}
	return engine, nil
}

// RecoverFromRestart closes out work that was in flight when the process
// died, and clears what it left on disk.
//
// Fleet mode has lease expiry for this. Standalone has nothing: a job stuck
// in "running" would make the account look permanently busy, so every later
// backup of it would be skipped — silently, every night — and its staging
// directory would block the next attempt anyway.
func (e *Engine) RecoverFromRestart() error {
	jobs, err := e.store.Jobs(0)
	if err != nil {
		return err
	}
	var interrupted int
	for _, stored := range jobs {
		// Only work that was actually in flight. Something merely queued
		// is still perfectly good work, and failing it would mean a
		// restart quietly emptied the queue.
		if stored.Status != job.StatusRunning {
			continue
		}
		stored.Status = job.StatusFailed
		stored.StagingErr = "interrupted by a service restart"
		finished := time.Now().UTC()
		stored.FinishedAt = &finished
		if _, err := e.store.PutJob(stored); err != nil {
			return err
		}
		interrupted++
	}

	restores, err := e.store.Restores(0)
	if err != nil {
		return err
	}
	for _, stored := range restores {
		if stored.Status != job.StatusRunning {
			continue
		}
		stored.Status = job.StatusFailed
		stored.Error = "interrupted by a service restart"
		finished := time.Now().UTC()
		stored.FinishedAt = &finished
		if _, err := e.store.PutRestore(stored); err != nil {
			return err
		}
		interrupted++
	}

	// Whatever those left behind is debris. Finished output is kept: a
	// rebuilt archive somebody was told to download should still be there
	// after a restart.
	dirs, err := e.staging.Active()
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := e.staging.Release(&dir); err != nil {
			e.log.Error("remove stale staging", "path", dir.Path, "error", err)
			continue
		}
		e.log.Warn("removed a staging directory left by a previous run",
			"key", dir.Key, "path", dir.Path)
	}

	if interrupted > 0 {
		e.log.Warn("marked interrupted work as failed", "count", interrupted)
	}
	return nil
}

// Store exposes the state file to the UI.
func (e *Engine) Store() *nodestore.Store { return e.store }

// Vault exposes the credential vault to the UI, which seals what an
// operator types before it is written.
func (e *Engine) Vault() *vault.Vault { return e.vault }

// Settings returns the node's configuration as loaded at startup.
func (e *Engine) Settings() nodestore.Settings { return e.settings }

// Accounts lists the cPanel accounts on this server.
func (e *Engine) Accounts(ctx context.Context) ([]cpanel.AccountInfo, error) {
	return e.provider.Accounts(ctx)
}

// ProbeCapabilities reports which pkgacct flags this host supports, and
// records them so the UI can warn about a host that cannot disable
// compression.
func (e *Engine) ProbeCapabilities(ctx context.Context) error {
	caps, err := e.provider.Capabilities(ctx)
	if err != nil {
		return err
	}
	flags := map[string]string{}
	for name, flag := range map[string]string{
		"nocompress":  caps.NoCompressFlag,
		"skiphomedir": caps.SkipHomedirFlag,
		"skipdb":      caps.SkipDBFlag,
	} {
		if flag != "" {
			flags[name] = flag
		}
	}
	e.settings.PkgacctFlags = flags
	return e.store.SaveSettings(e.settings)
}

// OpenRepository resolves a stored repository into something restic can be
// pointed at.
//
// forMaintenance addresses the endpoint that permits deletes, where the
// destination declares one.
func (e *Engine) OpenRepository(repositoryID string, forMaintenance bool) (resticrun.Repository, error) {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return resticrun.Repository{}, err
	}
	dest, err := e.store.Destination(repo.DestinationID)
	if err != nil {
		return resticrun.Repository{}, err
	}

	spec, err := e.buildSpec(dest)
	if err != nil {
		return resticrun.Repository{}, err
	}
	if forMaintenance {
		spec = destination.ForMaintenance(spec)
	}
	built, err := destination.Build(spec)
	if err != nil {
		return resticrun.Repository{}, err
	}

	password, err := e.openSecret(repo.PasswordSecretID)
	if err != nil {
		return resticrun.Repository{}, fmt.Errorf("node: repository password: %w", err)
	}
	return resticrun.Repository{Dest: built, Path: repo.Path, Password: string(password)}, nil
}

// buildSpec combines a destination's stored settings with its unsealed
// credentials.
func (e *Engine) buildSpec(dest nodestore.Destination) (destination.Spec, error) {
	secrets := map[string]string{}
	if dest.CredentialsSecretID != "" {
		plaintext, err := e.openSecret(dest.CredentialsSecretID)
		if err != nil {
			return destination.Spec{}, fmt.Errorf("node: destination credentials: %w", err)
		}
		if err := decodeSecrets(plaintext, &secrets); err != nil {
			return destination.Spec{}, err
		}
	}
	config := make(map[string]string, len(dest.Config))
	for key, value := range dest.Config {
		config[key] = value
	}
	return destination.Spec{
		Type: destination.Type(dest.Type), Config: config, Secrets: secrets,
	}, nil
}

func (e *Engine) openSecret(id string) ([]byte, error) {
	secret, err := e.store.Secret(id)
	if err != nil {
		return nil, err
	}
	return e.vault.Open(secret.Ciphertext)
}

// TestDestination checks that a destination is reachable and configured,
// without touching any repository.
func (e *Engine) TestDestination(ctx context.Context, destinationID string) error {
	dest, err := e.store.Destination(destinationID)
	if err != nil {
		return err
	}
	spec, err := e.buildSpec(dest)
	if err != nil {
		return err
	}
	built, err := destination.Build(spec)
	if err != nil {
		return err
	}

	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	checkErr := built.Preflight(checkCtx)

	now := time.Now().UTC()
	dest.LastCheckedAt = &now
	dest.LastCheckError = ""
	if checkErr != nil {
		dest.LastCheckError = checkErr.Error()
	}
	if _, err := e.store.PutDestination(dest); err != nil {
		return err
	}
	return checkErr
}

// Snapshots lists an account's snapshots in a repository.
func (e *Engine) Snapshots(ctx context.Context, repositoryID, account string) ([]resticrun.Snapshot, error) {
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return nil, err
	}
	filter := resticrun.SnapshotFilter{}
	if account != "" {
		filter.Tags = []string{"account:" + account}
	}
	return e.runner.Snapshots(ctx, repo, filter)
}

// Browse lists the contents of a snapshot, so an operator can pick out the
// one file they need.
func (e *Engine) Browse(ctx context.Context, repositoryID, snapshotID string, subpaths ...string) ([]resticrun.Entry, error) {
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return nil, err
	}
	return e.runner.Ls(ctx, repo, snapshotID, subpaths...)
}

// assignmentFor turns a stored job into the assignment the fleet agent
// already knows how to execute.
func (e *Engine) assignmentFor(j nodestore.Job, policy nodestore.Policy,
	account cpanel.AccountInfo) (protocol.JobAssignment, error) {

	assignment := protocol.JobAssignment{
		JobID:          j.ID,
		AccountID:      j.Account,
		CPanelUser:     j.Account,
		SizeEstimate:   account.SizeBytes,
		PayloadMode:    policy.PayloadMode,
		Compression:    policy.Compression,
		LimitUploadKiB: policy.LimitUploadKiB,
	}
	for _, repositoryID := range policy.RepositoryIDs {
		target, err := e.targetFor(repositoryID)
		if err != nil {
			// A partial target list would silently reduce the number of
			// copies the policy promises.
			return protocol.JobAssignment{}, err
		}
		assignment.Targets = append(assignment.Targets, target)
	}
	if len(assignment.Targets) == 0 {
		return protocol.JobAssignment{}, fmt.Errorf("node: policy %s has no repositories", policy.Name)
	}
	return assignment, nil
}

func (e *Engine) targetFor(repositoryID string) (protocol.Target, error) {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return protocol.Target{}, err
	}
	dest, err := e.store.Destination(repo.DestinationID)
	if err != nil {
		return protocol.Target{}, err
	}
	spec, err := e.buildSpec(dest)
	if err != nil {
		return protocol.Target{}, err
	}
	if _, err := destination.Build(spec); err != nil {
		return protocol.Target{}, err
	}
	password, err := e.openSecret(repo.PasswordSecretID)
	if err != nil {
		return protocol.Target{}, err
	}
	return protocol.Target{
		RepositoryID: repositoryID,
		Spec:         spec,
		RepoPath:     repo.Path,
		RepoPassword: string(password),
	}, nil
}

// SaveDestination stores changes to an existing destination, after checking
// that what was typed still produces something restic can be pointed at.
func (e *Engine) SaveDestination(dest nodestore.Destination) error {
	spec, err := e.buildSpec(dest)
	if err != nil {
		return err
	}
	if _, err := destination.Build(spec); err != nil {
		return err
	}
	_, err = e.store.PutDestination(dest)
	return err
}

// KindVerify is a restore that is rehearsed and thrown away: it proves the
// backup can be rebuilt without touching the account or keeping anything.
const KindVerify = "verify"

// progressFloor is how much has to change before a running job's progress
// is written again: a percentage point, or two seconds. restic reports
// once a second per repository and a fleet backup runs several at once, so
// writing every line would rewrite the same record for no visible gain.
const (
	progressFloor    = 1.0
	progressInterval = 2 * time.Second
)

// recordProgress stores how far a running backup has got.
//
// It is called from the goroutine reading restic's output, so it holds its
// own lock, keeps the work small, and never fails the backup: a job that
// cannot write its progress is still a job that is running.
func (e *Engine) recordProgress(jobID, repositoryID string, progress resticrun.Progress) {
	percent := progress.PercentDone * 100
	if percent > 100 {
		percent = 100
	}

	e.progressMu.Lock()
	last, seen := e.lastProgress[jobID]
	now := time.Now()
	if seen && percent-last.percent < progressFloor && now.Sub(last.at) < progressInterval {
		e.progressMu.Unlock()
		return
	}
	if e.lastProgress == nil {
		e.lastProgress = map[string]progressMark{}
	}
	e.lastProgress[jobID] = progressMark{percent: percent, at: now}
	e.progressMu.Unlock()

	err := e.store.SetJobProgress(jobID, nodestore.JobProgress{
		Percent:    percent,
		BytesDone:  progress.BytesDone,
		TotalBytes: progress.TotalBytes,
		FilesDone:  progress.FilesDone,
		TotalFiles: progress.TotalFiles,
		Repository: repositoryID,
		At:         now.UTC(),
	})
	if err != nil {
		e.log.Debug("record backup progress", "job_id", jobID, "error", err)
	}
}

// forgetProgress drops what was remembered about a job that has finished,
// so the map does not grow for the life of the process.
func (e *Engine) forgetProgress(jobID string) {
	e.progressMu.Lock()
	delete(e.lastProgress, jobID)
	e.progressMu.Unlock()
}

// progressMark is the last progress written for one job.
type progressMark struct {
	percent float64
	at      time.Time
}
