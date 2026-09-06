package node_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/resticrun"
)

// TestABackupCreatesARepositoryThatWasNeverCreated covers a destination
// that is saved and has nothing behind it.
//
// The repository is created when a destination is added, but only on the
// branch where the login was proved in the same request. A host key
// confirmed in a second step, or a login that came back with a warning,
// left the destination saved and no repository on it -- and nothing tried
// again until the service happened to restart. Every backup in between
// failed with restic's own words:
//
//	Fatal: repository does not exist: unable to open config file
//
// Seen on a live server: a destination added at 05:55, backups failing at
// 06:00, and the repository created the moment the service was restarted.
func TestABackupCreatesARepositoryThatWasNeverCreated(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var commands []string
	engine := newEngineWithExec(t, store, root,
		resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
			mu.Lock()
			commands = append(commands, strings.Join(cmd.Args, " "))
			mu.Unlock()
			for _, arg := range cmd.Args {
				switch arg {
				case "snapshots":
					return resticrun.CommandResult{Stdout: []byte("[]")}, nil
				case "backup":
					return resticrun.CommandResult{Stdout: []byte(
						`{"message_type":"summary","snapshot_id":"aaaaaaaaaaaaaaaa",` +
							`"total_bytes_processed":1024,"data_added":512}`)}, nil
				}
			}
			return resticrun.CommandResult{}, nil
		}))

	// A destination the way one is left when the repository was not
	// created with it.
	_, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "Local", Type: "local",
		Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	if repo.InitialisedAt != nil {
		t.Fatal("the fixture is wrong: adding a destination created the repository on its own")
	}

	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.QueueBackup(policy.ID, "customer1"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the queued backup: %v", err)
	}

	stored, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.InitialisedAt == nil {
		t.Error("a backup ran against a repository that was never created, " +
			"and nothing created it")
	}

	mu.Lock()
	ran := strings.Join(commands, " | ")
	mu.Unlock()
	if !strings.Contains(ran, "init") {
		t.Errorf("restic was never asked to create the repository: %s", ran)
	}

	jobs, err := store.Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	if jobs[0].Status == job.StatusFailed {
		t.Errorf("the backup failed against the repository it should have created: %s",
			jobs[0].StagingErr)
	}
}
