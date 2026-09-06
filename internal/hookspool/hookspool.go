// Package hookspool is where a cPanel lifecycle hook leaves an event the
// service was not running to hear.
//
// The hook process is a short-lived program that WHM runs as root. It
// cannot open the service's database -- the service has it -- so when the
// socket does not answer it has nowhere to put what it was told, and a
// create or a remove that nobody heard is simply lost. Polling recovers
// most of what a lost event would have said, but not the one thing that
// matters: if a username is deleted and recreated while the service is
// down, and the operating system hands the new account the same uid, the
// account list looks identical to the one that was there before. The two
// customers then share an identity, and the second can read the first's
// backups.
//
// So the hook writes the event to a file first and the service replays it
// when it comes back. One file per event, created exclusively and synced
// before the hook reports success, named by the time it happened so the
// order survives a restart. Replay deletes the file only once the event
// has been recorded, which is why replaying twice has to be harmless.
package hookspool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultDir is where the spool lives on a cPanel server. It sits under
// the service's own state directory, which is root-owned and 0700.
const DefaultDir = "/var/lib/gniza/hooks"

// maxPayload matches the limit the lifecycle socket accepts, so nothing
// can be spooled that the service would have refused.
const maxPayload = 1 << 20

// Event is one cPanel lifecycle notification, kept as cPanel sent it.
//
// Only account creation and removal are spooled. They are the two facts
// polling cannot reconstruct, and they are the ones that decide whose
// backups a name may read. A suspension replayed hours late would queue a
// backup nobody is waiting for.
type Event struct {
	At      time.Time `json:"at"`
	Event   string    `json:"event"`
	Account string    `json:"account"`
	// Payload is what cPanel sent, kept so a replay decides the account
	// exactly as the live hook would have.
	Payload []byte `json:"payload,omitempty"`
}

// Spooled reports whether this event is one worth keeping for replay.
func Spooled(event string) bool {
	return event == "create" || event == "remove"
}

// Entry is a spooled event and the file it came from.
type Entry struct {
	Event
	Path string
}

// Write records an event durably, and returns only once it is on the
// disk. An error here is a real failure: the caller has been told
// something it now has no way to remember.
func Write(dir string, event Event) (string, error) {
	if !Spooled(event.Event) {
		return "", fmt.Errorf("hookspool: %s is not an event worth replaying", event.Event)
	}
	if len(event.Payload) > maxPayload {
		return "", fmt.Errorf("hookspool: the %s hook payload is larger than 1 MiB", event.Event)
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Account == "" {
		event.Account = AccountIn(event.Payload)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("hookspool: make %s: %w", dir, err)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("hookspool: encode the %s event: %w", event.Event, err)
	}

	// The name carries the time so replay can put events in the order
	// they happened -- a remove before the create that reused the name --
	// and a counter so two hooks in the same nanosecond cannot collide.
	base := fmt.Sprintf("%020d-%s-%s", event.At.UTC().UnixNano(), event.Event, event.Account)
	for attempt := 0; attempt < 100; attempt++ {
		name := filepath.Join(dir, base+".json")
		if attempt > 0 {
			name = filepath.Join(dir, base+"-"+strconv.Itoa(attempt)+".json")
		}
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("hookspool: write %s: %w", name, err)
		}
		if _, err := file.Write(body); err != nil {
			file.Close()
			return "", fmt.Errorf("hookspool: write %s: %w", name, err)
		}
		// Both syncs matter: the first so the event survives a crash, the
		// second so its name does.
		if err := file.Sync(); err != nil {
			file.Close()
			return "", fmt.Errorf("hookspool: flush %s: %w", name, err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("hookspool: close %s: %w", name, err)
		}
		if handle, err := os.Open(dir); err == nil {
			_ = handle.Sync()
			handle.Close()
		}
		return name, nil
	}
	return "", fmt.Errorf("hookspool: could not find a free name for a %s event", event.Event)
}

// Pending is everything waiting to be replayed, oldest first.
//
// A file that cannot be read is left where it is and reported: deleting
// what we could not understand would throw away the only record of an
// ownership boundary.
func Pending(dir string) ([]Entry, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("hookspool: read %s: %w", dir, err)}
	}

	var (
		pending  []Entry
		problems []error
	)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	// The name begins with a zero-padded nanosecond timestamp, so sorting
	// the names is sorting by when the event happened.
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Errorf("hookspool: read %s: %w", path, err))
			continue
		}
		var event Event
		if err := json.Unmarshal(body, &event); err != nil {
			problems = append(problems, fmt.Errorf("hookspool: read %s: %w", path, err))
			continue
		}
		if !Spooled(event.Event) || event.Account == "" {
			problems = append(problems, fmt.Errorf(
				"hookspool: %s does not describe an account event", path))
			continue
		}
		pending = append(pending, Entry{Event: event, Path: path})
	}
	return pending, problems
}

// Done removes an event that has been recorded. It runs after the fact is
// stored, never before: a crash in between replays the event a second
// time, which is harmless, while the other order loses it.
func Done(entry Entry) error {
	if err := os.Remove(entry.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("hookspool: remove %s: %w", entry.Path, err)
	}
	return nil
}
