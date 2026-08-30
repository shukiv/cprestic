package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/repobuild"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/store"
	"github.com/shuki/cprest/internal/vault"
)

func runSnapshots(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("snapshots", flag.ExitOnError)
	admin := addAdminFlags(flags)
	hostname := flags.String("server", "", "server hostname")
	user := flags.String("user", "", "cPanel account user")
	repositoryID := flags.String("repository", "", "repository to read; empty uses the first provisioned one")
	resticBinary := flags.String("restic", "restic", "path to the restic binary")
	caCert := flags.String("restic-cacert", "", "CA bundle restic should trust")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *hostname == "" || *user == "" {
		return errors.New("-server and -user are required")
	}

	db, v, err := admin.open(ctx, true)
	if err != nil {
		return err
	}
	defer db.Close()

	account, err := db.AccountByUser(ctx, *hostname, *user)
	if err != nil {
		return err
	}
	repo, err := openRepositoryForAdmin(ctx, db, v, account.ID, *repositoryID)
	if err != nil {
		return err
	}

	runner := resticrun.New(resticrun.Config{
		Binary: *resticBinary, RuntimeDir: os.TempDir(), CACertPath: *caCert,
	}, nil)
	snapshots, err := runner.Snapshots(ctx, repo, resticrun.SnapshotFilter{
		Tags: []string{"account:" + *user},
	})
	if err != nil {
		return err
	}
	if len(snapshots) == 0 {
		fmt.Printf("no snapshots for %s on %s\n", *user, *hostname)
		return nil
	}

	fmt.Printf("%-12s  %-20s  %-10s  %s\n", "SNAPSHOT", "TAKEN", "MODE", "SIZE")
	for _, snapshot := range snapshots {
		fmt.Printf("%-12s  %-20s  %-10s  %s\n",
			snapshot.ShortID,
			snapshot.Time.Local().Format("2006-01-02 15:04:05"),
			snapshot.PayloadMode(),
			humanBytes(snapshot.Summary.TotalBytesProcessed))
	}
	return nil
}

func runRestore(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("restore", flag.ExitOnError)
	admin := addAdminFlags(flags)
	hostname := flags.String("server", "", "server hostname")
	user := flags.String("user", "", "cPanel account user")
	snapshot := flags.String("snapshot", "", "snapshot id, from the snapshots command")
	repositoryID := flags.String("repository", "", "repository to read; empty picks a provisioned one")
	files := flags.String("files", "",
		"comma-separated paths to recover instead of the whole account")
	target := flags.String("target", "",
		"where a files restore should leave what it recovers")
	apply := flags.Bool("apply", false,
		"hand the rebuilt archive to cPanel's restorepkg, overwriting the live account")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *hostname == "" || *user == "" || *snapshot == "" {
		return errors.New("-server, -user and -snapshot are required")
	}

	db, _, err := admin.open(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()

	account, err := db.AccountByUser(ctx, *hostname, *user)
	if err != nil {
		return err
	}

	request := store.RestoreRequest{
		AccountID:    account.ID,
		RepositoryID: *repositoryID,
		SnapshotID:   *snapshot,
		Kind:         protocol.RestoreAccount,
		TargetDir:    *target,
		Apply:        *apply,
	}
	if trimmed := strings.TrimSpace(*files); trimmed != "" {
		request.Kind = protocol.RestoreFiles
		for _, path := range strings.Split(trimmed, ",") {
			if path = strings.TrimSpace(path); path != "" {
				request.IncludePaths = append(request.IncludePaths, path)
			}
		}
	}
	if request.Apply && request.Kind != protocol.RestoreAccount {
		return errors.New("-apply only makes sense for a whole-account restore")
	}

	jobID, err := db.CreateRestore(ctx, request)
	if err != nil {
		return err
	}

	fmt.Printf("restore %s queued for %s on %s\n", jobID, *user, *hostname)
	if request.Apply {
		fmt.Println("WARNING: this restore will overwrite the live account when the agent picks it up")
	} else {
		fmt.Println("the agent will rebuild the archive and leave it in place; nothing is overwritten")
	}
	fmt.Printf("watch it with:\n  cprest-controller restore-status -job %s\n", jobID)
	return nil
}

func runRestoreStatus(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("restore-status", flag.ExitOnError)
	admin := addAdminFlags(flags)
	jobID := flags.String("job", "", "restore job id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *jobID == "" {
		return errors.New("-job is required")
	}

	db, _, err := admin.open(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()

	restore, err := db.RestoreByID(ctx, *jobID)
	if err != nil {
		return err
	}

	fmt.Printf("restore   %s\n", restore.ID)
	fmt.Printf("status    %s\n", restore.Status)
	fmt.Printf("kind      %s\n", restore.Kind)
	fmt.Printf("snapshot  %s\n", restore.SnapshotID)
	fmt.Printf("queued    %s\n", restore.CreatedAt.Local().Format(time.RFC3339))
	if restore.FinishedAt != nil {
		fmt.Printf("finished  %s\n", restore.FinishedAt.Local().Format(time.RFC3339))
	}
	if restore.BytesRestored > 0 {
		fmt.Printf("restored  %s\n", humanBytes(restore.BytesRestored))
	}
	if restore.ArchivePath != "" {
		fmt.Printf("archive   %s (on the cPanel server)\n", restore.ArchivePath)
	}
	if restore.Error != "" {
		fmt.Printf("error     %s\n", restore.Error)
	}
	return nil
}

// openRepositoryForAdmin resolves a repository for a read-only admin
// command, addressed the way the maintenance runner reaches it.
func openRepositoryForAdmin(ctx context.Context, db *store.Store, v *vault.Vault,
	accountID, repositoryID string) (resticrun.Repository, error) {

	if repositoryID == "" {
		repos, err := db.ProvisionedRepositoriesForAccount(ctx, accountID)
		if err != nil {
			return resticrun.Repository{}, err
		}
		if len(repos) == 0 {
			return resticrun.Repository{}, errors.New("no provisioned repository holds this account")
		}
		repositoryID = repos[0].ID
	}

	sealed, err := db.SealedRepository(ctx, repositoryID)
	if err != nil {
		return resticrun.Repository{}, err
	}
	opened, err := repobuild.OpenForMaintenance(v, repobuild.Sealed{
		DestinationType:    sealed.DestinationType,
		DestinationConfig:  sealed.DestinationConfig,
		CredentialsSealed:  sealed.CredentialsSealed,
		RepoPath:           sealed.RepositoryPath,
		RepoPasswordSealed: sealed.RepoPasswordSealed,
	})
	if err != nil {
		return resticrun.Repository{}, err
	}
	dest, err := destination.Build(opened.Spec)
	if err != nil {
		return resticrun.Repository{}, err
	}
	return resticrun.Repository{Dest: dest, Path: opened.Path, Password: opened.Password}, nil
}

func humanBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exponent := float64(bytes), 0
	for value >= unit && exponent < 4 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGT"[exponent-1])
}
