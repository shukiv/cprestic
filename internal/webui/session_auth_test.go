package webui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testSessionName   = "studio:Abcdef_123456789"
	testSessionKey    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testSecurityToken = "/cpsess1234567890"
)

func TestCPanelSessionIsAnAuthorizationBoundary(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	base := strings.Join([]string{
		"user=studio",
		"pass=" + testSessionKey,
		"cp_security_token=" + testSecurityToken,
		"origin_as_string=address=192.0.2.4,app=cpaneld,creator=studio,method=handle_form_login,path=form,possessed=0",
		"successful_internal_auth_with_timestamp=1799999900",
		"expires=1800003600",
	}, "\n") + "\n"

	tests := []struct {
		name    string
		body    string
		account string
		key     string
		token   string
		allowed bool
	}{
		{"owner", base, "studio", testSessionKey, testSecurityToken, true},
		{"wrong account", strings.Replace(base, "user=studio", "user=another", 1), "studio", testSessionKey, testSecurityToken, false},
		{"wrong session key", base, "studio", strings.Repeat("a", 64), testSecurityToken, false},
		{"wrong security token", base, "studio", testSessionKey, "/cpsess9999999999", false},
		{"preauthentication record", base + "needs_auth=1\n", "studio", testSessionKey, testSecurityToken, false},
		{"wrong cpsrvd service", strings.Replace(base, "app=cpaneld", "app=webmaild", 1), "studio", testSessionKey, testSecurityToken, false},
		{"expired record", strings.Replace(base, "expires=1800003600", "expires=1799999999", 1), "studio", testSessionKey, testSecurityToken, false},
		{"duplicate security field", base + "user=studio\n", "studio", testSessionKey, testSecurityToken, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := testSessionAuth(t, now, test.body)
			principal, err := auth.verifyCPanelSession(context.Background(), test.account, testSessionName, test.key, test.token)
			if test.allowed && (err != nil || principal != "studio") {
				t.Fatalf("valid owner session rejected: principal=%q err=%v", principal, err)
			}
			if !test.allowed && err == nil {
				t.Fatalf("unsafe session accepted as %q", principal)
			}
		})
	}
}

func TestRootPossessedCPanelAccountSessionIsAccepted(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	base := strings.Join([]string{
		"user=studio",
		"pass=" + testSessionKey,
		"cp_security_token=" + testSecurityToken,
		"origin_as_string=address=192.0.2.4,app=cpaneld,creator=root,method=handle_form_login,path=form,possessed=1",
		"hulk_registered=1",
		"possessor=root",
		"session_needs_temp_user=1",
		"session_temp_user=cpuser00000000000",
		"session_temp_pass=0123456789abcdef0123456789abcdef",
	}, "\n") + "\n"

	for _, test := range []struct {
		name    string
		body    string
		allowed bool
	}{
		{"root login as account", base, true},
		{"root login after temporary user creation", strings.Replace(base, "session_needs_temp_user=1", "created_session_temp_user=1", 1), true},
		{"non-root possessor", strings.Replace(base, "possessor=root", "possessor=reseller", 1), false},
		{"no temporary user", strings.Replace(base, "session_temp_user=cpuser00000000000\n", "", 1), false},
		{"no temporary password", strings.Replace(base, "session_temp_pass=0123456789abcdef0123456789abcdef\n", "", 1), false},
		{"not registered by cPHulk", strings.Replace(base, "hulk_registered=1", "hulk_registered=0", 1), false},
		{"wrong login method", strings.Replace(base, "method=handle_form_login", "method=unknown", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := testSessionAuth(t, now, test.body)
			principal, err := auth.verifyCPanelSession(context.Background(), "studio", testSessionName,
				testSessionKey, testSecurityToken)
			if test.allowed && (err != nil || principal != "studio") {
				t.Fatalf("valid root-possessed session rejected: principal=%q err=%v", principal, err)
			}
			if !test.allowed && err == nil {
				t.Fatalf("unsafe root-possession claim accepted as %q", principal)
			}
		})
	}
}

