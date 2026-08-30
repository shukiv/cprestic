package agent

import (
	"strings"
	"testing"
)

func TestTrimDetailKeepsTheTail(t *testing.T) {
	// restic names every file it could not read, and on a busy account
	// that is thousands of lines. The ones at the end are the summary and
	// the most recent failures, so those are the ones worth keeping.
	var builder strings.Builder
	for i := range 5000 {
		builder.WriteString("error: open /home/customer1/tmp/sess_")
		builder.WriteString(strings.Repeat("a", 12))
		builder.WriteString(": no such file\n")
		_ = i
	}
	builder.WriteString("LAST LINE: 3 files could not be read\n")

	trimmed := trimDetail(builder.String())
	if len(trimmed) > maxDetail+64 {
		t.Errorf("kept %d bytes, want about %d", len(trimmed), maxDetail)
	}
	if !strings.Contains(trimmed, "LAST LINE: 3 files could not be read") {
		t.Error("the tail, which is where restic's summary is, was cut off")
	}
	if !strings.HasPrefix(trimmed, "… earlier output omitted …") {
		t.Error("truncation is not signposted, so the log looks complete when it is not")
	}
	// Truncation must not leave a half line at the top.
	firstBody := strings.SplitN(trimmed, "\n", 3)[1]
	if firstBody != "" && !strings.HasPrefix(firstBody, "error: open") {
		t.Errorf("first kept line is a fragment: %q", firstBody)
	}
}

func TestTrimDetailLeavesShortOutputAlone(t *testing.T) {
	const short = "warning: could not read /home/c1/.cache/x\n"
	if got := trimDetail(short); got != strings.TrimSpace(short) {
		t.Errorf("got %q, want it unchanged", got)
	}
	if got := trimDetail("   \n  "); got != "" {
		t.Errorf("blank output became %q", got)
	}
}
