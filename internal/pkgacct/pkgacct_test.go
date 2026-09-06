package pkgacct

import (
	"os"
	"path/filepath"
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
	// restic is pointed at the database directory rather than at each
	// dump, so a snapshot's paths stay identical when an account gains or
	// loses a database. Paths that change would put every run in its own
	// retention group, and a group of one is never pruned.
	want := []string{
		"/var/cprest/staging/job-42/metadata",
		"/home/customer1",
		"/var/cprest/staging/job-42/databases",
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

	for _, database := range []string{"customer1_wp", "customer1_shop"} {
		want := "/var/cprest/staging/job-42/databases/" + database + ".sql"
		if got := payload.DumpPaths[database]; got != want {
			t.Errorf("dump path for %s = %q, want %q", database, got, want)
		}
	}
}

func TestPlanSplitPathsAreStableAcrossDatabaseChanges(t *testing.T) {
	plan := func(databases ...string) []string {
		t.Helper()
		payload, err := Plan(PlanRequest{
			Account: "c1", HomeDir: "/home/c1", Databases: databases,
			StagingDir: "/stage", Mode: ModeSplit, Caps: ProbeCapabilities(helpModern),
		})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		return payload.Paths()
	}

	before := plan("c1_wp")
	after := plan("c1_wp", "c1_shop")
	if len(before) != len(after) {
		t.Fatalf("adding a database changed the snapshot paths:\n  %v\n  %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("adding a database changed the snapshot paths:\n  %v\n  %v", before, after)
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

	split := CommandArgs("customer1", "/stage/customer1", ModeSplit, modern, false)
	assertContains(t, split, "--nocompress", "--skiphomedir", "--skipdb", "customer1")
	if last := split[len(split)-1]; last != "/stage/customer1/metadata" {
		t.Errorf("split target = %q, want the metadata subdirectory", last)
	}

	monolithic := CommandArgs("customer1", "/stage/customer1", ModeMonolithic, modern, false)
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
	bare := CommandArgs("c1", "/stage", ModeSplit, Capabilities{}, false)
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

func TestVerifyRejectsAnIncompletePayload(t *testing.T) {
	dir := t.TempDir()
	metadata := filepath.Join(dir, "metadata")
	home := filepath.Join(dir, "home")
	for _, path := range []string{metadata, home} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	payload := Payload{Mode: ModeSplit, Account: "c1", Parts: []Part{
		{Kind: PartMetadata, Path: metadata},
		{Kind: PartHomedir, Path: home},
	}}

	// An empty metadata directory is what pkgacct leaves when it writes
	// nothing and still exits zero. restic would warn and carry on, so
	// this has to be caught before the backup runs.
	err := payload.Verify()
	if err == nil {
		t.Fatal("an empty metadata directory was accepted")
	}
	if !strings.Contains(err.Error(), "metadata") || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want it to name the empty part", err)
	}

	if err := os.WriteFile(filepath.Join(metadata, "cpmove-c1.tar"),
		[]byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "index.html"),
		[]byte("<h1>hi</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := payload.Verify(); err != nil {
		t.Errorf("a complete payload was rejected: %v", err)
	}
}

func TestVerifyRejectsAMissingPart(t *testing.T) {
	dir := t.TempDir()
	payload := Payload{Parts: []Part{
		{Kind: PartMetadata, Path: filepath.Join(dir, "nothing-here")},
	}}
	err := payload.Verify()
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v, want it to report the missing part", err)
	}
}

func TestVerifyRejectsAnEmptyArchive(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "cpmove-c1.tar")
	if err := os.WriteFile(archive, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A zero-byte archive restores nothing at all.
	payload := Payload{Parts: []Part{{Kind: PartArchive, Path: archive}}}
	if err := payload.Verify(); err == nil {
		t.Error("a zero-byte archive was accepted")
	}
}

// TestSkipEmailReachesPkgacct covers a schedule that says to leave email
// out. Excluding ~/mail keeps the messages out of the file backup, but
// pkgacct packs the mail configuration — the mail account names and the
// hashes with them — inside its own archive, and no restic exclude can
// reach in there. So a backup an operator believed held no email held the
// credentials to all of it.
func TestSkipEmailReachesPkgacct(t *testing.T) {
	installed := Capabilities{
		NoCompressFlag: "--nocompress", SkipHomedirFlag: "--skiphomedir",
		SkipDBFlag: "--skipmysql", SkipMailFlag: "--skipmail",
	}
	args := CommandArgs("customer1", "/stage/customer1", ModeSplit, installed, true)
	if !contains(args, "--skipmail") {
		t.Errorf("args = %v, want pkgacct told to leave mail out", args)
	}
	kept := CommandArgs("customer1", "/stage/customer1", ModeSplit, installed, false)
	if contains(kept, "--skipmail") {
		t.Errorf("args = %v, mail was left out of a backup that wanted it", kept)
	}
	// A cPanel with no such flag must not be handed one.
	old := CommandArgs("customer1", "/stage/customer1", ModeSplit, Capabilities{}, true)
	if contains(old, "--skipmail") {
		t.Errorf("args = %v, a flag this pkgacct does not have was passed", old)
	}
}

// helpLive136 is what cPanel 136.0.38 prints, copied from a running
// server. Both spellings appear: the summary column and the long form.
const helpLive136 = `Usage: pkgacct [options] user [dir]
         --nocompress                  do not compress
         --skiphomedir                 exclude the home directory
         --skipdb                      exclude databases
         --skipmail                    exclude mail
         --skipmailconfig              exclude mail configuration
         --skipmailman                 exclude mailing lists

    --skipmail
        Exclude the account's mail directory from the archive.
    --skipmailconfig
        Exclude the account's mail configuration information from the
        archive.
`

// TestSkipEmailAlsoLeavesOutTheMailConfiguration covers what cPanel means
// by the two flags.
//
// --skipmail leaves out the mail directory: the messages. The mail
// accounts and their password hashes are not in there -- they are in the
// mail configuration, which is --skipmailconfig. A schedule set to leave
// email out was passing only the first, so every mailbox password on the
// account went into an archive taken deliberately without email.
func TestSkipEmailAlsoLeavesOutTheMailConfiguration(t *testing.T) {
	caps := ProbeCapabilities(helpLive136)
	if caps.SkipMailFlag != "--skipmail" {
		t.Errorf("SkipMailFlag = %q", caps.SkipMailFlag)
	}
	if caps.SkipMailConfigFlag != "--skipmailconfig" {
		t.Errorf("SkipMailConfigFlag = %q -- the mailbox password hashes are in "+
			"the configuration, and this is the flag that leaves it out",
			caps.SkipMailConfigFlag)
	}

	args := strings.Join(CommandArgs("customer1", "/var/cprest/staging/job-42",
		ModeSplit, caps, true), " ")
	for _, want := range []string{"--skipmail", "--skipmailconfig"} {
		if !strings.Contains(args, want) {
			t.Errorf("pkgacct %s does not pass %s", args, want)
		}
	}

	// A schedule that keeps email passes neither.
	kept := strings.Join(CommandArgs("customer1", "/var/cprest/staging/job-42",
		ModeSplit, caps, false), " ")
	if strings.Contains(kept, "skipmail") {
		t.Errorf("a backup that keeps email asked pkgacct to leave it out: %s", kept)
	}

	// A cPanel too old to know the flag is not asked for it, rather than
	// being handed an argument it will reject.
	older := ProbeCapabilities(helpModern)
	if older.SkipMailConfigFlag != "" {
		t.Errorf("SkipMailConfigFlag = %q for a version without it", older.SkipMailConfigFlag)
	}
	if strings.Contains(strings.Join(CommandArgs("customer1", "/tmp/x",
		ModeSplit, older, true), " "), "--skipmailconfig") {
		t.Error("a flag the host does not support was passed anyway")
	}
}
