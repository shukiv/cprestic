package inventory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/shukiv/gniza/internal/granular"
)

// itemsOf turns what was read out of a snapshot into what the page lists.
func itemsOf(kind granular.Kind, held *contents) ([]Item, error) {
	switch kind {
	case granular.KindDNS:
		return underPrefix(held.members, "dnszones/", ".db"), nil
	case granular.KindSSL:
		return underPrefix(held.members, "apache_tls/", ""), nil
	case granular.KindDomains:
		return domainItems(held.members), nil
	case granular.KindCron:
		return cronItems(held.bodies), nil
	case granular.KindFTP:
		return ftpItems(held.bodies), nil
	case granular.KindDBUsers:
		return databaseUserItems(held.auth, held.grants), nil
	}
	return nil, fmt.Errorf("inventory: a backup cannot be asked what %s it holds", kind)
}

// underPrefix lists the files directly under one directory of the archive,
// with a suffix taken off the name when the file carries one.
func underPrefix(members []string, prefix, suffix string) []Item {
	var items []Item
	for _, member := range members {
		if !strings.HasPrefix(member, prefix) {
			continue
		}
		name := strings.TrimPrefix(member, prefix)
		// A directory entry, or something in a directory below this one.
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if suffix != "" {
			if !strings.HasSuffix(name, suffix) {
				continue
			}
			name = strings.TrimSuffix(name, suffix)
		}
		if granular.UsableDomainName(name) != nil {
			continue
		}
		items = append(items, Item{Name: name, Label: name})
	}
	return sorted(items)
}

// domainItems lists the account's domains from their web server
// configuration. cPanel writes one file per domain in userdata, beside
// per-domain files for TLS and PHP that are not themselves domains.
func domainItems(members []string) []Item {
	var items []Item
	for _, member := range members {
		name := strings.TrimPrefix(member, "userdata/")
		if name == member || name == "" || strings.Contains(name, "/") {
			continue
		}
		if strings.HasSuffix(name, "_SSL") || strings.HasSuffix(name, ".php-fpm.yaml") ||
			strings.HasSuffix(name, ".cache") || name == "main" || name == "cache.json" {
			continue
		}
		if granular.UsableDomainName(name) != nil {
			continue
		}
		items = append(items, Item{Name: name, Label: name})
	}
	return sorted(items)
}

// cronItems lists the account's scheduled commands.
//
// They have no name: a crontab is lines, and one line is told from another
// by what it runs. They are listed to be read rather than to be picked
// from, because taking some lines out of a crontab would hand back a file
// that is not the one the backup holds.
func cronItems(bodies map[string][]byte) []Item {
	var items []Item
	for member, body := range bodies {
		if !strings.HasPrefix(member, "cron/") {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// SHELL, MAILTO and the like configure the crontab rather
			// than being jobs in it.
			if cronSetting.MatchString(line) {
				continue
			}
			if schedule, command, found := splitCron(line); found {
				items = append(items, Item{Label: command, Detail: schedule})
				continue
			}
			// A line this cannot take apart is still one of the account's
			// cron jobs, and showing it whole says more than leaving it
			// out would.
			items = append(items, Item{Label: line})
		}
	}
	return items
}

// cronSetting matches the lines that configure a crontab rather than being
// jobs in it: SHELL, MAILTO, PATH and anything else cron reads as an
// environment variable.
var cronSetting = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*=`)

// splitCron separates a cron line's schedule from what it runs, so a page
// can show the command rather than five columns of numbers first.
func splitCron(line string) (schedule, command string, ok bool) {
	fields := strings.Fields(line)
	// cron's own shorthand for a schedule: "@daily", "@reboot".
	if len(fields) >= 2 && strings.HasPrefix(fields[0], "@") {
		return fields[0], strings.Join(fields[1:], " "), true
	}
	if len(fields) < 6 {
		return "", "", false
	}
	for _, field := range fields[:5] {
		if strings.ContainsAny(field, "=\"'") {
			return "", "", false
		}
	}
	return strings.Join(fields[:5], " "),
		strings.Join(fields[5:], " "), true
}

// ftpItems lists the account's FTP logins by name.
//
// The file they come from is a password file, and every line of it carries
// a hash. Only the first field is read: nothing else on the line has any
// business on a page.
func ftpItems(bodies map[string][]byte) []Item {
	var items []Item
	for _, line := range strings.Split(string(bodies["proftpdpasswd"]), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 6 || fields[0] == "" {
			continue
		}
		items = append(items, Item{Label: fields[0], Detail: fields[5]})
	}
	return sorted(items)
}

// databaseUserItems lists the account's database users, and says which of
// its databases each one could open. A user's password is in the backup and
// stays there: what a page needs to show is who existed.
func databaseUserItems(auth map[string]map[string]Auth, grants map[string][]Grant) []Item {
	var items []Item
	for user, hosts := range auth {
		if granular.UsableDatabaseUserName(user) != nil {
			continue
		}
		var databases []string
		for host := range hosts {
			for _, grant := range grants[user+"@"+host] {
				if !has(databases, grant.Database) {
					databases = append(databases, grant.Database)
				}
			}
		}
		sort.Strings(databases)
		item := Item{Name: user, Label: user}
		switch len(databases) {
		case 0:
			item.Detail = "no databases"
		default:
			item.Detail = "can open " + granular.JoinAnd(databases)
		}
		items = append(items, item)
	}
	return sorted(items)
}

func has(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sorted(items []Item) []Item {
	sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
	return items
}
