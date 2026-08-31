// Package granular works out what has to come out of a snapshot to restore
// one thing rather than a whole account.
//
// A restore of a single mailbox, one database or an account's DNS records
// is the common case in practice: something was deleted this morning and
// everything else on the account is fine. Rebuilding and reapplying the
// whole account to fix it would replace far more than was lost.
//
// Nothing here runs restic or touches a disk. It turns a request into the
// paths a snapshot holds, so the mapping can be tested against known
// snapshot layouts rather than against a live cPanel.
package granular

import (
	"fmt"
	"path"
	"strings"

	"github.com/shuki/cprest/internal/reassemble"
)

// Kind names a thing an operator restores on its own.
type Kind string

const (
	// KindFiles restores named paths from the home directory.
	KindFiles Kind = "files"
	// KindWebsite restores the document root.
	KindWebsite Kind = "website"
	// KindMailbox restores one mailbox, or a whole domain's mail.
	KindMailbox Kind = "mailbox"
	// KindDatabase restores one database dump.
	KindDatabase Kind = "database"
	// KindDNS restores the account's zone files.
	KindDNS Kind = "dns"
	// KindSSL restores its certificates and keys.
	KindSSL Kind = "ssl"
	// KindSettings restores the cPanel configuration of the account.
	KindSettings Kind = "settings"
	// KindCron restores the account's cron jobs.
	KindCron Kind = "cron"
	// KindDomains restores its domains and their web server configuration.
	KindDomains Kind = "domains"
	// KindFTP restores its FTP accounts.
	KindFTP Kind = "ftp"
	// KindDBUsers restores the database users and their grants. A database
	// without the user that owns it is a database no site can open.
	KindDBUsers Kind = "dbusers"
)

// Kinds is every kind, in the order the interface offers them.
var Kinds = []Kind{
	KindFiles, KindWebsite, KindMailbox, KindDatabase, KindDBUsers,
	KindDNS, KindDomains, KindSSL, KindCron, KindFTP, KindSettings,
}

// Title is the kind as the interface names it.
func (k Kind) Title() string {
	switch k {
	case KindFiles:
		return "Files or folders"
	case KindWebsite:
		return "Website files"
	case KindMailbox:
		return "A mailbox"
	case KindDatabase:
		return "A database"
	case KindDNS:
		return "DNS records"
	case KindSSL:
		return "SSL certificates"
	case KindSettings:
		return "Panel configuration"
	case KindCron:
		return "Cron jobs"
	case KindDomains:
		return "Domains"
	case KindFTP:
		return "FTP accounts"
	case KindDBUsers:
		return "Database users"
	}
	return string(k)
}

// NeedsNames reports whether a kind is meaningless without the operator
// naming what they want.
func (k Kind) NeedsNames() bool {
	switch k {
	case KindFiles, KindMailbox, KindDatabase:
		return true
	}
	return false
}

// Request is one granular restore.
type Request struct {
	Kind    Kind
	Account string
	// Names are the paths, mailboxes or databases asked for. Which of
	// those it means depends on Kind, and some kinds need none.
	Names []string
}

// Plan is everything that has to come out of a snapshot to satisfy a
// request.
type Plan struct {
	// Include are snapshot paths handed to restic.
	Include []string
	// Members are prefixes inside the account's metadata archive to keep.
	// Empty when the request needs nothing from it.
	Members []string
	// Metadata is the snapshot path of the metadata part, set only when
	// Members is non-empty.
	Metadata string
	// Describes the restore in the words the operator chose it by.
	Description string
}

// Members inside a cpmove archive, verified against cPanel 136.0.37. They
// are cPanel's names, not ours, and a snapshot from a different version may
// not carry all of them — a plan asks for what it wants and the extraction
// reports what it actually found.
var (
	dnsMembers = []string{"dnszones/"}
	sslMembers = []string{
		"apache_tls/", "ssl/", "sslcerts/", "sslkeys/",
		"has_sslstorage", "autossl.json",
	}
	settingsMembers = []string{
		"cp/", "meta/", "quota", "shell", "shadow", "digestshadow",
		"userconfig/", "version", "packaged_in_version",
	}
	cronMembers   = []string{"cron/"}
	ftpMembers    = []string{"proftpdpasswd"}
	domainMembers = []string{"userdata/", "dnszones/", "ips/", "addons"}
	// A mailbox is not only its maildir: forwarders, filters and the
	// domain's mail configuration live in the metadata archive.
	mailMembers = []string{"va/", "vad/", "vf/", "meta/mailserver"}
)

