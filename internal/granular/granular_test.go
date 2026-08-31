package granular

import (
	"strings"
	"testing"

	"github.com/shuki/cprest/internal/reassemble"
)

// A split snapshot of one account, as the agent records it.
var split = reassemble.Parts{
	Metadata:  "/var/lib/cprest/staging/stage-backup-studio/metadata",
	Homedir:   "/home/studio",
	Databases: "/var/lib/cprest/staging/stage-backup-studio/databases",
}

func TestEachKindAsksForTheRightPartOfTheSnapshot(t *testing.T) {
	for _, tc := range []struct {
		kind        Kind
		names       []string
		wantInclude []string
		wantMembers bool
	}{
		{KindWebsite, nil, []string{"/home/studio/public_html"}, false},
		{KindFiles, []string{"public_html/index.php", "/home/studio/.htaccess"},
			[]string{"/home/studio/public_html/index.php", "/home/studio/.htaccess"}, false},
		{KindMailbox, []string{"studio.co.il/sales"},
			[]string{"/home/studio/mail/studio.co.il/sales", split.Metadata}, true},
		{KindDatabase, []string{"studio_kpeh1"},
			[]string{"/var/lib/cprest/staging/stage-backup-studio/databases/studio_kpeh1.sql"}, false},
		{KindDNS, nil, []string{split.Metadata}, true},
		{KindSSL, nil, []string{split.Metadata}, true},
		{KindSettings, nil, []string{split.Metadata}, true},
	} {
		plan, err := Build(split, Request{Kind: tc.kind, Account: "studio", Names: tc.names})
		if err != nil {
			t.Errorf("%s: %v", tc.kind, err)
			continue
		}
		if strings.Join(plan.Include, "|") != strings.Join(tc.wantInclude, "|") {
			t.Errorf("%s include = %v, want %v", tc.kind, plan.Include, tc.wantInclude)
		}
		if got := len(plan.Members) > 0; got != tc.wantMembers {
			t.Errorf("%s members = %v, want any: %v", tc.kind, plan.Members, tc.wantMembers)
		}
		if plan.Description == "" {
			t.Errorf("%s: a plan has to say what it restores", tc.kind)
		}
	}
}

// Every account on a cPanel server belongs to a different customer, so a
// path that leaves this account's home directory is refused rather than
// quietly restored.
func TestAPathOutsideTheAccountIsRefused(t *testing.T) {
	for _, name := range []string{
		"/home/someone-else/public_html",
		"../someone-else/.ssh",
		"/etc/shadow",
		"public_html/../../someone-else",
		"",
	} {
		if _, err := Build(split, Request{Kind: KindFiles, Account: "studio", Names: []string{name}}); err == nil {
			t.Errorf("a files restore of %q was accepted", name)
		}
	}
}

func TestADatabaseIsANameNotAPath(t *testing.T) {
	for _, name := range []string{"../../etc/passwd", "studio/../..", "a/b"} {
		if _, err := Build(split, Request{Kind: KindDatabase, Account: "studio", Names: []string{name}}); err == nil {
			t.Errorf("a database restore of %q was accepted", name)
		}
	}
}

// A plan that asks for nothing would report success having restored
// nothing, so an empty or impossible request has to fail here.
func TestAnImpossibleRequestFailsRatherThanAskingForNothing(t *testing.T) {
	monolithic := reassemble.Parts{Archive: "/var/lib/cprest/staging/stage-backup-studio/cpmove-studio.tar"}

	for _, tc := range []struct {
		what  string
		parts reassemble.Parts
		req   Request
	}{
		{"a database from a snapshot with no dumps", monolithic,
			Request{Kind: KindDatabase, Account: "studio", Names: []string{"studio_kpeh1"}}},
		{"DNS from a snapshot with no metadata", monolithic,
			Request{Kind: KindDNS, Account: "studio"}},
		{"files from a snapshot with no home directory", monolithic,
			Request{Kind: KindFiles, Account: "studio", Names: []string{"public_html"}}},
		{"a mailbox with no mailbox named", split,
			Request{Kind: KindMailbox, Account: "studio"}},
		{"a database with none named", split,
			Request{Kind: KindDatabase, Account: "studio"}},
		{"an unknown kind", split, Request{Kind: "everything", Account: "studio"}},
		{"no account", split, Request{Kind: KindDNS}},
	} {
		if _, err := Build(tc.parts, tc.req); err == nil {
			t.Errorf("%s was accepted", tc.what)
		}
	}
}

// The parts of an account an operator asks for by name, and where each
// one comes from. These are cPanel's own names inside a cpmove archive,
// read off a live 136.0.37 rather than assumed.
func TestEveryBackupItemHasSomewhereToComeFrom(t *testing.T) {
	for _, kind := range Kinds {
		req := Request{Kind: kind, Account: "studio"}
		switch kind {
		case KindFiles:
			req.Names = []string{"public_html/index.php"}
		case KindMailbox:
			req.Names = []string{"studio.co.il/sales"}
		case KindDatabase:
			req.Names = []string{"studio_kpeh1"}
		}
		plan, err := Build(split, req)
		if err != nil {
			t.Errorf("%s: %v", kind, err)
			continue
		}
		if len(plan.Include) == 0 {
			t.Errorf("%s asks for nothing", kind)
		}
		if kind.Title() == string(kind) {
			t.Errorf("%s has no name an operator would recognise", kind)
		}
	}
}

// Database users are staged beside the dumps, so a snapshot's paths do not
// change when an account gains or loses a database.
func TestDatabaseUsersComeFromBesideTheDumps(t *testing.T) {
	plan, err := Build(split, Request{Kind: KindDBUsers, Account: "studio"})
	if err != nil {
		t.Fatal(err)
	}
	want := split.Databases + "/" + DatabaseUsersFile
	if len(plan.Include) != 1 || plan.Include[0] != want {
		t.Errorf("include = %v, want %q", plan.Include, want)
	}

	// A backup with no databases has no users to restore either, and says
	// so rather than producing an empty file.
	if _, err := Build(reassemble.Parts{Metadata: "/stage/metadata", Homedir: "/home/studio"},
		Request{Kind: KindDBUsers, Account: "studio"}); err == nil {
		t.Error("database users were promised from a backup that holds none")
	}
}
