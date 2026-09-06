package webui_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// TestTestingADestinationCreatesTheRepository covers the state an operator
// is left in when adding a destination proves nothing.
//
// The repository is created when a destination is added, but only where
// the login was proved in the same request. An SFTP destination whose key
// the operator installs by hand is saved unproved: the card then said
// "Reachable · checked just now" and "not created yet" at the same time,
// with no way anywhere in the interface to create it. It appeared when
// the service was next restarted, which is not an answer.
//
// Testing a destination now creates what is missing, which is what the
// operator pressing it is asking for.
func TestTestingADestinationCreatesTheRepository(t *testing.T) {
	var mu sync.Mutex
	var commands []string
	client, _, engine := newUIWithExec(t,
		resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
			mu.Lock()
			commands = append(commands, strings.Join(cmd.Args, " "))
			mu.Unlock()
			for _, arg := range cmd.Args {
				if arg == "snapshots" {
					return resticrun.CommandResult{Stdout: []byte("[]")}, nil
				}
			}
			return resticrun.CommandResult{}, nil
		}))

	dest, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "Another Linux server", Type: "local",
		Config: map[string]string{"root": t.TempDir()},
	}, nil, "cp01")
	if err != nil {
		t.Fatal(err)
	}
	if repo.InitialisedAt != nil {
		t.Fatal("the fixture is wrong: adding the destination created the repository")
	}

	_, page := get(t, client, "/destinations")
	if !strings.Contains(page, "not created yet") {
		t.Error("the card does not say the repository is not there")
	}
	if !strings.Contains(page, "<b>Test</b> creates it") {
		t.Error("the card does not say what creates it")
	}

	resp, err := client.PostForm("http://ui/?p=destinations/test",
		url.Values{"csrf": {csrfToken(t, page)}, "id": {dest.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// The answer is the recovery key card for the repository it just
	// created, rendered in place rather than a redirect.
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("testing the destination answered %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Take its recovery") {
		t.Error("creating the repository did not end on its recovery key")
	}

	stored, err := engine.Store().Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.InitialisedAt == nil {
		t.Fatal("the destination was proved reachable and its repository still " +
			"does not exist, with nothing in the interface to create it")
	}

	mu.Lock()
	ran := strings.Join(commands, " | ")
	mu.Unlock()
	if !strings.Contains(ran, "init") {
		t.Errorf("restic was never asked to create the repository: %s", ran)
	}

	// And the card says so afterwards.
	_, page = get(t, client, "/destinations")
	if strings.Contains(page, "not created yet") {
		t.Error("the card still says the repository is not there")
	}
}
