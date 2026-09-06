// Package inventory says what a backup holds in the parts of an account
// that are not files.
//
// A snapshot's files can be listed by restic itself, so a page can offer
// the one that was lost. Everything else about an account -- its DNS
// zones, its certificates, its cron jobs, its FTP logins, its database
// users -- is inside a single archive or a single SQL file, and restic can
// only say that the container is there. A page built on that alone tells
// somebody "your DNS records are in this backup" and cannot answer the
// question they actually have, which is whether the zone they deleted this
// morning is one of them.
//
// This package opens those containers and reads out the names. Nothing is
// restored to do it: the container is streamed out of the repository and
// read as it arrives.
package inventory

import (
	"context"
	"io"

	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
)

// Item is one thing in a backup that a person can point at.
type Item struct {
	// Name is what a restore asks for. Empty when this kind is listed to
	// be read rather than picked from.
	Name string
	// Label is what the page calls it.
	Label string
	// Detail is a second line, for when the name alone does not say what
	// the thing is.
	Detail string
}

// Reader is the part of a restic runner this package uses.
type Reader interface {
	Dump(ctx context.Context, repo resticrun.Repository, snapshotID, path string, out io.Writer) error
	Ls(ctx context.Context, repo resticrun.Repository, snapshotID string, subpaths ...string) ([]resticrun.Entry, error)
}

// Source is one snapshot to read.
type Source struct {
	// Key tells one repository from another in the cache. The repository's
	// identifier is what a caller has; nothing here interprets it.
	Key        string
	Repo       resticrun.Repository
	SnapshotID string
	Parts      reassemble.Parts
}

// maxHeaders bounds how many members of an archive are read. An archive is
// only as trustworthy as the server that produced it, and one built to run
// forever would otherwise hold a page open forever with it.
const maxHeaders = 20000

// maxBody bounds a member read for its contents. The members read whole
// here are a crontab and a password file; a megabyte is far past either.
const maxBody = 1 << 20
