package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/shuki/cprest/internal/bugreport"
)

// BugRepository is where this server's reports go.
func (e *Engine) BugRepository() string {
	if repository, err := e.store.Settings(); err == nil && repository.BugRepository != "" {
		return repository.BugRepository
	}
	return bugreport.DefaultRepository
}

// HasBugToken says whether a report can be sent from here, rather than
// handed to the browser to submit.
func (e *Engine) HasBugToken() bool {
	settings, err := e.store.Settings()
	return err == nil && settings.BugTokenSecretID != ""
}

// SetBugToken stores a GitHub token, sealed, or forgets the one there.
func (e *Engine) SetBugToken(token string) error {
	settings, err := e.store.Settings()
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		settings.BugTokenSecretID = ""
		return e.store.SaveSettings(settings)
	}
	sealed, err := e.vault.Seal([]byte(strings.TrimSpace(token)))
	if err != nil {
		return err
	}
	id, err := e.store.PutSecret("github_token", sealed, e.vault.KeyID())
	if err != nil {
		return err
	}
	settings.BugTokenSecretID = id
	return e.store.SaveSettings(settings)
}

// ReportBug opens an issue for what an operator has written, and returns
// where it went.
//
// Only with a token: without one the interface sends the operator to
// GitHub's own form with the report filled in, which is the same thing
// happening in the browser where somebody is already signed in.
func (e *Engine) ReportBug(ctx context.Context, subject, body string) (string, error) {
	settings, err := e.store.Settings()
	if err != nil {
		return "", err
	}
	if settings.BugTokenSecretID == "" {
		return "", fmt.Errorf("node: no GitHub token is configured")
	}
	token, err := e.openSecret(settings.BugTokenSecretID)
	if err != nil {
		return "", err
	}
	repository := settings.BugRepository
	if repository == "" {
		repository = bugreport.DefaultRepository
	}
	return bugreport.CreateIssue(ctx, nil, repository, string(token), subject, body)
}
