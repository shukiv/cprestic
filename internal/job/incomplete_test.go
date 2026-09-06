package job

import "testing"

func TestIncompleteCopiesDoNotCountAsSuccessfulBackups(t *testing.T) {
	partial := TargetResult{Status: TargetSuccess, Incomplete: true}
	if got := Rollup([]TargetResult{partial}); got != StatusFailed {
		t.Fatalf("only an incomplete snapshot exists, but job is %s", got)
	}
	if got := Rollup([]TargetResult{partial, {Status: TargetSuccess}}); got != StatusPartialSuccess {
		t.Fatalf("incomplete copy was hidden by the complete one: %s", got)
	}
}
