package nodestore

import (
	"time"

	"github.com/shuki/cprest/internal/job"
)

// Destination is a storage endpoint. Config holds non-secret settings;
// credentials live sealed under CredentialsSecretID.
//
// Field names match internal/store.Destination on purpose.
type Destination struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Type                string            `json:"type"`
	Config              map[string]string `json:"config"`
	CredentialsSecretID string            `json:"credentials_secret_id,omitempty"`
	AppendOnly          bool              `json:"append_only"`
	CreatedAt           time.Time         `json:"created_at"`
	LastCheckedAt       *time.Time        `json:"last_checked_at,omitempty"`
	LastCheckError      string            `json:"last_check_error,omitempty"`
}

// Repository is a restic repository inside a destination.
type Repository struct {
	ID               string `json:"id"`
	DestinationID    string `json:"destination_id"`
	Path             string `json:"path"`
	PasswordSecretID string `json:"password_secret_id"`
	// ChunkerSourceRepoID names the repository whose chunker parameters
	// this one must copy. Those parameters are fixed at creation and can
	// never change, so every repository after the first must have one.
	// See docs/DESIGN.md §7.
	ChunkerSourceRepoID string     `json:"chunker_source_repo_id,omitempty"`
	InitialisedAt       *time.Time `json:"initialised_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// Retention is the keep policy handed to "restic forget".
type Retention struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
	KeepYearly  int `json:"keep_yearly,omitempty"`
}

// Policy is a schedule with its retention and payload settings.
type Policy struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	ScheduleCron   string    `json:"schedule_cron"`
	PayloadMode    string    `json:"payload_mode"`
	Compression    string    `json:"compression"`
	LimitUploadKiB int       `json:"limit_upload_kib"`
	Retention      Retention `json:"retention"`
	// RepositoryIDs are the copies this policy keeps. One unreachable
	// destination does not invalidate the others.
	RepositoryIDs []string `json:"repository_ids"`
	// Accounts are the cPanel users on this schedule. Empty means every
	// account the server has, resolved at run time so new accounts are
	// picked up without an edit.
	Accounts  []string   `json:"accounts"`
	Enabled   bool       `json:"enabled"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// AllAccounts reports whether the policy covers every account on the server.
func (p Policy) AllAccounts() bool { return len(p.Accounts) == 0 }

// Job is one backup run of one account.
type Job struct {
	ID         string      `json:"id"`
	PolicyID   string      `json:"policy_id"`
	Account    string      `json:"account"`
	Status     job.Status  `json:"status"`
	Targets    []JobTarget `json:"targets"`
	StagingErr string      `json:"staging_error,omitempty"`
	QueuedAt   time.Time   `json:"queued_at"`
	StartedAt  *time.Time  `json:"started_at,omitempty"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
}

// JobTarget is one repository's outcome within a job.
type JobTarget struct {
	RepositoryID   string           `json:"repository_id"`
	Status         job.TargetStatus `json:"status"`
	SnapshotID     string           `json:"snapshot_id,omitempty"`
	BytesAdded     uint64           `json:"bytes_added"`
	BytesProcessed uint64           `json:"bytes_processed"`
	DurationSecs   float64          `json:"duration_seconds"`
	Incomplete     bool             `json:"incomplete"`
	Error          string           `json:"error,omitempty"`
	// Detail is restic's own account of the run: the files it could not
	// read, and any warnings. Kept so "some files unreadable" can be
	// looked into rather than merely noticed.
	Detail string `json:"detail,omitempty"`
}

// Restore is one restore run.
type Restore struct {
	ID           string   `json:"id"`
	Account      string   `json:"account"`
	RepositoryID string   `json:"repository_id"`
	SnapshotID   string   `json:"snapshot_id"`
	Kind         string   `json:"kind"`
	IncludePaths []string `json:"include_paths,omitempty"`
	TargetDir    string   `json:"target_dir,omitempty"`
	// Apply hands the rebuilt archive to restorepkg, overwriting the live
	// account. Off unless explicitly asked for.
	Apply         bool       `json:"apply"`
	Status        job.Status `json:"status"`
	BytesRestored uint64     `json:"bytes_restored"`
	ArchivePath   string     `json:"archive_path,omitempty"`
	RestoredTo    string     `json:"restored_to,omitempty"`
	Applied       bool       `json:"applied"`
	Error         string     `json:"error,omitempty"`
	QueuedAt      time.Time  `json:"queued_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// Secret is a sealed credential. The plaintext never reaches this file.
type Secret struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Ciphertext []byte    `json:"ciphertext"`
	KeyID      string    `json:"key_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// Settings are the node's own configuration.
type Settings struct {
	// StagingRoot is where pkgacct writes. Snapshot paths embed it and
	// restic groups retention by path, so changing it after the first
	// backup orphans every existing retention group. Treat it as fixed.
	StagingRoot   string  `json:"staging_root"`
	MaxConcurrent int     `json:"max_concurrent"`
	SafetyMargin  float64 `json:"safety_margin"`
	ResticBinary  string  `json:"restic_binary"`
	ResticCache   string  `json:"restic_cache"`
	ResticCACert  string  `json:"restic_cacert,omitempty"`
	// ConfigDir holds the SSH keys and known_hosts files cprest generates
	// for SFTP destinations.
	ConfigDir string `json:"config_dir"`
	Hostname  string `json:"hostname"`
	// PkgacctFlags records what the installed pkgacct actually supports,
	// probed rather than assumed.
	PkgacctFlags map[string]string `json:"pkgacct_flags,omitempty"`
}
