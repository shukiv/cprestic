package bugreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const IntakeURL = "https://bugs.jabali-panel.com/api/v1/intake"

// IntakeProgram is the programme reports are filed in. It is the tracker's
// own key for this project and not a name this program is free to choose,
// so it did not follow the rename to Gniza: the tracker has no Gniza
// programme, and filing into one that does not exist is rejected. Every
// message below names it rather than spelling it out again, so an operator
// is told the name they will actually see in the tracker.
const IntakeProgram = "cprestic"

// IntakeReceipt is confirmation from the tracker, not merely an HTTP success.
type IntakeReceipt struct {
	Action     string   `json:"action"`
	Program    string   `json:"program"`
	IssueID    string   `json:"issue_id"`
	Identifier string   `json:"identifier"`
	URL        string   `json:"url"`
	Warnings   []string `json:"warnings"`
}

type intakeRequest struct {
	Program     string            `json:"program"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Severity    string            `json:"severity"`
	Source      string            `json:"source"`
	Logs        map[string]string `json:"logs,omitempty"`
}

// Safe returns a separate copy suitable for both preview and delivery. User
// text is redacted too; the intake's own redactor is only a second defence.
func (r Report) Safe() Report {
	out := Report{Subject: Redact(r.Subject), Body: Redact(r.Body)}
	for _, section := range r.Sections {
		out.Sections = append(out.Sections, Section{
			Title: Redact(section.Title), Text: Clip(Redact(section.Text), MaxSectionBytes),
		})
	}
	return out
}

func (r Report) Validate() error {
	if strings.TrimSpace(r.Subject) == "" || strings.TrimSpace(r.Body) == "" {
		return fmt.Errorf("a report needs a subject and a description of what happened")
	}
	if !utf8.ValidString(r.Subject) || len(r.Subject) > 255 || strings.ContainsAny(r.Subject, "\r\n") {
		return fmt.Errorf("the subject must be one line of valid text, at most 255 bytes")
	}
	if !utf8.ValidString(r.Body) || len(r.Body) > 20000 {
		return fmt.Errorf("the description must be valid text, at most 20,000 bytes; shorten it before previewing or sending")
	}
	if len(r.Sections) > 20 {
		return fmt.Errorf("the report has too many diagnostic sections (maximum 20)")
	}
	return nil
}

// Submit files a report only in the IntakeProgram programme. The caller may supply
// a transport for testing; neither a form nor stored settings can redirect
// the destination. Do not retry automatically: a lost response may follow a
// successfully created work item, and this API is not an idempotent write.
func Submit(ctx context.Context, client *http.Client, token string, report Report) (IntakeReceipt, error) {
	var receipt IntakeReceipt
	if err := usableIntakeKey(token); err != nil {
		return receipt, err
	}
	report = report.Safe()
	// The configured credential must not occur in any transmitted text,
	// even when pasted without a recognizable "token=" prefix.
	clean := func(s string) string { return strings.ReplaceAll(s, token, "[removed]") }
	report.Subject, report.Body = clean(report.Subject), clean(report.Body)
	if err := report.Validate(); err != nil {
		return receipt, err
	}
	payload := intakeRequest{Program: IntakeProgram, Title: report.Subject,
		Description: report.Body, Severity: "medium", Source: IntakeProgram + "-whm", Logs: map[string]string{}}
	for i, section := range report.Sections {
		// Numbering keeps duplicate section headings from silently losing data.
		name := fmt.Sprintf("%02d %s", i+1, clean(section.Title))
		if len(name) > 100 {
			return receipt, fmt.Errorf("a diagnostic section name is too long")
		}
		payload.Logs[name] = clean(section.Text)
	}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > 4<<20 {
		return receipt, fmt.Errorf("the report could not be encoded within the intake's size limit; download it instead")
	}
	ctx, cancel := context.WithTimeout(ctx, 95*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, IntakeURL, bytes.NewReader(body))
	if err != nil {
		return receipt, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", IntakeProgram+"-bugreport")
	var sender http.Client
	if client != nil {
		sender = *client
	}
	// Never forward report content or credentials through a redirect,
	// including redirects to another path on this same host.
	sender.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := sender.Do(req)
	if err != nil {
		return receipt, fmt.Errorf("intake delivery could not be confirmed; check %s Intake before retrying, or download the report", IntakeProgram)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return receipt, fmt.Errorf("intake confirmation could not be read; check %s Intake before retrying", IntakeProgram)
	}
	var envelope struct {
		OK    bool          `json:"ok"`
		Data  IntakeReceipt `json:"data"`
		Error string        `json:"error"`
	}
	parsed := json.Unmarshal(raw, &envelope) == nil
	if (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated) || !parsed || !envelope.OK {
		reason := "delivery was not confirmed; check " + IntakeProgram + " Intake before retrying or download the report"
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			reason = "the intake key was rejected; ask the server administrator to replace it"
		case http.StatusTooManyRequests:
			reason = "the intake is rate-limited; wait before retrying"
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
				reason = fmt.Sprintf("the intake is rate-limited; wait %d seconds before retrying", seconds)
			}
		case http.StatusBadRequest:
			reason = "the intake rejected the report; verify the " + IntakeProgram + " programme and report fields"
		case http.StatusRequestEntityTooLarge:
			reason = "the intake refused this report's size; download it instead"
		}
		// Do not reflect arbitrary proxy pages, request bodies, or credentials
		// from upstream errors into a privileged page or the service journal.
		return receipt, fmt.Errorf("%s (HTTP %d%s)", reason, resp.StatusCode, requestReference(clean(resp.Header.Get("X-Request-ID"))))
	}
	receipt = envelope.Data
	if receipt.Program != IntakeProgram || receipt.IssueID == "" || (receipt.Action != "created" && receipt.Action != "commented") {
		return IntakeReceipt{}, fmt.Errorf("intake returned an unexpected confirmation; check %s Intake before retrying", IntakeProgram)
	}
	receipt.Identifier = clean(Redact(receipt.Identifier))
	for i := range receipt.Warnings {
		receipt.Warnings[i] = clean(Redact(receipt.Warnings[i]))
	}
	u, err := url.Parse(receipt.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || strings.Contains(receipt.URL, token) {
		receipt.URL = ""
	}
	return receipt, nil
}

var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

func requestReference(id string) string {
	if !requestIDPattern.MatchString(id) {
		return ""
	}
	return "; request " + id
}

func usableIntakeKey(key string) error {
	if len(key) < 16 || len(key) > 4096 {
		return fmt.Errorf("the intake key must contain 16–4096 printable characters")
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return fmt.Errorf("the intake key must not contain whitespace or control characters")
		}
	}
	return nil
}
