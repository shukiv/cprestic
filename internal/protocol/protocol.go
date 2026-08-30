// Package protocol defines the wire types the agent and controller
// exchange. The agent always initiates: cPanel servers sit behind NAT and
// provider firewalls, so the controller never dials them.
// See docs/DESIGN.md §3.
package protocol

import (
	"time"

	"github.com/shuki/cprest/internal/destination"
)

// Paths on the controller's agent API.
const (
	PathEnrol    = "/v1/enrol"
	PathNextJob  = "/v1/jobs/next"
	PathReport   = "/v1/jobs/report"
	PathHealthz  = "/healthz"
	HeaderAgent  = "X-Cprest-Agent"
	DefaultPoll  = 60 * time.Second
	MaxBodyBytes = 1 << 20
)

// EnrolRequest is sent once at startup so the controller learns what this
// server can do. Flag support differs between cPanel versions and is
// probed, not assumed.
type EnrolRequest struct {
	Hostname     string            `json:"hostname"`
	AgentVersion string            `json:"agent_version"`
	ResticVer    string            `json:"restic_version"`
	PkgacctFlags map[string]string `json:"pkgacct_flags"`
	StagingRoot  string            `json:"staging_root"`
}

// EnrolResponse confirms which server record the certificate maps to.
type EnrolResponse struct {
	ServerID     string        `json:"server_id"`
	PollInterval time.Duration `json:"poll_interval"`
}

// JobAssignment is one account to back up, with everything needed to do it.
// It is generated per job and carries resolved secrets, so it is never
// logged and never written to the agent's disk.
type JobAssignment struct {
	JobID     string `json:"job_id"`
	AccountID string `json:"account_id"`

	CPanelUser string   `json:"cpanel_user"`
	HomeDir    string   `json:"home_dir"`
	Databases  []string `json:"databases"`
	// SizeEstimate drives the staging space preflight. A job without one
	// is refused rather than allowed to fill the volume.
	SizeEstimate uint64 `json:"size_estimate"`

	PayloadMode    string `json:"payload_mode"`
	Compression    string `json:"compression"`
	LimitUploadKiB int    `json:"limit_upload_kib"`

	Targets        []Target  `json:"targets"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// Target is one repository this job must write to.
type Target struct {
	RepositoryID string            `json:"repository_id"`
	Spec         destination.Spec  `json:"spec"`
	RepoPath     string            `json:"repo_path"`
	RepoPassword string            `json:"repo_password"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// TargetReport is the outcome for one repository.
type TargetReport struct {
	RepositoryID   string  `json:"repository_id"`
	Status         string  `json:"status"`
	SnapshotID     string  `json:"snapshot_id,omitempty"`
	BytesAdded     uint64  `json:"bytes_added"`
	BytesProcessed uint64  `json:"bytes_processed"`
	DurationSecs   float64 `json:"duration_seconds"`
	Attempt        int     `json:"attempt"`
	Incomplete     bool    `json:"incomplete"`
	Error          string  `json:"error,omitempty"`
}

// JobReport closes out a job. The controller derives the job's status from
// the target reports rather than trusting an agent-supplied rollup.
type JobReport struct {
	JobID   string         `json:"job_id"`
	Targets []TargetReport `json:"targets"`
	// StagingError describes a failure before any target was attempted,
	// such as insufficient disk for pkgacct.
	StagingError string `json:"staging_error,omitempty"`
}

// ErrorResponse is the body of any non-2xx reply.
type ErrorResponse struct {
	Error string `json:"error"`
}
