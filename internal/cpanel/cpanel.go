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
// ApplyOptions are the choices an operator makes about handing an archive
// to cPanel's own restore.
type ApplyOptions struct {
	// Unrestricted turns off cPanel's Restricted Restore.
	//
	// Restricted is the default here, and cPanel's is the opposite. The
	// archive contains a customer's home directory, which the customer
	// controls, and restorepkg runs as root: unrestricted mode will
	// follow what it finds in there. Restricted mode refuses some
	// legitimate content too, which is why this is a choice rather than
	// a rule.
	Unrestricted bool
	// Overwrite restores over an account that is already on this server.
	// Without it restorepkg will not replace what is there, and a restore
	// the interface promised would overwrite the account quietly did not.
	Overwrite bool
	// NewUser asks restorepkg to create the archive under a disposable
	// username. It is used only by live restore certification.
	NewUser string
	// SkipDNS prevents a certification restore from changing production
	// DNS zones.
	SkipDNS bool
}

// Certifier is implemented by a real cPanel provider that can prove an
// archive is accepted by restorepkg on an isolated certification host.
type Certifier interface {
	Certify(ctx context.Context, archivePath, disposableUser string) error
}

type AccountInfo struct {
	User      string
	HomeDir   string
	Databases []string
	// HasPostgreSQL is true when cPanel's database map records PostgreSQL
	// data for this account, or when that map cannot reliably rule it out.
	// Split payloads only dump MySQL themselves, so PostgreSQL requires the
	// complete pkgacct archive to avoid a successful backup with missing data.
	HasPostgreSQL bool
	// SizeBytes drives the staging space preflight. Zero means it has not
	// been measured, not that the account is empty.
	SizeBytes uint64
	// PrimaryDomain is shown in listings where it is known.
	PrimaryDomain string
	// Missing says cPanel knows about this account but its home directory
	// is not there. Such an account is still listed, because an account
	// left off the page is an account nobody notices is not being backed
	// up; its backup will fail, loudly, which is the point.
	Missing bool
}

// StageRequest asks a provider to materialise one account's payload.
type StageRequest struct {
	Account    AccountInfo
	StagingDir string
	Mode       pkgacct.Mode
	// SkipHomedir and SkipDatabases leave those parts out of the payload
	// entirely, for a schedule that says so.
	SkipHomedir   bool
	SkipDatabases bool
	// SkipEmail leaves the account's mail out. It reaches pkgacct as well
	// as restic: excluding ~/mail keeps the messages out of the file
	// backup, but the mail configuration -- the account names, and the
	// hashes that go with them -- is packed inside pkgacct's own archive,
	// where no restic exclude can reach it.
	SkipEmail bool
}

// Provider produces backup payloads for cPanel accounts.
type Provider interface {
	// NativeExcludes is what cPanel's own backups would leave out of
	// this account: the server-wide cpbackup-exclude.conf and the
	// account's own. An operator who wrote a path in there has said it
	// must not leave the server.
	NativeExcludes(home string) []string

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
	Apply(ctx context.Context, archivePath string, options ApplyOptions) error

	// PutHomeDir copies a restored subtree back into an account's home
	// directory, as that account.
	//
	// The tree comes out of a root-owned staging directory, which the
	// account cannot read; what lands in the home directory is written by
	// the account itself, so it is owned by them and can reach nothing
	// they could not already reach. The caller passes the tree rooted
	// where the home directory was: what is under from/public_html lands
	// in the account's own public_html.
	PutHomeDir(ctx context.Context, user, from string) error

	// LoadDatabase replaces the contents of one of the account's databases
	// with a dump taken from a backup.
	//
	// The database must already exist and must belong to the account. It
	// is not created here: creating one is a cPanel operation, with a
	// quota and a database map behind it, and a restore that quietly made
	// one would leave the panel not knowing about it.
	LoadDatabase(ctx context.Context, user, database, dumpPath string) error
}
