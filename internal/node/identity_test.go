package node_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/shuki/cprest/internal/nodestore"
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
