package inventory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Auth is what authenticates one database user on one host: the password
// as MySQL stores it, and the plugin that reads it.
type Auth struct {
	// Hash is the stored password, hex-encoded. cPanel writes it this way
	// because caching_sha2_password's is binary.
	Hash string `json:"pass_hash"`
	// Plugin is MySQL's authentication plugin for this user.
	Plugin string `json:"auth_plugin"`
}

// Grant is one database a user could open, and what it could do there.
type Grant struct {
	Database   string
	Privileges []string
}

// ParseAuth reads the file that carries each user's stored password.
// cPanel's shape: user -> host -> what authenticates it.
func ParseAuth(raw []byte) (map[string]map[string]Auth, error) {
	var auth map[string]map[string]Auth
	if err := json.Unmarshal(raw, &auth); err != nil {
		return nil, fmt.Errorf("inventory: read the database users in this backup: %w", err)
	}
	return auth, nil
}

// stagedGrant matches the grant lines cprest itself writes into the
// runnable file: one database, no global privileges and nothing after the
// grantee. Anything else is refused rather than guessed at, because the
// two things that could be guessed -- dropping a privilege an application
// connects with, or widening one past the account -- are both worse than
// saying so.
var stagedGrant = regexp.MustCompile(
	"^GRANT (.+) ON `(.+)`\\.\\* TO '([^']+)'@'([^']+)'$")

// ParseGrants reads the privileges each user had, keyed by user@host.
func ParseGrants(raw []byte) (map[string][]Grant, error) {
	grants := map[string][]Grant{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		if line == "" || strings.HasPrefix(line, "--") ||
			strings.HasPrefix(line, "CREATE USER ") {
			continue
		}
		if !strings.HasPrefix(line, "GRANT ") {
			return nil, fmt.Errorf(
				"inventory: the database users in this backup hold a line this cannot read")
		}
		parts := stagedGrant.FindStringSubmatch(line)
		if parts == nil {
			return nil, fmt.Errorf(
				"inventory: a grant in this backup is not one on a single database, " +
					"and restoring it is not something this can do")
		}
		database, literal := unescapeGrantDatabase(parts[2])
		if !literal {
			return nil, fmt.Errorf(
				"inventory: a grant in this backup is on a pattern rather than on one "+
					"database (%s), and restoring it is not something this can do",
				parts[2])
		}
		var privileges []string
		for _, privilege := range strings.Split(parts[1], ",") {
			if privilege = strings.TrimSpace(privilege); privilege != "" {
				privileges = append(privileges, privilege)
			}
		}
		who := parts[3] + "@" + parts[4]
		grants[who] = append(grants[who], Grant{
			Database: database, Privileges: privileges,
		})
	}
	return grants, nil
}

// unescapeGrantDatabase turns the database of a GRANT back into the name of
// a database.
//
// MySQL stores the database of a grant as a LIKE pattern, so SHOW GRANTS
// prints "acct\\_shop" for the database "acct_shop" -- the backslash is
// what makes the underscore mean itself rather than any character. Carrying
// that name through unchanged would ask cPanel for a database nobody has.
//
// An underscore or a per-cent that is not escaped really is a wildcard: the
// grant covers every database matching it, which is not one database and
// not something this can put back. Those are reported rather than restored
// against whichever database the pattern happens to look like.
func unescapeGrantDatabase(pattern string) (name string, literal bool) {
	var out strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			if i+1 >= len(pattern) {
				return "", false
			}
			i++
			out.WriteByte(pattern[i])
		case '_', '%':
			return "", false
		default:
			out.WriteByte(pattern[i])
		}
	}
	return out.String(), true
}
