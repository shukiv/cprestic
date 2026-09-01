package cpanel

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
)

// This file reads what cPanel itself records about an account, rather than
// reconstructing it from the account's name.
//
// Both conventions this replaces are true on most servers and wrong on
// some, and wrong in the two ways that matter: an account whose home was
// moved to another partition disappeared from the backup list without an
// error, and a database whose name did not start with its owner's name
// was either missed or attributed to whoever else's name it did start
// with.

// homeFor is the account's home directory as the system records it.
//
// cPanel can move an account to another partition — /home2, /home3, a
// separate mount — and the account's name then says nothing about where
// it lives. Reading it from the password database is what cPanel's own
// tooling does, and it is right whatever the layout.
func (r *Real) homeFor(name string) string {
	if r.HomeRoot != "" {
		// A test or a development server pointing the whole thing
		// somewhere else.
		return filepath.Join(r.HomeRoot, name)
	}
	if account, err := user.Lookup(name); err == nil && account.HomeDir != "" {
		return account.HomeDir
	}
	return filepath.Join("/home", name)
}

// suspended reports whether cPanel has this account suspended.
//
// A suspended account is one whose owner has been cut off, often for
// non-payment and sometimes for abuse. Its files are still there and are
// still worth backing up; what it must not do is reach in and drive this
// service.
func (r *Real) suspended(name string) bool {
	if _, err := os.Stat(filepath.Join(r.suspendedDir(), name)); err == nil {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(r.usersDir(), name))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "SUSPENDED=1" {
			return true
		}
	}
	return false
}

func (r *Real) suspendedDir() string {
	if r.SuspendedDir != "" {
		return r.SuspendedDir
	}
	return "/var/cpanel/suspended"
}

// postgresPaths are where a cPanel server keeps PostgreSQL if it has it.
var postgresPaths = []string{
	"/usr/local/cpanel/3rdparty/bin/psql",
	"/var/lib/pgsql",
	"/usr/bin/psql",
}

