package webui

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/granular"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/resticrun"
)

func TestFeatureResponseMustExplicitlyAllowTheAccount(t *testing.T) {
	for name, response := range map[string]string{
		"enabled":  `{"data":{"has_feature":1},"metadata":{"result":1,"reason":"OK"}}`,
		"disabled": `{"data":{"has_feature":0},"metadata":{"result":1,"reason":"OK"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			allowed, err := parseFeatureResponse([]byte(response))
			if err != nil {
				t.Fatalf("parseFeatureResponse: %v", err)
			}
			if allowed != (name == "enabled") {
				t.Fatalf("allowed = %v", allowed)
			}
		})
	}
	for _, response := range []string{
		`not json`,
		`{"data":{"has_feature":1},"metadata":{"result":0,"reason":"bad query"}}`,
		`{"data":{"has_feature":1}}`,
	} {
		if allowed, err := parseFeatureResponse([]byte(response)); err == nil || allowed {
			t.Fatalf("unsafe feature response accepted: %s", response)
		}
	}
}

func TestAccountGuardRejectsTheRootTokenAndOtherAccountsTokens(t *testing.T) {
	server := &Server{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		csrfToken:   "root-whm-secret",
		userCSRFKey: []byte("a separate account-facing derivation key"),
	}
	called := false
	handler := server.userGuard(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	for name, token := range map[string]struct {
		token  string
		status int
	}{
		"root token":           {server.csrfToken, http.StatusForbidden},
		"another account":      {server.userCSRFToken("rtflow"), http.StatusForbidden},
		"this account's token": {server.userCSRFToken("studio"), http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			called = false
			body := url.Values{"csrf": {token.token}}.Encode()
			request := httptest.NewRequest(http.MethodPost, "/restore", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
			response := httptest.NewRecorder()
			handler(response, request)
			if response.Code != token.status || called != (token.status == http.StatusOK) {
				t.Fatalf("status=%d called=%v; want status=%d", response.Code, called, token.status)
			}
		})
	}
}

func TestPublicSocketEnforcesFeatureManagerBehindThePHPFrontend(t *testing.T) {
	for _, test := range []struct {
		name       string
		check      func(context.Context, string) (bool, error)
		wantStatus int
		wantCalled bool
	}{
		{"enabled", func(context.Context, string) (bool, error) { return true, nil }, 200, true},
		{"disabled", func(context.Context, string) (bool, error) { return false, nil }, 403, false},
		{"unverifiable", func(context.Context, string) (bool, error) {
			return false, errors.New("cpanel unavailable")
		}, 503, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), userFeatures: accountFeatureGate{
				decisions: map[string]featureDecision{}, check: test.check, slots: make(chan struct{}, 1),
			}}
			called := false
			handler := server.userPage(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
			response := httptest.NewRecorder()
			handler(response, request)
			if response.Code != test.wantStatus || called != test.wantCalled {
				t.Fatalf("status=%d called=%v; want %d, %v",
					response.Code, called, test.wantStatus, test.wantCalled)
			}
			if test.name == "disabled" && !strings.Contains(response.Body.String(), "not enabled") {
				t.Fatalf("disabled response did not explain Feature Manager: %s", response.Body.String())
			}
		})
	}
}

func TestAccountRestoreHistoryDoesNotExposeRootDiagnostics(t *testing.T) {
	restore := accountSafeRestore(nodestore.Restore{
		Error:       "restic: unable to open sftp:backup-admin@internal.example:/root/archive",
		Detail:      "/var/lib/cprest/staging/restore-studio",
		ArchivePath: "/var/lib/cprest/staging/restore-studio/account.tar",
		RestoredTo:  "/var/lib/cprest/staging/restore-studio/tree",
	})
	if strings.Contains(restore.Error, "internal.example") || restore.Detail != "" ||
		restore.ArchivePath != "" || restore.RestoredTo != "" {
		t.Fatalf("account-visible restore still carries root diagnostics: %+v", restore)
	}
}

func TestAccountRecoveryRequestsStayDownloadOnly(t *testing.T) {
	full, err := userRestoreRequest("studio", "repo", "snapshot", userKindAccount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if full.Kind != protocol.RestoreAccount || full.Apply || full.ItemKind != "" {
		t.Fatalf("full account request crossed the safe-download boundary: %+v", full)
	}

	database, err := userRestoreRequest("studio", "repo", "snapshot",
		granular.KindDatabase, []string{" studio_wp ", ""})
	if err != nil {
		t.Fatal(err)
	}
	if database.Kind != protocol.RestoreItems || database.Apply ||
		database.ItemKind != string(granular.KindDatabase) ||
		len(database.ItemNames) != 1 || database.ItemNames[0] != "studio_wp" {
		t.Fatalf("database request = %+v", database)
	}

	if _, err := userRestoreRequest("studio", "repo", "snapshot",
		granular.KindDatabase, nil); !errors.Is(err, errUserRestoreNeedsNames) {
		t.Fatalf("database without a selection was accepted: %v", err)
	}
	if _, err := userRestoreRequest("studio", "repo", "snapshot",
		granular.KindSettings, nil); err == nil {
		t.Fatal("raw panel settings were exposed to the account interface")
	}
}

func TestAccountRecoveryPagesRenderTheGuidedFlow(t *testing.T) {
	server, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	request := httptest.NewRequest(http.MethodGet, "/browse", nil)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
	response := httptest.NewRecorder()
	server.renderUser(response, request, "user_browse.html", userView{
		Account: "studio", Kinds: userKinds, Repository: "repo", Snapshot: "abcdef",
		SnapshotAt: now, Snapshots: []resticrun.Snapshot{{ID: "abcdef", Time: now}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("render status = %d: %s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	for _, want := range []string{
		"Restore &amp; download", "Restore point", "Full account", "Home directory",
		"Cron jobs", "Databases", "Database users", "Domains", "SSL certificates",
		"Email accounts", "FTP accounts", `aria-label="Use system theme"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("account recovery page is missing %q", want)
		}
	}
}

func TestAccountRedirectUsesTheCPanelLivePHPEntryPoint(t *testing.T) {
	response := httptest.NewRecorder()
	redirectUser(response, "/browse?repository=repo&snapshot=abc&item=database",
		"error", "Choose an item")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("redirect status = %d", response.Code)
	}
	location := response.Header().Get("Location")
	if !strings.HasPrefix(location, "browse.live.php?") || strings.Contains(location, "p=") {
		t.Fatalf("account redirect escaped the .live.php route: %q", location)
	}
	for _, want := range []string{"repository=repo", "snapshot=abc", "item=database", "kind=error"} {
		if !strings.Contains(location, want) {
			t.Errorf("redirect %q is missing %q", location, want)
		}
	}
}
