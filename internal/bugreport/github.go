package bugreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultRepository is where this program's own issues live.
const DefaultRepository = "shukiv/cprestic"

// NewIssueURL is GitHub's own new-issue form, filled in.
//
// This is the way that needs nothing configured: the operator is already
// signed in to GitHub in the browser that is open, and they press Submit
// themselves, having read what is in it. GitHub's forms are served over a
// URL, and a URL has a practical length -- so a long body is cut here and
// the operator is told the whole thing is on the page to paste.
func NewIssueURL(repository, subject, body string) string {
	if repository == "" {
		repository = DefaultRepository
	}
	const room = 6000
	trimmed := body
	if len(trimmed) > room {
		trimmed = trimmed[:room] + "\n\n[cut to fit a link -- the full report is on the page it came from]"
	}
	query := url.Values{"title": {subject}, "body": {trimmed}}
	return "https://github.com/" + repository + "/issues/new?" + query.Encode()
}

// UsableRepository checks a repository is one, before it is stored or put
// into a URL.
func UsableRepository(repository string) error {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("bugreport: %q is not an owner/repository", repository)
	}
	for _, part := range []string{owner, name} {
		for _, r := range part {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
			if !ok {
				return fmt.Errorf("bugreport: %q is not an owner/repository", repository)
			}
		}
	}
	return nil
}

// CreateIssue opens an issue through GitHub's API and returns its address.
//
// Used only when a token has been configured. Without one the browser goes
// to the pre-filled form instead, which asks nothing of this server.
func CreateIssue(ctx context.Context, client *http.Client,
	repository, token, subject, body string) (string, error) {

	if err := UsableRepository(repository); err != nil {
		return "", err
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("bugreport: no GitHub token is configured")
	}
	payload, err := json.Marshal(map[string]string{"title": subject, "body": body})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.github.com/repos/"+repository+"/issues", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("Content-Type", "application/json")

	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("bugreport: reach GitHub: %w", err)
	}
	defer response.Body.Close()

	// Enough to read the answer, and no more: this is a remote server's
	// reply and nothing here needs megabytes of it.
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("bugreport: read GitHub's answer: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var refusal struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(answer, &refusal)
		if refusal.Message == "" {
			refusal.Message = response.Status
		}
		return "", fmt.Errorf("bugreport: GitHub refused it: %s", refusal.Message)
	}
	var issue struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(answer, &issue); err != nil || issue.HTMLURL == "" {
		return "", fmt.Errorf("bugreport: GitHub accepted it but said where unreadably")
	}
	return issue.HTMLURL, nil
}
