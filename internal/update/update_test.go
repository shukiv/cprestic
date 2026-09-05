package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A build that is not a release is never told to upgrade: somebody running
// their own build knows where it came from.
func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.1.0", "v0.1.1", true},
		{"v0.9.9", "v1.0.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.2.0", "v0.1.9", false},
		{"v0.1.0-3-gabc1234-dirty", "v0.2.0", false},
		{"dev", "v0.2.0", false},
		{"v0.1.0", "", false},
		{"v0.1.0", "nightly", false},
		{"0.1.0", "0.2.0", true},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestLatest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/who/what/releases/latest" {
			t.Errorf("asked for %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"tag_name": "v0.3.0",
			"html_url": "https://example.invalid/releases/v0.3.0",
			"body": "what changed",
			"published_at": "2026-09-05T12:00:00Z"
		}`))
	}))
	defer server.Close()

	release, err := latestFrom(context.Background(), server.Client(),
		server.URL+"/repos/who/what/releases/latest")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if release.Version != "v0.3.0" || release.Notes != "what changed" {
		t.Fatalf("read %+v", release)
	}
	if release.Published.Year() != 2026 {
		t.Fatalf("published %v", release.Published)
	}
}

// A release without a tag is not something to compare a version against.
func TestLatestRefusesAnswerWithoutTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"html_url": "https://example.invalid"}`))
	}))
	defer server.Close()

	if _, err := latestFrom(context.Background(), server.Client(), server.URL); err == nil {
		t.Fatal("accepted a release with no tag")
	}
}

func TestLatestRefusesRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()

	if _, err := latestFrom(context.Background(), server.Client(), server.URL); err == nil {
		t.Fatal("accepted a 403")
	}
}
