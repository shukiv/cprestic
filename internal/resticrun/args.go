package resticrun

import (
	"fmt"
	"sort"
	"strconv"
)

// BackupSpec describes one restic backup invocation.
type BackupSpec struct {
	// Paths are the staged files or directories to back up.
	Paths []string
	// Tags are attached to the snapshot, e.g. account and job identifiers.
	Tags []string
	// Host overrides the snapshot's recorded hostname so snapshots stay
	// attributable after a server is renamed or replaced.
	Host string
	// Exclude patterns are passed through to restic.
	Exclude []string
	// LimitUploadKiB throttles upload bandwidth. Zero means unlimited.
	LimitUploadKiB int
	// OnProgress, when set, is called as restic reports progress. It is
	// called from the goroutine reading restic's output, so an
	// implementation must be quick and safe to call concurrently with the
	// rest of the program.
	OnProgress func(Progress)
}

// ForgetSpec describes a retention run. It is executed by the maintenance
// runner, never by an agent: append-only destinations reject deletes from
// agent credentials by design. See docs/DESIGN.md §8.
type ForgetSpec struct {
	KeepLast    int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	KeepYearly  int
	// Tags restricts the policy to matching snapshots.
	Tags []string
	// GroupBy is restic's --group-by. Empty means restic's default of
	// "host,paths".
	GroupBy string
	// Prune actually removes the unreferenced data.
	Prune bool
}

// CheckSpec describes an integrity check.
type CheckSpec struct {
	// ReadDataSubsetPercent verifies a rolling fraction of pack data.
	// Zero checks structure only.
	ReadDataSubsetPercent int
}

// BackupArgs builds the argument list for "restic backup".
//
// The repository, password and backend credentials are supplied through the
// environment, so they never appear here.
func BackupArgs(spec BackupSpec) ([]string, error) {
	if len(spec.Paths) == 0 {
		return nil, fmt.Errorf("resticrun: backup needs at least one path")
	}
	args := []string{"backup", "--json"}
	if spec.Host != "" {
		args = append(args, "--host", spec.Host)
	}
	for _, tag := range spec.Tags {
		args = append(args, "--tag", tag)
	}
	for _, pattern := range spec.Exclude {
		args = append(args, "--exclude", pattern)
	}
	if spec.LimitUploadKiB > 0 {
		args = append(args, "--limit-upload", strconv.Itoa(spec.LimitUploadKiB))
	}
	return append(args, spec.Paths...), nil
}

// InitArgs builds the argument list for "restic init".
//
// chunkerSourceURI, when set, seeds the new repository with an existing
// repository's chunker parameters. Those parameters are fixed at creation
// and can never be changed, so every repository after a server's first must
// be created this way to keep "restic copy" available later.
// See docs/DESIGN.md §7.
func InitArgs(chunkerSourceURI string) []string {
	args := []string{"init", "--repository-version", "2"}
	if chunkerSourceURI != "" {
		args = append(args, "--from-repo", chunkerSourceURI, "--copy-chunker-params")
	}
	return args
}

// ForgetArgs builds the argument list for "restic forget".
func ForgetArgs(spec ForgetSpec) ([]string, error) {
	args := []string{"forget", "--json"}
	keeps := []struct {
		flag  string
		value int
	}{
		{"--keep-last", spec.KeepLast},
		{"--keep-daily", spec.KeepDaily},
		{"--keep-weekly", spec.KeepWeekly},
		{"--keep-monthly", spec.KeepMonthly},
		{"--keep-yearly", spec.KeepYearly},
	}
	specified := 0
	for _, keep := range keeps {
		if keep.value < 0 {
			return nil, fmt.Errorf("resticrun: %s must not be negative", keep.flag)
		}
		if keep.value > 0 {
			args = append(args, keep.flag, strconv.Itoa(keep.value))
			specified++
		}
	}
	if specified == 0 {
		// A forget with no keep policy would delete every snapshot.
		return nil, fmt.Errorf("resticrun: forget needs at least one keep policy")
	}
	for _, tag := range spec.Tags {
		args = append(args, "--tag", tag)
	}
	if spec.GroupBy != "" {
		args = append(args, "--group-by", spec.GroupBy)
	}
	if spec.Prune {
		args = append(args, "--prune")
	}
	return args, nil
}

// CheckArgs builds the argument list for "restic check".
func CheckArgs(spec CheckSpec) ([]string, error) {
	args := []string{"check"}
	switch {
	case spec.ReadDataSubsetPercent < 0 || spec.ReadDataSubsetPercent > 100:
		return nil, fmt.Errorf("resticrun: read-data subset %d%% is out of range",
			spec.ReadDataSubsetPercent)
	case spec.ReadDataSubsetPercent > 0:
		args = append(args, "--read-data-subset",
			strconv.Itoa(spec.ReadDataSubsetPercent)+"%")
	}
	return args, nil
}

// envSlice renders an environment map as a sorted "K=V" slice. Sorting keeps
// commands reproducible, which makes them comparable in tests and in logs.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}
