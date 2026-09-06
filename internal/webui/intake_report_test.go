package webui_test

import (
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shuki/cprest/internal/bugreport"
	"github.com/shuki/cprest/internal/job"
	"github.com/shuki/cprest/internal/nodestore"
)

type reportTransport func(*http.Request) (*http.Response, error)

func (f reportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reportHidden(t *testing.T, page, name string) string {
	t.Helper()
	match := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatalf("report has no %s field", name)
	}
	return html.UnescapeString(match[1])
}

func postReport(t *testing.T, client *http.Client, fields map[string][]string) string {
	t.Helper()
	resp, err := client.PostForm("http://ui/report/send", fields)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST report: %d %v", resp.StatusCode, err)
	}
	return string(body)
}

func TestIntakeFormSendsOnlyTheReviewedReport(t *testing.T) {
	const key = "fixture-intake-credential-123456"
	var calls atomic.Int64
	requests := make(chan string, 1)
	intake := &http.Client{Transport: reportTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		requests <- string(body)
		if r.URL.String() != bugreport.IntakeURL || r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("incorrect intake request")
		}
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
			`{"ok":true,"data":{"action":"created","program":"cprestic","issue_id":"fixture-id","identifier":"CPRESTIC-42","url":"http://tracker.example/CPRESTIC-42/","warnings":["example warning"]}}`))}, nil
	})}
	client, _, engine := newUIWithIntake(t, nil, intake)
	if err := os.WriteFile(engine.BugIntakeKeyPath(), []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	_, form := get(t, client, "/report")
	if !strings.Contains(form, "cprestic") || regexp.MustCompile(`<button\b[^>]*\bname="send"`).MatchString(form) {
		t.Fatal("initial form bypasses preview or omits intake program")
	}
	fields := map[string][]string{"csrf": {csrfToken(t, form)}, "subject": {"Restore failed"}, "body": {"A restore did not finish. password=remove-me"}}
	fields["send"] = []string{"1"}
	direct := postReport(t, client, fields)
	if !strings.Contains(direct, "Preview this report again") || calls.Load() != 0 {
		t.Fatal("unreviewed report was sent")
	}
	delete(fields, "send")
	preview := postReport(t, client, fields)
	if calls.Load() != 0 || !strings.Contains(preview, "Send to intake") {
		t.Fatal("preview sent data or failed to offer Send")
	}
	fields["prepared"] = []string{reportHidden(t, preview, "prepared")}
	fields["signature"] = []string{reportHidden(t, preview, "signature")}
	if strings.Contains(fields["prepared"][0], "remove-me") {
		t.Fatal("signed diagnostics contain an unredacted password")
	}
	fields["send"] = []string{"1"}
	// New failures after preview are not consented-to attachments.
	if _, err := engine.Store().PutJob(nodestore.Job{Account: "new-customer", Status: job.StatusFailed, StagingErr: "after-preview-private-detail"}); err != nil {
		t.Fatal(err)
	}
	delete(fields, "send")
	fields["download"] = []string{"1"}
	download := postReport(t, client, fields)
	if !strings.HasPrefix(download, "# Restore failed") || strings.Contains(download, "after-preview-private-detail") || calls.Load() != 0 {
		t.Fatal("download did not preserve the reviewed diagnostic snapshot")
	}
	delete(fields, "download")
	fields["send"] = []string{"1"}
	goodSignature := fields["signature"][0]
	fields["signature"] = []string{"tampered"}
	if page := postReport(t, client, fields); !strings.Contains(page, "Preview this report again") || calls.Load() != 0 {
		t.Fatal("tampered preview was sent")
	}
	fields["signature"] = []string{goodSignature}
	sent := postReport(t, client, fields)
	if calls.Load() != 1 {
		t.Fatalf("sent %d requests", calls.Load())
	}
	for _, want := range []string{"Sent to cprestic", "CPRESTIC-42", "Open in Plane", "example warning"} {
		if !strings.Contains(sent, want) {
			t.Errorf("confirmation missing %q", want)
		}
	}
	if strings.Contains(sent, key) || strings.Contains(<-requests, "after-preview-private-detail") {
		t.Fatal("unreviewed content or key escaped")
	}
	_, settings := get(t, client, "/settings")
	if strings.Contains(settings, key) || strings.Contains(settings, `name="bug_email"`) || !strings.Contains(settings, bugreport.IntakeURL) {
		t.Fatal("settings leak a key or offer the old email destination")
	}
}

func TestIntakeFailureKeepsReportAndDownloadAvailable(t *testing.T) {
	var calls atomic.Int64
	client, _, engine := newUIWithIntake(t, nil, &http.Client{Transport: reportTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"ok":false}`))}, nil
	})})
	if err := os.WriteFile(engine.BugIntakeKeyPath(), []byte("fixture-intake-key-123456"), 0600); err != nil {
		t.Fatal(err)
	}
	_, form := get(t, client, "/report")
	fields := map[string][]string{"csrf": {csrfToken(t, form)}, "subject": {"Restore failed"}, "body": {"Keep this description"}}
	preview := postReport(t, client, fields)
	fields["prepared"], fields["signature"] = []string{reportHidden(t, preview, "prepared")}, []string{reportHidden(t, preview, "signature")}
	fields["send"] = []string{"1"}
	page := postReport(t, client, fields)
	for _, want := range []string{"wait 60 seconds", "Keep this description", "Download it", "Send to intake", `role="alert"`} {
		if !strings.Contains(page, want) {
			t.Errorf("failed form missing %q", want)
		}
	}
	if strings.Contains(page, "Sent to cprestic") || calls.Load() != 1 {
		t.Fatal("failure was reported as success or retried")
	}
}
