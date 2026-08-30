package pkgacct

import (
	"strings"
	"testing"
)

const helpModern = `Usage: pkgacct [options] user [dir]
  --nocompress      do not compress the archive
  --skiphomedir     skip the home directory
  --skipdb          skip databases
`

const helpOld = `Usage: pkgacct user [dir]
  --skiphome        skip the home directory
`

func TestProbeCapabilities(t *testing.T) {
	modern := ProbeCapabilities(helpModern)
	if modern.NoCompressFlag != "--nocompress" {
		t.Errorf("NoCompressFlag = %q", modern.NoCompressFlag)
	}
	if modern.SkipHomedirFlag != "--skiphomedir" {
		t.Errorf("SkipHomedirFlag = %q", modern.SkipHomedirFlag)
	}
	if modern.SkipDBFlag != "--skipdb" {
		t.Errorf("SkipDBFlag = %q", modern.SkipDBFlag)
	}

	old := ProbeCapabilities(helpOld)
	if old.NoCompressFlag != "" {
		t.Errorf("NoCompressFlag = %q, want empty for a version without it", old.NoCompressFlag)
	}
	if old.SkipHomedirFlag != "--skiphome" {
		t.Errorf("SkipHomedirFlag = %q", old.SkipHomedirFlag)
	}

	if empty := ProbeCapabilities(""); empty != (Capabilities{}) {
		t.Errorf("empty help should yield no capabilities, got %+v", empty)
	}
}

func TestPlanSplit(t *testing.T) {
	payload, err := Plan(PlanRequest{
		Account:    "customer1",
		HomeDir:    "/home/customer1",
		Databases:  []string{"customer1_wp", "customer1_shop"},
		StagingDir: "/var/cprest/staging/job-42",
		Mode:       ModeSplit,
		Caps:       ProbeCapabilities(helpModern),
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if payload.Degraded {
		t.Errorf("split mode on a capable server should not be degraded: %s", payload.Reason)
	}
	want := []string{
		"/var/cprest/staging/job-42/metadata",
		"/home/customer1",
		"/var/cprest/staging/job-42/databases/customer1_wp.sql",
		"/var/cprest/staging/job-42/databases/customer1_shop.sql",
	}
	got := payload.Paths()
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
}

func TestPlanSplitDegradesWithoutSkipHomedir(t *testing.T) {
	payload, err := Plan(PlanRequest{
		Account: "c1", HomeDir: "/home/c1", StagingDir: "/stage", Mode: ModeSplit,
		Caps: Capabilities{},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !payload.Degraded {
		t.Error("split mode without a skip-homedir flag duplicates the home directory and is degraded")
	}
}

func TestPlanMonolithicDegradesWithoutNoCompress(t *testing.T) {
	capable, err := Plan(PlanRequest{
		Account: "c1", StagingDir: "/stage", Mode: ModeMonolithic,
		Caps: ProbeCapabilities(helpModern),
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if capable.Degraded {
		t.Errorf("uncompressed monolithic should not be degraded: %s", capable.Reason)
	}
	if got := capable.Paths()[0]; !strings.HasSuffix(got, "cpmove-c1.tar") {
		t.Errorf("path = %q, want an uncompressed tar", got)
	}

	// A gzip stream has no stable chunk boundaries between runs, so restic
	// stores a full copy every night. That must be surfaced, not silent.
	incapable, err := Plan(PlanRequest{
		Account: "c1", StagingDir: "/stage", Mode: ModeMonolithic, Caps: Capabilities{},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !incapable.Degraded {
		t.Error("compressed monolithic payload should be reported as degraded")
	}
	if got := incapable.Paths()[0]; !strings.HasSuffix(got, ".tar.gz") {
		t.Errorf("path = %q, want a compressed archive", got)
	}
}

func TestPlanValidation(t *testing.T) {
	if _, err := Plan(PlanRequest{StagingDir: "/stage"}); err == nil {
		t.Error("missing account should be rejected")
	}
	if _, err := Plan(PlanRequest{Account: "c1"}); err == nil {
		t.Error("missing staging directory should be rejected")
	}
	if _, err := Plan(PlanRequest{Account: "c1", StagingDir: "/s", Mode: ModeSplit}); err == nil {
		t.Error("split mode without a home directory should be rejected")
	}
	if _, err := Plan(PlanRequest{Account: "c1", StagingDir: "/s", Mode: "weird"}); err == nil {
		t.Error("unknown mode should be rejected")
	}
}

func TestCommandArgs(t *testing.T) {
	modern := ProbeCapabilities(helpModern)

	split := CommandArgs("customer1", "/stage/customer1", ModeSplit, modern)
	assertContains(t, split, "--nocompress", "--skiphomedir", "--skipdb", "customer1")
	if last := split[len(split)-1]; last != "/stage/customer1/metadata" {
		t.Errorf("split target = %q, want the metadata subdirectory", last)
	}

	monolithic := CommandArgs("customer1", "/stage/customer1", ModeMonolithic, modern)
	assertContains(t, monolithic, "--nocompress", "customer1")
	for _, unwanted := range []string{"--skiphomedir", "--skipdb"} {
		if contains(monolithic, unwanted) {
			t.Errorf("monolithic mode should not pass %s: the archive must be complete", unwanted)
		}
	}
	if last := monolithic[len(monolithic)-1]; last != "/stage/customer1" {
		t.Errorf("monolithic target = %q", last)
	}

	// A host without the flags gets none of them rather than a guess.
	bare := CommandArgs("c1", "/stage", ModeSplit, Capabilities{})
	if len(bare) != 2 || bare[0] != "c1" || bare[1] != "/stage/metadata" {
		t.Errorf("args = %v, want just the account and target", bare)
	}
}

func assertContains(t *testing.T, args []string, wanted ...string) {
	t.Helper()
	for _, want := range wanted {
		if !contains(args, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
}

func contains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
