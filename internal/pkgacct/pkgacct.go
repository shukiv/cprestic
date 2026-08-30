// Package pkgacct plans the payload an agent stages for one cPanel account.
//
// The payload strategy decides how well restic can deduplicate, and is the
// most consequential choice in the system: a compressed pkgacct archive has
// no stable chunk boundaries between runs, so restic stores close to the
// full archive every night, in every destination. See docs/DESIGN.md §4.
package pkgacct

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode selects how an account is decomposed into a payload.
type Mode string

const (
	// ModeSplit backs up account metadata, the home directory and each
	// database separately, so restic sees real files and deduplicates
	// normally. This is the default.
	ModeSplit Mode = "split"
	// ModeMonolithic backs up one uncompressed pkgacct archive. It keeps
	// cPanel's own archive layout intact at a large storage cost, and is
	// only sound when pkgacct compression can actually be disabled.
	ModeMonolithic Mode = "monolithic"
)

// PartKind labels a component of a staged payload.
type PartKind string

const (
	PartMetadata PartKind = "metadata"
	PartHomedir  PartKind = "homedir"
	PartDatabase PartKind = "database"
	PartArchive  PartKind = "archive"
)

// Part is one staged path handed to restic.
type Part struct {
	Kind PartKind
	Path string
}

// Payload is everything restic backs up for one account in one job.
type Payload struct {
	Mode    Mode
	Account string
	Parts   []Part
	// DumpPaths is where each database dump is written. They live inside
	// the database part's directory, which is what restic is pointed at:
	// naming the directory rather than each file keeps a snapshot's paths
	// identical when an account gains or loses a database, and paths that
	// change would put every run in its own retention group.
	DumpPaths map[string]string
	// Degraded is set when the payload had to be built in a way that
	// deduplicates poorly, with Reason explaining why. The controller
	// surfaces this rather than letting storage cost silently balloon.
	Degraded bool
	Reason   string
}

