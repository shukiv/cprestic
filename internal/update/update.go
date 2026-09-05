// Package update asks GitHub what the newest published release is.
//
// It reads and nothing else. Whether anything is installed as a result is a
// decision somebody makes in the interface, because an update that arrives
// on its own is a way for whoever can publish a release to reach every
// server running this, as root, with nobody looking.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases are published.
const Repo = "shukiv/cprestic"

// Release is one published release, as much of it as is worth showing.
type Release struct {
	Version   string
	URL       string
	Notes     string
	Published time.Time
}

// Latest reads the newest release. GitHub's "latest" excludes drafts and
// pre-releases, so a release published for testing is not offered to
// somebody's production server.
func Latest(ctx context.Context, client *http.Client, repo string) (Release, error) {
	if repo == "" {
		repo = Repo
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return latestFrom(ctx, client, "https://api.github.com/repos/"+repo+"/releases/latest")
}

// latestFrom is Latest against a given address, so the parsing can be
// tested without asking GitHub.
func latestFrom(ctx context.Context, client *http.Client, address string) (Release, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return Release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	// Unauthenticated, so GitHub rate-limits by address. One request a day
	// is nowhere near the limit, and a token here would be a credential
	// stored on every cPanel server for the sake of reading a version
	// number that is public.
	request.Header.Set("User-Agent", "cprest")

	response, err := client.Do(request)
	if err != nil {
		return Release{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub answered %s", response.Status)
	}

	// Bounded: this is a public endpoint answering a program that runs as
	// root, and release notes are written by people.
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Release{}, err
	}
	var payload struct {
		Tag         string    `json:"tag_name"`
		URL         string    `json:"html_url"`
		Body        string    `json:"body"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Release{}, err
	}
	if payload.Tag == "" {
		return Release{}, fmt.Errorf("that release has no tag")
	}
	return Release{
		Version:   payload.Tag,
		URL:       payload.URL,
		Notes:     payload.Body,
		Published: payload.PublishedAt,
	}, nil
}

// Newer reports whether latest is a later release than current.
//
// A build that is not exactly a tag -- "dev", or a git describe like
// v0.1.0-3-gabc1234-dirty -- is never told about a newer version. Somebody
// running a build of their own knows where it came from, and an offer to
// replace it with a release would be an offer to discard their work.
func Newer(current, latest string) bool {
	running, ok := parse(current)
	if !ok {
		return false
	}
	published, ok := parse(latest)
	if !ok {
		return false
	}
	for i := range running {
		if published[i] != running[i] {
			return published[i] > running[i]
		}
	}
	return false
}

// parse reads vMAJOR.MINOR.PATCH, and refuses anything with more to it.
func parse(version string) ([3]int, bool) {
	var parsed [3]int
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return parsed, false
	}
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return parsed, false
		}
		parsed[i] = number
	}
	return parsed, true
}
