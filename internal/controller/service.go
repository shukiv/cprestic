// Package controller is the control plane: it turns database state into
// job assignments for agents and records what comes back.
//
// Backup data never passes through here. The controller only schedules,
// hands out credentials for the destinations an agent must reach, and
// stores results. See docs/DESIGN.md §2.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/repobuild"
	"github.com/shuki/cprest/internal/store"
	"github.com/shuki/cprest/internal/vault"
)

// Service holds the controller's dependencies.
type Service struct {
	store *store.Store
	vault *vault.Vault
	log   *slog.Logger

	// LeaseDuration is how long a claimed job stays leased before another
	// agent may take it. It must exceed the longest plausible backup.
	LeaseDuration time.Duration
	// PollInterval is what agents are told to wait between polls.
	PollInterval time.Duration
}

// New builds a Service.
func New(db *store.Store, v *vault.Vault, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:         db,
		vault:         v,
		log:           log,
		LeaseDuration: 6 * time.Hour,
		PollInterval:  protocol.DefaultPoll,
	}
}

// Enrol records what an agent reports about its host.
func (s *Service) Enrol(ctx context.Context, serverID string, req protocol.EnrolRequest) (protocol.EnrolResponse, error) {
	if err := s.store.RecordEnrolment(ctx, serverID, req.PkgacctFlags, req.StagingRoot); err != nil {
		return protocol.EnrolResponse{}, err
	}
	s.log.Info("agent enrolled",
		"server_id", serverID, "hostname", req.Hostname,
		"agent_version", req.AgentVersion, "restic_version", req.ResticVer,
		"pkgacct_flags", req.PkgacctFlags)
	return protocol.EnrolResponse{ServerID: serverID, PollInterval: s.PollInterval}, nil
}

// NextJob leases a job for a server and builds the assignment, decrypting
// only the credentials that job actually needs.
func (s *Service) NextJob(ctx context.Context, serverID string) (protocol.JobAssignment, error) {
	claimed, err := s.store.ClaimNextJob(ctx, serverID, s.LeaseDuration)
	if err != nil {
		return protocol.JobAssignment{}, err
	}

	assignment := protocol.JobAssignment{
		JobID:          claimed.JobID,
		AccountID:      claimed.Account.ID,
		CPanelUser:     claimed.Account.CPanelUser,
		SizeEstimate:   uint64(claimed.Account.SizeEstimate),
		PayloadMode:    claimed.Policy.PayloadMode,
		Compression:    claimed.Policy.Compression,
		LimitUploadKiB: claimed.Policy.LimitUploadKiB,
		LeaseExpiresAt: claimed.LeaseExpiresAt,
	}

	for _, target := range claimed.Targets {
		built, err := s.buildTarget(target)
		if err != nil {
			// Failing the whole assignment is deliberate: handing an agent
			// a partial target list would silently reduce the number of
			// copies a policy promises.
			return protocol.JobAssignment{}, fmt.Errorf(
				"controller: build target %s: %w", target.RepositoryID, err)
		}
		assignment.Targets = append(assignment.Targets, built)
	}
	return assignment, nil
}

func (s *Service) buildTarget(target store.ClaimedTarget) (protocol.Target, error) {
	opened, err := repobuild.Open(s.vault, repobuild.Sealed{
		DestinationType:    target.DestinationType,
		DestinationConfig:  target.DestinationConfig,
		CredentialsSealed:  target.CredentialsSealed,
		RepoPath:           target.RepositoryPath,
		RepoPasswordSealed: target.RepoPasswordSealed,
	})
	if err != nil {
		return protocol.Target{}, err
	}
	return protocol.Target{
		RepositoryID: target.RepositoryID,
		Spec:         opened.Spec,
		RepoPath:     opened.Path,
		RepoPassword: opened.Password,
	}, nil
}

// Report records the outcome of a job.
//
// The rolled-up status is computed from stored rows, not taken from the
// agent: an agent must not be able to declare its own job successful.
func (s *Service) Report(ctx context.Context, serverID string, report protocol.JobReport) (job.Status, error) {
	if report.JobID == "" {
		return "", errors.New("controller: report is missing a job id")
	}

	targets := make([]store.TargetReport, 0, len(report.Targets))
	for _, target := range report.Targets {
		status := job.TargetStatus(target.Status)
		switch status {
		case job.TargetSuccess, job.TargetFailed, job.TargetSkipped:
		default:
			return "", fmt.Errorf("controller: target %s has invalid status %q",
				target.RepositoryID, target.Status)
		}
		targets = append(targets, store.TargetReport{
			RepositoryID:   target.RepositoryID,
			Status:         status,
			SnapshotID:     target.SnapshotID,
			BytesAdded:     target.BytesAdded,
			BytesProcessed: target.BytesProcessed,
			DurationSecs:   target.DurationSecs,
			Incomplete:     target.Incomplete,
			Error:          target.Error,
		})
	}

	status, err := s.store.ApplyReport(ctx, report.JobID, targets, report.StagingError)
	if err != nil {
		return "", err
	}

	level := slog.LevelInfo
	switch status {
	case job.StatusFailed:
		level = slog.LevelError
	case job.StatusPartialSuccess:
		// Two good copies out of three is a warning, not an incident.
		level = slog.LevelWarn
	}
	s.log.Log(ctx, level, "job reported",
		"server_id", serverID, "job_id", report.JobID, "status", status,
		"targets", len(report.Targets), "staging_error", report.StagingError)
	return status, nil
}

// ReclaimLeases returns jobs whose agent stopped reporting to the queue.
func (s *Service) ReclaimLeases(ctx context.Context) (int, error) {
	count, err := s.store.ReclaimExpiredLeases(ctx)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		s.log.Warn("reclaimed expired job leases", "count", count)
	}
	return count, nil
}
