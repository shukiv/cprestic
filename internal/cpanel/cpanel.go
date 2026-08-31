// Package cpanel stages the files an account's backup consists of.
//
// The work is behind an interface so the agent can be exercised end to end
// on a machine with no cPanel installation: the fake provider builds a
// synthetic account tree with the same shape the real one produces.
package cpanel

import (
	"context"

	"github.com/shuki/cprest/internal/pkgacct"
)

// AccountInfo is what the provider knows about an account.
//
// Accounts fills in only User and HomeDir, because measuring a size means
// walking a home directory and listing databases means a MySQL round trip.
// Account fills in everything, and is called when a backup is about to run.
type AccountInfo struct {
	User      string
	HomeDir   string
	Databases []string
	// SizeBytes drives the staging space preflight. Zero means it has not
	// been measured, not that the account is empty.
	SizeBytes uint64
	// PrimaryDomain is shown in listings where it is known.
	PrimaryDomain string
}

// StageRequest asks a provider to materialise one account's payload.
type StageRequest struct {
	Account    AccountInfo
	StagingDir string
	Mode       pkgacct.Mode
	// SkipHomedir and SkipDatabases leave those parts out of the payload
	// entirely, for a schedule that says so. The account's configuration
	// always travels: it is one archive that restorepkg needs, and
	// pkgacct has no switch that takes pieces out of it.
	SkipHomedir   bool
	SkipDatabases bool
}

// Provider produces backup payloads for cPanel accounts.
type Provider interface {
	// Capabilities reports which pkgacct flags this host supports.
	Capabilities(ctx context.Context) (pkgacct.Capabilities, error)

	// Accounts lists every cPanel account on the host. Standalone mode
	// uses it to resolve a policy that covers "all accounts" at run time,
	// so accounts added later are picked up without an edit.
	Accounts(ctx context.Context) ([]AccountInfo, error)

	// Account looks up an account's home directory and databases.
	Account(ctx context.Context, user string) (AccountInfo, error)

	// Stage writes the payload into StagingDir and returns what was
	// produced. The returned payload reports Degraded when the host
	// forced a layout that deduplicates poorly.
	Stage(ctx context.Context, req StageRequest) (pkgacct.Payload, error)

	// StageSystem materialises the server's own configuration: what a
	// replacement machine needs before the accounts restored onto it mean
	// anything.
	StageSystem(ctx context.Context, stagingDir string) (pkgacct.Payload, error)

	// Apply hands a rebuilt account archive to cPanel, overwriting the
	// live account. Callers must only reach this when an operator has
	// explicitly asked for it.
	Apply(ctx context.Context, archivePath string) error
}
