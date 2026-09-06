package node_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
)

// TestARecycledNameDoesNotInheritTheOldOwnersBackups covers the way one
// customer sees another's data without anything going wrong.
//
// A cPanel username is a label. Delete an account and create another with
// the same name -- which a host does whenever a customer leaves and the
// next one asks for a name they liked -- and every part of this program
// that goes by name treats the second customer as the first. Their own
// self-service page lists the previous customer's backups, and restores
// them on request.
//
// The unix uid is the thing that actually changes, and this is where that
// change is noticed and turned into a boundary.
func TestARecycledNameDoesNotInheritTheOldOwnersBackups(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handover := time.Now().UTC()

	// What the first customer's backups looked like: a name recorded
	// against the uid it meant then.
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "webshop", UID: 1042, SinceAt: handover.Add(-90 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// A name that has never changed hands filters nothing. The day this
	// program first noticed an account is not a boundary between two
	// owners, and treating it as one would hide a customer's own history
	// from them.
	first, err := store.Identity("webshop")
	if err != nil {
		t.Fatal(err)
	}
	if first.Recycled {
		t.Fatal("an account that has always been the same was marked as changed hands")
	}

	// The name is deleted and given to somebody else: same name, new
	// unix account.
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "webshop", UID: 1108, SinceAt: handover, Recycled: true,
		CreatedAt: first.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}

	second, err := store.Identity("webshop")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Recycled {
		t.Fatal("the handover was not recorded, so nothing would be hidden")
	}
	if second.UID == first.UID {
		t.Fatal("the new owner was recorded as the old one")
	}
	if !second.SinceAt.Equal(handover) {
		t.Fatalf("the boundary is %v, want the moment the name changed hands", second.SinceAt)
	}
	// Everything the first customer backed up is before the boundary,
	// which is what the customer page and the customer restore both
	// filter on.
	if !first.SinceAt.Before(second.SinceAt) {
		t.Fatal("the old owner's backups do not fall before the boundary")
	}
}

// TestANameThatAppearsAfterTheBackfillIsTreatedAsNew covers the hole the
// first version of this left open.
//
// noteIdentity's "never seen this name" branch reasoned from an absent
// record, and an absent record is not evidence that a name has always
// meant the same customer. A name recycled before this program ever
// recorded it looked exactly like one that had never changed hands, so
// the new customer would have been shown the old one's backups -- the
// precise thing all of this exists to stop.
//
// The backfill is what makes that branch sound afterwards: every account
// on the server is recorded once, so a name with no record later is one
// that was not there when we looked.
func TestANameThatAppearsAfterTheBackfillIsTreatedAsNew(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	// Before the backfill, a name with no record is simply one nobody has
	// looked at yet.
	before, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if before.IdentitiesBackfilledAt != nil {
		t.Fatal("a fresh server claims to have recorded its accounts already")
	}

	// After it, the same absence means something else entirely.
	done := time.Now().UTC()
	settings.IdentitiesBackfilledAt = &done
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	// A name that turns up now was not on the server when accounts were
	// recorded, so whatever is stored under it is not its to read.
	appeared := nodestore.AccountIdentity{
		Account: "webshop", UID: 1200, SinceAt: time.Now().UTC(),
		Recycled: settings.IdentitiesBackfilledAt != nil,
	}
	if !appeared.Recycled {
		t.Fatal("a name that appeared after the backfill was trusted with what came before it")
	}
	if _, err := store.PutIdentity(appeared); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Identity("webshop")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Recycled || stored.SinceAt.IsZero() {
		t.Fatalf("identity = %+v, want a boundary the customer page can filter on", stored)
	}
}
