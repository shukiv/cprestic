package webui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/resticrun"
)

// TestDumpAccountRecoveryPages is the account-side counterpart to
// TestDumpPages. It is off during normal tests; CPREST_USER_DUMP turns the
// real server-rendered fragments into local files for browser inspection.
func TestDumpAccountRecoveryPages(t *testing.T) {
	dir := os.Getenv("CPREST_USER_DUMP")
	if dir == "" {
		t.Skip("set CPREST_USER_DUMP to a directory to dump account pages")
	}
	server, err := New(nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-2 * time.Hour)
	points := []resticrun.Snapshot{
		{ID: "d4c3b2a1", Time: now},
		{ID: "11223344", Time: now.Add(-24 * time.Hour)},
		{ID: "55667788", Time: now.Add(-7 * 24 * time.Hour)},
	}
	views := map[string]userView{
		"overview": {
			Account: "arkady", TotalPoints: 34, Latest: now, Ready: 1,
			Repositories: []userRepository{{
				ID: "backup-server", Name: "BackupServer", Snapshots: 34, Latest: now,
			}},
		},
		"restore": {
			Account: "arkady", Kinds: userKinds, Repository: "backup-server",
			Snapshot: points[0].ID, SnapshotAt: now, Snapshots: points,
		},
		"databases": {
			Account: "arkady", Kinds: userKinds, Repository: "backup-server",
			Snapshot: points[0].ID, SnapshotAt: now, Snapshots: points,
			Kind: granular.KindDatabase, Path: "/backup/arkady/databases",
			Entries: []browseEntry{
				{Name: "arkady_wp.sql", Path: "/backup/arkady/databases/arkady_wp.sql", Item: "arkady_wp", Size: 675_000},
				{Name: "arkady_shop.sql", Path: "/backup/arkady/databases/arkady_shop.sql", Item: "arkady_shop", Size: 18_400_000},
			},
		},
	}
	for name, view := range views {
		templateName := "user_browse.html"
		path := "/browse"
		if name == "overview" {
			templateName = "user_home.html"
			path = "/"
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "arkady"))
		response := httptest.NewRecorder()
		server.renderUser(response, request, templateName, view)
		if response.Code != http.StatusOK {
			t.Fatalf("render %s = %d: %s", name, response.Code, response.Body.String())
		}
		if err := os.WriteFile(filepath.Join(dir, "account-"+name+".html"), response.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