// postgresInstalled reports whether this server has PostgreSQL at all.
//
// It is the fallback for an account whose database map cannot be read. A
// server without PostgreSQL has no account with PostgreSQL databases, and
// assuming otherwise costs a full copy of that account every night.
func (r *Real) postgresInstalled() bool {
	paths := postgresPaths
	if len(r.PostgresPaths) > 0 {
		paths = r.PostgresPaths
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func (r *Real) databasesDir() string {
	if r.DatabasesDir != "" {
		return r.DatabasesDir
	}
	return "/var/cpanel/databases"
}

// databaseMap is the part of cPanel's own record of an account's databases
// that this program needs. cPanel keeps one of these per account in
// /var/cpanel/databases, and it is authoritative: it holds databases that
// do not carry the account's prefix, which a server with prefixing
// disabled is full of.
type databaseMap struct {
	// A pointer so an absent MYSQL section can be told apart from an
	// account that genuinely has no databases. They are not the same
	// thing: the first means this file does not answer the question, and
	// treating it as "no databases" drops an account's data out of its
	// backups without anything failing.
	MySQL *struct {
		Owner    string                     `json:"owner"`
		DBs      map[string]json.RawMessage `json:"dbs"`
		DBUsers  map[string]json.RawMessage `json:"dbusers"`
		NoPrefix map[string]json.RawMessage `json:"noprefix"`
	} `json:"MYSQL"`
}

// recordedDatabases reads what cPanel says this account owns.
//
// The second return value says whether cPanel had a record at all. An
// absent file is not an account with no databases — it is an older cPanel,
// or one that has never written the map — and the caller falls back to the
// naming convention rather than backing up nothing.
func (r *Real) recordedDatabases(name string) (databases, users []string, recorded bool) {
	raw, err := os.ReadFile(filepath.Join(r.databasesDir(), name+".json"))
	if err != nil {
		return nil, nil, false
	}
	var recordedMap databaseMap
	if err := json.Unmarshal(raw, &recordedMap); err != nil {
		return nil, nil, false
	}
	if recordedMap.MySQL == nil || recordedMap.MySQL.Owner == "" {
		// A file that does not say whose databases these are does not
		// answer the question. Fall back rather than back up nothing.
		return nil, nil, false
	}
	// An account whose map names a different owner is not this account's
	// map. Trusting it would hand one customer another's databases.
	if recordedMap.MySQL.Owner != name {
		return nil, nil, false
	}
	for database := range recordedMap.MySQL.DBs {
		if database != "" {
			databases = append(databases, database)
		}
	}
	for database := range recordedMap.MySQL.NoPrefix {
		if database != "" {
			databases = append(databases, database)
		}
	}
	for dbUser := range recordedMap.MySQL.DBUsers {
		if dbUser != "" {
			users = append(users, dbUser)
		}
	}
	sort.Strings(databases)
	sort.Strings(users)
	return unique(databases), unique(users), true
}

// recordedPostgreSQL reports whether cPanel's per-account database map
// contains PostgreSQL databases. The second result is false when the map
// cannot answer safely; callers must not turn that uncertainty into "none".
//
// cPanel has used both a direct PGSQL database map and a section with dbs and
// noprefix maps, so both shapes are accepted.
func (r *Real) recordedPostgreSQL(name string) (present, recorded bool) {
	raw, err := os.ReadFile(filepath.Join(r.databasesDir(), name+".json"))
	if err != nil {
		return false, false
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sections); err != nil {
		return false, false
	}
	rawPGSQL, found := sections["PGSQL"]
	if !found || string(rawPGSQL) == "null" {
		// A map that answers about MySQL is a map cPanel wrote and keeps
		// current, and cPanel writes a PGSQL section when the account
		// has PostgreSQL databases. Its absence from such a map is an
		// answer -- "none" -- not a gap. Reading it as a gap made every
		// account on a server with no PostgreSQL fall back to the
		// single-archive payload, which stores a full copy of every
		// account every night.
		if _, answered := sections["MYSQL"]; answered {
			return false, true
		}
		return false, false
	}
	var pgsql map[string]json.RawMessage
	if err := json.Unmarshal(rawPGSQL, &pgsql); err != nil {
		return false, false
	}
	for _, key := range []string{"dbs", "noprefix"} {
		if rawDatabases, found := pgsql[key]; found {
			var databases map[string]json.RawMessage
			if err := json.Unmarshal(rawDatabases, &databases); err != nil {
				return false, false
			}
			if len(databases) > 0 {
				return true, true
			}
		}
	}
	for key := range pgsql {
		switch key {
		case "owner", "server", "dbs", "dbusers", "noprefix":
			continue
		default:
			// Older maps put database names directly below PGSQL.
			return true, true
		}
	}
	return false, true
}

// ownedDatabases is every database cPanel attributes to an account,
// whoever else's name it happens to start with.
//
// /etc/dbowners is the server-wide answer to the same question and is
// used to check the fallback: a database called "alice_shop" that
// dbowners attributes to bob is bob's, and backing it up as alice's would
// put one customer's data, and the grants that read it, into another
// customer's backup.
func (r *Real) ownedDatabases(name string, candidates []string) []string {
	owners, err := readDBOwners(r.dbOwnersPath())
	if err != nil {
		return candidates
	}
	kept := make([]string, 0, len(candidates))
	for _, database := range candidates {
		if owner, known := owners[database]; known && owner != name {
			continue
		}
		kept = append(kept, database)
	}
	return kept
}

func (r *Real) dbOwnersPath() string {
	if r.DBOwnersPath != "" {
		return r.DBOwnersPath
	}
	return "/etc/dbowners"
}

// readDBOwners parses cPanel's database-to-owner map. It is a plain
// "database: owner" list with a version comment at the top.
func readDBOwners(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		database, owner, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		database, owner = strings.TrimSpace(database), strings.TrimSpace(owner)
		if database != "" && owner != "" {
			owners[database] = owner
		}
	}
	if len(owners) == 0 {
		return nil, fmt.Errorf("cpanel: %s named no databases", path)
	}
	return owners, nil
}

func unique(values []string) []string {
	if len(values) < 2 {
		return values
	}
	kept := values[:1]
	for _, value := range values[1:] {
		if value != kept[len(kept)-1] {
			kept = append(kept, value)
		}
	}
	return kept
}

// recordedDatabaseUsers is the database users cPanel attributes to an
// account.
func (r *Real) recordedDatabaseUsers(name string) ([]string, bool) {
	_, users, recorded := r.recordedDatabases(name)
	if !recorded {
		return nil, false
	}
	// The account is a database user on its own databases on most
	// servers, whether or not the record lists it.
	for _, user := range users {
		if user == name {
			return users, true
		}
	}
	return append([]string{name}, users...), true
}
