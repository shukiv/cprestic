package resticrun_test

import (
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/resticrun"
)

// TestForgetAlwaysGroupsByHostAndTags is the guard against deleting one
// server's backups to satisfy another's retention.
//
// A keep count counts within a group. restic's default groups by host and
// paths, and this program's paths move when a schedule's payload mode
// changes; grouping by tags alone would put two servers writing into one
// repository in the same group, and "keep seven daily" would then keep
// seven between them.
func TestForgetAlwaysGroupsByHostAndTags(t *testing.T) {
	args, err := resticrun.ForgetArgs(resticrun.ForgetSpec{KeepDaily: 7})
	if err != nil {
		t.Fatalf("ForgetArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--group-by host,tags") {
		t.Fatalf("args = %v, want grouping by host and tags", args)
	}
}

// TestADryRunNeverPrunes covers the combination that would make a
// question destructive: prune removes the data a forget unreferenced, and
// a dry run must not do that whatever else it is asked for.
func TestADryRunNeverPrunes(t *testing.T) {
	if _, err := resticrun.ForgetArgs(resticrun.ForgetSpec{
		KeepDaily: 7, DryRun: true, Prune: true,
	}); err == nil {
		t.Fatal("a dry run was allowed to prune")
	}

	args, err := resticrun.ForgetArgs(resticrun.ForgetSpec{KeepDaily: 7, DryRun: true})
	if err != nil {
		t.Fatalf("ForgetArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--dry-run") {
		t.Errorf("args = %v, want a dry run", args)
	}
	if strings.Contains(joined, "--prune") {
		t.Errorf("args = %v, a dry run asked to prune", args)
	}
}

// TestForgetWithNoKeepPolicyIsRefused covers the argument list that would
// delete every backup in the repository.
func TestForgetWithNoKeepPolicyIsRefused(t *testing.T) {
	if _, err := resticrun.ForgetArgs(resticrun.ForgetSpec{}); err == nil {
		t.Fatal("a forget with no keep policy was allowed")
	}
}

// realPlan is what restic 0.19.1 answered on a live repository, trimmed to
// two groups and to the snapshot fields this program reads.
const realPlan = `[
 {"tags":["account:studio","mode:split"],"host":"mx.7171.online","paths":null,
  "keep":[{"time":"2026-08-31T23:34:27-04:00","id":"aa","short_id":"aa"},
          {"time":"2026-08-30T02:03:00-04:00","id":"bb","short_id":"bb"}],
  "remove":[{"time":"2026-08-31T23:24:03-04:00","id":"cc","short_id":"cc"},
            {"time":"2026-08-31T23:20:00-04:00","id":"dd","short_id":"dd"}]},
 {"tags":["account:service","mode:split"],"host":"mx.7171.online","paths":null,
  "keep":[{"time":"2026-08-31T02:03:48-04:00","id":"ee","short_id":"ee"}],
  "remove":null}
]`

func TestParseForgetPlanReadsARealAnswer(t *testing.T) {
	plan, err := resticrun.ParseForgetPlan([]byte(realPlan))
	if err != nil {
		t.Fatalf("ParseForgetPlan: %v", err)
	}
	if plan.Kept != 3 || plan.Removed != 2 {
		t.Fatalf("plan keeps %d and removes %d, want 3 and 2", plan.Kept, plan.Removed)
	}
	if len(plan.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(plan.Groups))
	}
	// Whatever loses the most comes first: that is what an operator is
	// deciding about.
	if plan.Groups[0].Account != "studio" {
		t.Errorf("first group is %q, want the one losing backups", plan.Groups[0].Account)
	}
	if plan.Groups[0].Oldest.IsZero() || plan.Groups[0].Newest.IsZero() {
		t.Error("the span of what would go was not read")
	}
	// A null "remove" is an account losing nothing, not a parse failure.
	if plan.Groups[1].Removed != 0 || plan.Groups[1].Kept != 1 {
		t.Errorf("second group = %+v, want one kept and none removed", plan.Groups[1])
	}
}

// TestAStaleLockIsNotAnEmptyPlan covers what restic writes to stdout when
// it cannot run at all. Reading that as an array of no groups would
// report "nothing to remove" for a repository whose retention has in fact
// never run — the silence this whole feature exists to end.
func TestAStaleLockIsNotAnEmptyPlan(t *testing.T) {
	locked := `{"message_type":"exit_error","code":11,` +
		`"message":"unable to create lock in backend: repository is already locked"}`
	plan, err := resticrun.ParseForgetPlan([]byte(locked))
	if err == nil {
		t.Fatalf("a locked repository parsed as a plan: %+v", plan)
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("err = %v, want it to say the repository was locked", err)
	}
}