func TestAdminBinProofBindsTheSessionDerivedPrincipal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	owner := strings.Join([]string{
		"user=studio",
		"pass=" + testSessionKey,
		"cp_security_token=" + testSecurityToken,
		"origin_as_string=address=192.0.2.4,app=cpaneld,creator=studio,method=handle_form_login,path=form,possessed=0",
		"successful_internal_auth_with_timestamp=1799999900",
	}, "\n") + "\n"

	auth := testSessionAuth(t, now, owner)
	token, err := auth.issueForPrincipal(context.Background(), "studio", "studio", testSessionName,
		testSessionKey, testSecurityToken, http.MethodGet, "/")
	if err != nil {
		t.Fatalf("owner AdminBin proof rejected: %v", err)
	}
	if err := auth.consume("studio", token, http.MethodGet, "/"); err != nil {
		t.Fatalf("owner capability rejected: %v", err)
	}
	if _, err := auth.issueForPrincipal(context.Background(), "studio", "intruder@example.test", testSessionName,
		testSessionKey, testSecurityToken, http.MethodGet, "/"); err == nil {
		t.Fatal("a principal that did not match the raw cPanel session was accepted")
	}

	team := strings.Replace(owner, "user=studio", "user=developer@example.test", 1)
	teamAuth := testSessionAuth(t, now, team)
	teamAuth.teamAdmin = func(_ context.Context, account, principal string) (bool, error) {
		return account == "studio" && principal == "developer@example.test", nil
	}
	if _, err := teamAuth.issueForPrincipal(context.Background(), "studio", "developer@example.test", testSessionName,
		testSessionKey, testSecurityToken, http.MethodGet, "/"); err != nil {
		t.Fatalf("Administrator Team session rejected: %v", err)
	}
}

func TestRootOnlyAdminBinEndpointIssuesARequestBoundCapability(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	body := strings.Join([]string{
		"user=studio",
		"cp_security_token=" + testSecurityToken,
		"origin_as_string=address=192.0.2.4,app=cpaneld,creator=studio,method=handle_form_login,path=form,possessed=0",
		"successful_internal_auth_with_timestamp=1799999900",
	}, "\n") + "\n"
	auth := testSessionAuth(t, now, body)
	server := &Server{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		userAuth: auth,
	}
	request := httptest.NewRequest(http.MethodPost, adminCapabilityEndpoint, nil)
	request.Header.Set(cpanelAccountHeader, "studio")
	request.Header.Set(cpanelPrincipalHeader, "studio")
	request.Header.Set(cpanelSessionHeader, testSessionName)
	request.Header.Set(cpanelTokenHeader, testSecurityToken)
	request.Header.Set(capabilityMethodHeader, http.MethodGet)
	request.Header.Set(capabilityTargetHeader, "/browse?repository=one")
	response := httptest.NewRecorder()

	server.issueAdminUserCapability(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("AdminBin endpoint status=%d body=%q", response.Code, response.Body.String())
	}
	if err := auth.consume("studio", strings.TrimSpace(response.Body.String()), http.MethodGet,
		"/browse?repository=one"); err != nil {
		t.Fatalf("issued capability was not usable for its exact request: %v", err)
	}
}

func TestOnlyAdministratorTeamSessionsReceiveCapabilities(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	body := strings.Join([]string{
		"user=studio",
		"pass=" + testSessionKey,
		"team_user=developer@example.test",
		"cp_security_token=" + testSecurityToken,
		"origin_as_string=address=192.0.2.4,app=cpaneld,creator=studio,method=handle_form_login,path=form,possessed=0",
		"successful_internal_auth_with_timestamp=1799999900",
	}, "\n") + "\n"

	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "administrator", false: "web role"}[allowed], func(t *testing.T) {
			auth := testSessionAuth(t, now, body)
			called := false
			auth.teamAdmin = func(_ context.Context, account, principal string) (bool, error) {
				called = true
				if account != "studio" || principal != "developer@example.test" {
					t.Fatalf("role query was for %q, %q", account, principal)
				}
				return allowed, nil
			}
			principal, err := auth.verifyCPanelSession(context.Background(), "studio", testSessionName, testSessionKey, testSecurityToken)
			if !called {
				t.Fatal("team principal was not checked against its current role")
			}
			if allowed && (err != nil || principal != "developer@example.test") {
				t.Fatalf("administrator rejected: principal=%q err=%v", principal, err)
			}
			if !allowed && err == nil {
				t.Fatal("non-administrator team user was accepted")
			}
		})
	}
}

