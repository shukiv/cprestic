package reassemble

import (
	"math"
	"testing"
)

func TestRestoreSpaceCountsAllCoexistingCopies(t *testing.T) {
	if got := StagingBytes(0); got != 0 {
		t.Fatalf("unknown source size became a usable estimate: %d", got)
	}
	if got := StagingBytes(10 << 30); got < 30<<30 {
		t.Fatalf("raw metadata, extracted tree and output archive cannot coexist: %d", got)
	}
	if got := StagingBytes(math.MaxUint64); got != math.MaxUint64 {
		t.Fatalf("overflow made a huge restore small: %d", got)
	}
}