// Verify checks that every part a payload promised is actually on disk.
//
// This exists because of a real failure: pkgacct wrote nothing where it was
// told to, restic warned that it could not read the path and carried on,
// and the result was a snapshot holding the home directory and none of the
// account's configuration — reported as a success. A backup missing a part
// is not a backup, so it has to fail here rather than later.
func (p Payload) Verify() error {
	var missing []string
	for _, part := range p.Parts {
		info, err := os.Stat(part.Path)
		switch {
		case err != nil:
			missing = append(missing, fmt.Sprintf("%s (%s) is missing", part.Kind, part.Path))
		case info.IsDir():
			entries, err := os.ReadDir(part.Path)
			if err != nil || len(entries) == 0 {
				missing = append(missing, fmt.Sprintf("%s (%s) is empty", part.Kind, part.Path))
			}
		case info.Size() == 0:
			missing = append(missing, fmt.Sprintf("%s (%s) is empty", part.Kind, part.Path))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("pkgacct: the staged payload is incomplete: %s",
			strings.Join(missing, "; "))
	}
	return nil
}

// Paths returns the staged paths in payload order.
func (p Payload) Paths() []string {
	paths := make([]string, 0, len(p.Parts))
	for _, part := range p.Parts {
		paths = append(paths, part.Path)
	}
	return paths
}

// Capabilities records which pkgacct flags a given cPanel version accepts.
//
// Flag names have moved between cPanel releases, so the agent probes the
// installed binary at enrolment instead of assuming. An empty field means
// the flag was not found in the help output.
type Capabilities struct {
	NoCompressFlag  string
	SkipHomedirFlag string
	SkipDBFlag      string
}

// knownFlags maps the capability we need to the flag spellings observed
// across cPanel versions, most current first.
var knownFlags = map[string][]string{
	"nocompress":  {"--nocompress", "--uncompressed"},
	"skiphomedir": {"--skiphomedir", "--skiphome"},
	"skipdb":      {"--skipdb", "--skipmysql"},
}

// ProbeCapabilities parses the output of "pkgacct --help".
//
// Parsing help text is unlovely but beats hardcoding flags for a binary we
// do not ship and cannot version-pin.
func ProbeCapabilities(helpOutput string) Capabilities {
	find := func(key string) string {
		for _, flag := range knownFlags[key] {
			// Match the flag as a whole token so "--skiphome" does not
			// match inside "--skiphomedir".
			for _, field := range strings.FieldsFunc(helpOutput, isFlagSeparator) {
				if field == strings.TrimPrefix(flag, "--") {
					return flag
				}
			}
		}
		return ""
	}
	return Capabilities{
		NoCompressFlag:  find("nocompress"),
		SkipHomedirFlag: find("skiphomedir"),
		SkipDBFlag:      find("skipdb"),
	}
}

// PlanRequest describes the account to stage.
type PlanRequest struct {
	Account    string
	HomeDir    string
	Databases  []string
	StagingDir string
	Mode       Mode
	Caps       Capabilities
}

// Plan works out the payload for a request, without running anything.
//
// It reports Degraded rather than failing when the requested mode is only
// partly supported: a poorly deduplicating backup still beats no backup,
// provided the operator is told.
func Plan(req PlanRequest) (Payload, error) {
	if req.Account == "" {
		return Payload{}, fmt.Errorf("pkgacct: account is required")
	}
	if req.StagingDir == "" {
		return Payload{}, fmt.Errorf("pkgacct: staging directory is required")
	}

	switch req.Mode {
	case ModeSplit:
		return planSplit(req)
	case ModeMonolithic, "":
		return planMonolithic(req)
	default:
		return Payload{}, fmt.Errorf("pkgacct: unknown mode %q", req.Mode)
	}
}

func planSplit(req PlanRequest) (Payload, error) {
	if req.HomeDir == "" {
		return Payload{}, fmt.Errorf("pkgacct: split mode needs the account home directory")
	}
	payload := Payload{Mode: ModeSplit, Account: req.Account}
	payload.Parts = append(payload.Parts, Part{
		Kind: PartMetadata,
		Path: filepath.Join(req.StagingDir, "metadata"),
	})
	payload.Parts = append(payload.Parts, Part{Kind: PartHomedir, Path: req.HomeDir})
	if len(req.Databases) > 0 {
		databaseDir := filepath.Join(req.StagingDir, "databases")
		payload.Parts = append(payload.Parts, Part{Kind: PartDatabase, Path: databaseDir})
		payload.DumpPaths = make(map[string]string, len(req.Databases))
		for _, db := range req.Databases {
			payload.DumpPaths[db] = filepath.Join(databaseDir, db+".sql")
		}
	}
	if req.Caps.SkipHomedirFlag == "" {
		// Without a skip flag the metadata archive re-includes the home
		// directory, so it is stored twice: once as files, once inside
		// the archive.
		payload.Degraded = true
		payload.Reason = "pkgacct on this server cannot exclude the home directory; " +
			"metadata archive duplicates it"
	}
	return payload, nil
}

func planMonolithic(req PlanRequest) (Payload, error) {
	payload := Payload{
		Mode:    ModeMonolithic,
		Account: req.Account,
		Parts: []Part{{
			Kind: PartArchive,
			Path: filepath.Join(req.StagingDir, "cpmove-"+req.Account+".tar"),
		}},
	}
	if req.Caps.NoCompressFlag == "" {
		payload.Degraded = true
		payload.Reason = "pkgacct on this server cannot disable compression; " +
			"restic deduplication will be close to zero and every run stores a full copy"
		payload.Parts[0].Path += ".gz"
	}
	return payload, nil
}

func isFlagSeparator(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' ||
		r == ',' || r == '[' || r == ']' || r == '|' || r == '-' || r == '='
}

// CommandArgs builds the pkgacct invocation for a mode, using only flags
// the host was probed to support.
//
// The archive is written into the staging directory rather than pkgacct's
// default location so the agent controls where the disk fills up.
func CommandArgs(account, stagingDir string, mode Mode, caps Capabilities) []string {
	var args []string
	if caps.NoCompressFlag != "" {
		// Compression here would defeat restic's deduplication entirely.
		args = append(args, caps.NoCompressFlag)
	}
	if mode == ModeSplit {
		if caps.SkipHomedirFlag != "" {
			args = append(args, caps.SkipHomedirFlag)
		}
		if caps.SkipDBFlag != "" {
			// Databases are dumped separately, one file each, so a single
			// changed table does not rewrite the whole payload.
			args = append(args, caps.SkipDBFlag)
		}
	}
	target := stagingDir
	if mode == ModeSplit {
		target = filepath.Join(stagingDir, "metadata")
	}
	return append(args, account, target)
}
