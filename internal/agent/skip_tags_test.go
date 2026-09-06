package agent

import (
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/resticrun"
)

// A schedule that leaves part of the account out must say so on the
// snapshot it writes. Without it a backup taken without the databases is
// indistinguishable from a full one: a whole-account restore picks it, and
// retention counts it towards the keeps and evicts a complete backup to
// make room for it.
func TestASnapshotSaysWhatTheScheduleLeftOut(t *testing.T) {
	full := skipTags(protocol.JobAssignment{})
	if len(full) != 0 {
		t.Errorf("a backup of the whole account is tagged %v", full)
	}

	tags := skipTags(protocol.JobAssignment{
		SkipHomedir: true, SkipDatabases: true, SkipEmail: true})
	want := []string{"skip:homedir", "skip:databases", "skip:email"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Fatalf("tags = %v, want %v in that order -- the tag set decides the "+
			"retention group, and an order that varies between runs splits it",
			tags, want)
	}

	// And what the restore side reads back is what was written.
	snapshot := resticrun.Snapshot{Tags: append(
		[]string{"account:webshop", "mode:split"}, tags...)}
	if snapshot.Complete() {
		t.Error("a backup taken without three parts of the account reads as complete")
	}
	if got := strings.Join(snapshot.Skipped(), ","); got != "homedir,databases,email" {
		t.Errorf("Skipped() = %q", got)
	}

	whole := resticrun.Snapshot{Tags: []string{"account:webshop", "mode:split"}}
	if !whole.Complete() || len(whole.Skipped()) != 0 {
		t.Error("a full backup does not read as one")
	}
}

// A schedule that skips databases must reach the tag from the assignment,
// not from the payload: an account with no databases produces a payload
// with no database part either, and the two mean different things.
func TestSkippingIsReadFromTheScheduleNotTheAccount(t *testing.T) {
	if tags := skipTags(protocol.JobAssignment{SkipDatabases: true}); len(tags) != 1 ||
		tags[0] != "skip:databases" {
		t.Fatalf("tags = %v", tags)
	}
	if tags := skipTags(protocol.JobAssignment{}); len(tags) != 0 {
		t.Fatalf("an account that simply has no databases was tagged %v, which "+
			"would put it in a retention group of its own and refuse it for "+
			"whole-account recovery", tags)
	}
}
