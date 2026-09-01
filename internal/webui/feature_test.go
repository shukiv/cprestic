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

	"github.com/shuki/cprest/internal/nodestore"
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
