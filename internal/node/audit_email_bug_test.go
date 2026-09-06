package node

import (
	"path/filepath"
	"testing"

	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/nodestore"
)

// Positive audit reproduction: SkipEmail removes mailbox messages but still
// sends cPanel's email account names and password hashes from ~/etc.
func TestAuditSkipEmailStillIncludesMailCredentials(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "customer1")
	excludes := excludesFor(nodestore.Policy{SkipEmail: true}, cpanel.AccountInfo{
		HomeDir: home,
	}, nil)

	wantMail := filepath.Join(home, "mail")
	wantEtc := filepath.Join(home, "etc")
	mailExcluded := false
	etcExcluded := false
	for _, exclude := range excludes {
		mailExcluded = mailExcluded || exclude == wantMail
		etcExcluded = etcExcluded || exclude == wantEtc
	}
	if !mailExcluded {
		t.Fatalf("mailbox directory was not excluded: %v", excludes)
	}
	if etcExcluded {
		t.Fatalf("mail credentials were unexpectedly excluded: %v", excludes)
	}
}
