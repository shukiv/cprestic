package job

import "testing"

func TestRollup(t *testing.T) {
	cases := []struct {
		name    string
		targets []TargetResult
		want    Status
	}{
		{
			name: "all succeeded",
			targets: []TargetResult{
				{Status: TargetSuccess}, {Status: TargetSuccess},
			},
			want: StatusSuccess,
		},
		{
			// One unavailable destination must not invalidate two good copies.
			name: "one of three failed",
			targets: []TargetResult{
				{Status: TargetSuccess}, {Status: TargetSuccess}, {Status: TargetFailed},
			},
			want: StatusPartialSuccess,
		},
		{
			name: "all failed",
			targets: []TargetResult{
				{Status: TargetFailed}, {Status: TargetFailed},
			},
			want: StatusFailed,
		},
		{
			name:    "no targets is a misconfiguration, not a pass",
			targets: nil,
			want:    StatusFailed,
		},
		{
			name: "every target skipped means no copy was made",
			targets: []TargetResult{
				{Status: TargetSkipped}, {Status: TargetSkipped},
			},
			want: StatusFailed,
		},
		{
			name: "skipped alongside a success does not downgrade the job",
			targets: []TargetResult{
				{Status: TargetSuccess}, {Status: TargetSkipped},
			},
			want: StatusSuccess,
		},
		{
			name: "unfinished target keeps the job running",
			targets: []TargetResult{
				{Status: TargetSuccess}, {Status: TargetRunning},
			},
			want: StatusRunning,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Rollup(tc.targets); got != tc.want {
				t.Errorf("Rollup = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatusTerminal(t *testing.T) {
	terminal := []Status{StatusSuccess, StatusPartialSuccess, StatusFailed, StatusCancelled}
	for _, status := range terminal {
		if !status.Terminal() {
			t.Errorf("%q should be terminal", status)
		}
	}
	for _, status := range []Status{StatusPending, StatusRunning} {
		if status.Terminal() {
			t.Errorf("%q should not be terminal", status)
		}
	}
}
