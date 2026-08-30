package store_test

import (
	"context"
	"testing"

	"github.com/shuki/cprest/internal/store"
	"github.com/shuki/cprest/internal/testsupport"
)

func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	dsn := testsupport.PostgresDSN(t)
	ctx := context.Background()

	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// The migrations are multi-statement scripts with their own
	// BEGIN/COMMIT, which pgx's default extended protocol rejects.
	ran, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(ran) == 0 {
		t.Fatal("Migrate applied nothing on an empty database")
	}

	again, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Migrate re-applied %v", again)
	}

	var tables int
	err = db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name IN
		       ('servers','accounts','destinations','repositories','policies',
		        'backup_jobs','backup_job_targets','maintenance_runs','secrets')`).Scan(&tables)
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 9 {
		t.Errorf("found %d of 9 expected tables", tables)
	}
}
