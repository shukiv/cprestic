package reassemble

import "math"

// StagingBytes includes the downloaded payload, its extracted tree and the
// output archive, which may coexist for account and granular restores alike.
// Zero remains unknown so the staging allocator refuses it.
func StagingBytes(source uint64) uint64 {
	if source == 0 {
		return 0
	}
	const overhead = uint64(1 << 30)
	if source > (math.MaxUint64-overhead)/3 {
		return math.MaxUint64
	}
	return source*3 + overhead
}
