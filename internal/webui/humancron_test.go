package webui

import "testing"

func TestHumanCronReadsBackTheShapesTheFormOffers(t *testing.T) {
	for expr, want := range map[string]string{
		"0 2 * * *":   "Every day at 02:00",
		"30 23 * * *": "Every day at 23:30",
		"0 3 * * 0":   "Every Sunday at 03:00",
		"0 3 * * 7":   "Every Sunday at 03:00",
		"15 * * * *":  "Every hour at :15",
	} {
		if got := humanCron(expr); got != want {
			t.Errorf("humanCron(%q) = %q, want %q", expr, got, want)
		}
	}
}

// An expression the form never produces is the operator's own. Reading it
// back wrongly would be worse than not reading it back at all.
func TestHumanCronStaysQuietOnAnythingElse(t *testing.T) {
	for _, expr := range []string{
		"*/5 * * * *",
		"0 2 1 * *",
		"0 2 * 6 *",
		"0 2,14 * * *",
		"nonsense",
		"",
	} {
		if got := humanCron(expr); got != "" {
			t.Errorf("humanCron(%q) = %q, want empty", expr, got)
		}
	}
}
