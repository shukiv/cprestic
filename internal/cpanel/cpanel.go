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
type AccountInfo struct {
	User      string
	HomeDir   string
	Databases []string
	// SizeBytes is used for the staging space preflight.
	SizeBytes uint64
}

// StageRequest asks a provider to materialise one account's payload.
type StageRequest struct {
	Account    AccountInfo
	StagingDir string
	Mode       pkgacct.Mode
}

// Provider produces backup payloads for cPanel accounts.
type Provider interface {
	// Capabilities reports which pkgacct flags this host supports.
	Capabilities(ctx context.Context) (pkgacct.Capabilities, error)

	// Account looks up an account's home directory and databases.
	Account(ctx context.Context, user string) (AccountInfo, error)

	// Stage writes the payload into StagingDir and returns what was
	// produced. The returned payload reports Degraded when the host
	// forced a layout that deduplicates poorly.
	Stage(ctx context.Context, req StageRequest) (pkgacct.Payload, error)
}