// Build turns a request into the paths that satisfy it.
//
// It fails rather than returning an empty plan: a granular restore that
// quietly asks for nothing would report success having produced nothing,
// which is the failure this program most has to avoid.
func Build(parts reassemble.Parts, req Request) (Plan, error) {
	if req.Account == "" {
		return Plan{}, fmt.Errorf("granular: account is required")
	}
	if req.Kind.NeedsNames() && len(req.Names) == 0 {
		return Plan{}, fmt.Errorf("granular: %s needs at least one name", req.Kind)
	}

	switch req.Kind {
	case KindFiles:
		return buildFiles(parts, req)
	case KindWebsite:
		return buildHomedir(parts, req, []string{"public_html"}, "the website files")
	case KindMailbox:
		return buildMailbox(parts, req)
	case KindDatabase:
		return buildDatabase(parts, req)
	case KindDNS:
		return buildMetadata(parts, dnsMembers, "the DNS records")
	case KindSSL:
		return buildMetadata(parts, sslMembers, "the SSL certificates and keys")
	case KindSettings:
		return buildMetadata(parts, settingsMembers, "the panel configuration")
	case KindCron:
		return buildMetadata(parts, cronMembers, "the cron jobs")
	case KindDomains:
		return buildMetadata(parts, domainMembers, "the domains")
	case KindFTP:
		return buildMetadata(parts, ftpMembers, "the FTP accounts")
	case KindDBUsers:
		return buildDatabaseUsers(parts)
	default:
		return Plan{}, fmt.Errorf("granular: unknown restore kind %q", req.Kind)
	}
}

func buildFiles(parts reassemble.Parts, req Request) (Plan, error) {
	if parts.Homedir == "" {
		return Plan{}, fmt.Errorf("granular: this snapshot has no home directory in it")
	}
	plan := Plan{Description: "the named files"}
	for _, name := range req.Names {
		full, err := underHome(parts.Homedir, name)
		if err != nil {
			return Plan{}, err
		}
		plan.Include = append(plan.Include, full)
	}
	return plan, nil
}

func buildHomedir(parts reassemble.Parts, req Request, relative []string, description string) (Plan, error) {
	if parts.Homedir == "" {
		return Plan{}, fmt.Errorf("granular: this snapshot has no home directory in it")
	}
	plan := Plan{Description: description}
	for _, name := range relative {
		full, err := underHome(parts.Homedir, name)
		if err != nil {
			return Plan{}, err
		}
		plan.Include = append(plan.Include, full)
	}
	return plan, nil
}

func buildMailbox(parts reassemble.Parts, req Request) (Plan, error) {
	names := make([]string, 0, len(req.Names))
	for _, name := range req.Names {
		names = append(names, path.Join("mail", name))
	}
	plan, err := buildHomedir(parts, req, names,
		"the mail for "+strings.Join(req.Names, ", "))
	if err != nil {
		return Plan{}, err
	}
	// Forwarders and filters are configuration, not maildir, so they only
	// come back if the metadata part is in the snapshot too.
	if parts.Metadata != "" {
		plan.Metadata = parts.Metadata
		plan.Members = mailMembers
		plan.Include = append(plan.Include, parts.Metadata)
	}
	return plan, nil
}

func buildDatabase(parts reassemble.Parts, req Request) (Plan, error) {
	if parts.Databases == "" {
		return Plan{}, fmt.Errorf(
			"granular: this snapshot holds no separate database dumps, " +
				"so a single database cannot be taken out of it")
	}
	plan := Plan{Description: "the database " + strings.Join(req.Names, ", ")}
	for _, name := range req.Names {
		if err := plainName(name); err != nil {
			return Plan{}, err
		}
		plan.Include = append(plan.Include, path.Join(parts.Databases, name+".sql"))
	}
	return plan, nil
}

// DatabaseUsersFile is where the account's database users and their
// grants are staged, inside the same directory as the dumps so a
// snapshot's paths do not change when an account gains or loses one.
const DatabaseUsersFile = "_users.sql"

func buildDatabaseUsers(parts reassemble.Parts) (Plan, error) {
	if parts.Databases == "" {
		return Plan{}, fmt.Errorf(
			"granular: this backup holds no databases, so it holds no database users either")
	}
	return Plan{
		Include:     []string{path.Join(parts.Databases, DatabaseUsersFile)},
		Description: "the database users and their grants",
	}, nil
}

func buildMetadata(parts reassemble.Parts, members []string, description string) (Plan, error) {
	if parts.Metadata == "" {
		return Plan{}, fmt.Errorf(
			"granular: this snapshot has no account metadata in it, "+
				"so %s cannot be taken out of it", description)
	}
	return Plan{
		Include:     []string{parts.Metadata},
		Members:     members,
		Metadata:    parts.Metadata,
		Description: description,
	}, nil
}

// underHome resolves a name inside the account's home directory.
//
// An absolute path is accepted only if it is already inside that home
// directory: every account on a cPanel server is a different customer, and
// a restore that could be steered at /home/someone-else would hand one
// customer's files to another.
func underHome(home, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("granular: empty path")
	}
	full := name
	if !strings.HasPrefix(name, "/") {
		full = path.Join(home, name)
	}
	full = path.Clean(full)
	if full != home && !strings.HasPrefix(full, home+"/") {
		return "", fmt.Errorf("granular: %s is not inside %s", name, home)
	}
	return full, nil
}

// plainName rejects anything that is not simply a name, so a database
// cannot be spelt as a path.
func plainName(name string) error {
	if name == "" {
		return fmt.Errorf("granular: empty name")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("granular: %q is not a database name", name)
	}
	return nil
}
