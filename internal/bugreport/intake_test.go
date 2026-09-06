package bugreport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
)

type intakeTransport func(*http.Request) (*http.Response, error)

func (f intakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const testIntakeKey = "test-intake-key-not-a-real-secret"
const createdReceipt = `{"ok":true,"data":{"action":"created","program":"cprestic","issue_id":"test-issue","identifier":"CPRESTIC-42","url":"http://192.168.100.100:8090/bug-reports/browse/CPRESTIC-42/","warnings":["one label was skipped"]}}`

func intakeResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestSafeReportRemainsTheSameDuringPreviewAndSubmission(t *testing.T) {
	report := Report{Subject: "Failure", Body: "password=redact-me", Sections: []Section{
		{Title: "Single long line", Text: strings.Repeat("א", MaxSectionBytes)},
		{Title: "Long log", Text: strings.Repeat("an earlier log line\n", MaxSectionBytes)},
	}}
	safe := report.Safe()
	if !reflect.DeepEqual(safe, safe.Safe()) {
		t.Fatal("preparing a reviewed report changed its content")
	}
	for _, section := range safe.Sections {
		if len(section.Text) > MaxSectionBytes || !utf8.ValidString(section.Text) {
			t.Fatal("diagnostic section exceeds its cap or splits a UTF-8 character")
		}
	}
}

func TestIntakePayloadRoutesExplicitlyAndRedactsBeforeSending(t *testing.T) {
	report := Report{Subject: "Restore failed password=hidden", Body: "Expected success. token=description-secret " + testIntakeKey,
		Sections: []Section{{Title: "Service log", Text: "password=log-secret\n" + testIntakeKey}, {Title: "Versions", Text: "cpanel 136\nrestic 0.19"}}}
	calls := 0
	client := &http.Client{Transport: intakeTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != IntakeURL || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+testIntakeKey || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("missing authorization or JSON content type")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("request is unbounded")
		}
		body, _ := io.ReadAll(r.Body)
		for _, secret := range []string{"hidden", "description-secret", "log-secret", testIntakeKey} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("secret survived: %s", secret)
			}
		}
		var payload intakeRequest
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Program != "cprestic" || payload.Source != "cprestic-whm" || payload.Severity != "medium" || len(payload.Logs) != 2 {
			t.Fatalf("unexpected payload: %+v", payload)
		}
		if payload.Logs["02 Versions"] != report.Sections[1].Text {
			t.Fatal("diagnostics lost")
		}
		return intakeResponse(http.StatusCreated, createdReceipt), nil
	})}
	receipt, err := Submit(t.Context(), client, testIntakeKey, report)
	if err != nil || receipt.Identifier != "CPRESTIC-42" || receipt.URL == "" || len(receipt.Warnings) != 1 || calls != 1 {
		t.Fatalf("receipt=%+v err=%v calls=%d", receipt, err, calls)
	}
}

func TestIntakeNeverTreatsErrorsOrRedirectsAsDelivery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		body, want string
	}{
		{"credentials", 401, `{"ok":false,"error":"` + testIntakeKey + `"}`, "key was rejected"},
		{"rate limit", 429, `{"ok":false}`, "wait 60 seconds"},
		{"program", 400, `{"ok":false}`, "verify the cprestic program"},
		{"too large", 413, `{"ok":false}`, "download it"},
		{"upstream", 502, "private proxy body " + testIntakeKey, "not confirmed"},
		{"redirect", 307, "", "not confirmed"},
		{"HTML success", 200, "<html>login</html>", "not confirmed"},
		{"false success", 200, `{"ok":false}`, "not confirmed"},
		{"wrong project", 201, strings.ReplaceAll(createdReceipt, "cprestic", "jabali-panel"), "unexpected confirmation"},
		{"missing ID", 201, `{"ok":true,"data":{"action":"created","program":"cprestic"}}`, "unexpected confirmation"},
		{"oversize response", 200, strings.Repeat("x", 65537), "confirmation could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: intakeTransport(func(*http.Request) (*http.Response, error) {
				calls++
				resp := intakeResponse(tc.code, tc.body)
				resp.Header.Set("Location", "https://other.example/collect")
				resp.Header.Set("Retry-After", "60")
				resp.Header.Set("X-Request-ID", "request-42")
				return resp, nil
			})}
			_, err := Submit(t.Context(), client, testIntakeKey, Report{Subject: "Failure", Body: "Details"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if strings.Contains(err.Error(), testIntakeKey) || strings.Contains(err.Error(), "private proxy") {
				t.Fatal("upstream content leaked")
			}
		})
	}
}

func TestIntakeCommentedAndUnsafeTrackerLinks(t *testing.T) {
	client := &http.Client{Transport: intakeTransport(func(*http.Request) (*http.Response, error) {
		return intakeResponse(200, `{"ok":true,"data":{"action":"commented","program":"cprestic","issue_id":"id","url":"javascript:alert(1)"}}`), nil
	})}
	got, err := Submit(t.Context(), client, testIntakeKey, Report{Subject: "Failure", Body: "Details"})
	if err != nil || got.Action != "commented" || got.URL != "" {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestIntakeCancellationAndValidationDoNotRetry(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: intakeTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Submit(ctx, client, testIntakeKey, Report{Subject: "Failure", Body: "Details"}); err == nil {
		t.Fatal("cancelled delivery succeeded")
	}
	if calls > 1 {
		t.Fatal("cancelled request retried")
	}
	calls = 0
	for _, report := range []Report{{}, {Subject: "newline\nin title", Body: "details"}, {Subject: "too long", Body: strings.Repeat("x", 20001)}} {
		if _, err := Submit(t.Context(), client, testIntakeKey, report); err == nil {
			t.Fatal("invalid report accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid input reached intake")
	}
	_, err := Submit(t.Context(), client, "short", Report{Subject: "Failure", Body: "Details"})
	if err == nil || calls != 0 {
		t.Fatal("invalid key reached intake")
	}
}

func TestIntakeKeyMustBePrivateRegularFile(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, IntakeKeyFile)
	if _, err := ReadIntakeKey(key); err == nil {
		t.Fatal("missing key accepted")
	}
	if err := os.WriteFile(key, []byte(testIntakeKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadIntakeKey(key); err != nil || got != testIntakeKey {
		t.Fatalf("private key: %q %v", got, err)
	}
	if err := os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIntakeKey(key); err == nil {
		t.Fatal("world-readable key accepted")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIntakeKey(link); err == nil {
		t.Fatal("symlink accepted")
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil && !errors.Is(err, syscall.ENOSYS) {
		t.Fatal(err)
	}
	if _, err := ReadIntakeKey(fifo); err == nil {
		t.Fatal("nonregular key accepted")
	}
}