func TestCapabilityIsShortLivedRequestBoundAndSingleUse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	body := strings.Join([]string{
		"user=studio",
		"pass=" + testSessionKey,
		"cp_security_token=" + testSecurityToken,
		"origin_as_string=address=192.0.2.4,app=cpaneld,creator=studio,method=handle_form_login,path=form,possessed=0",
		"successful_internal_auth_with_timestamp=1799999900",
	}, "\n") + "\n"
	auth := testSessionAuth(t, now, body)
	token, err := auth.issue(context.Background(), "studio", testSessionName, testSessionKey, testSecurityToken,
		http.MethodGet, "/browse?repository=one")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := auth.consume("studio", token, http.MethodGet, "/browse?repository=two"); err == nil {
		t.Fatal("capability was not bound to its query string")
	}
	if err := auth.consume("another", token, http.MethodGet, "/browse?repository=one"); err == nil {
		t.Fatal("capability was not bound to the Unix peer account")
	}
	if err := auth.consume("studio", token, http.MethodGet, "/browse?repository=one"); err != nil {
		t.Fatalf("first exact use rejected: %v", err)
	}
	if err := auth.consume("studio", token, http.MethodGet, "/browse?repository=one"); err == nil {
		t.Fatal("capability could be replayed")
	}

	expiring := testSessionAuth(t, now, body)
	token, err = expiring.issue(context.Background(), "studio", testSessionName, testSessionKey, testSecurityToken,
		http.MethodPost, "/restore")
	if err != nil {
		t.Fatal(err)
	}
	expiring.now = func() time.Time { return now.Add(capabilityLifetime + time.Second) }
	if err := expiring.consume("studio", token, http.MethodPost, "/restore"); err == nil {
		t.Fatal("expired capability was accepted")
	}
}

func TestCapabilityMiddlewareRejectsDirectSocketRequests(t *testing.T) {
	auth := newAccountSessionAuth([]byte("01234567890123456789012345678901"))
	server := &Server{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		userAuth: auth,
	}
	called := false
	handler := server.requireUserCapability(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("direct request status=%d called=%v", response.Code, called)
	}
}

func TestTeamRoleResponsesFailClosed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	tests := []struct {
		name    string
		json    string
		allowed bool
		wantErr bool
	}{
		{"administrator array", `{"result":{"status":1,"data":[{"user":"developer","roles":["admin","web"]}]}}`, true, false},
		{"administrator hash", `{"result":{"status":1,"data":{"developer":{"roles":"administrator,web"}}}}`, true, false},
		{"web only", `{"result":{"status":1,"data":[{"user":"developer","roles":["web"]}]}}`, false, false},
		{"suspended administrator", `{"result":{"status":1,"data":[{"user":"developer","roles":["admin"],"suspended":1}]}}`, false, false},
		{"expired administrator", `{"result":{"status":1,"data":[{"user":"developer","roles":["admin"],"expire_date":1799999999}]}}`, false, false},
		{"different user", `{"result":{"status":1,"data":[{"user":"someone","roles":["admin"]}]}}`, false, false},
		{"failed query", `{"result":{"status":0,"data":null}}`, false, true},
		{"malformed", `not json`, false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := parseTeamAdministrator([]byte(test.json), "developer@example.test", now)
			if allowed != test.allowed || (err != nil) != test.wantErr {
				t.Fatalf("allowed=%v err=%v; want allowed=%v error=%v", allowed, err, test.allowed, test.wantErr)
			}
		})
	}
}

func testSessionAuth(t *testing.T, now time.Time, body string) *accountSessionAuth {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, testSessionName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := newAccountSessionAuth([]byte("01234567890123456789012345678901"))
	auth.sessionsDir = dir
	auth.rootUID = uint32(os.Getuid())
	auth.now = func() time.Time { return now }
	auth.teamAdmin = func(context.Context, string, string) (bool, error) { return false, nil }
	return auth
}
