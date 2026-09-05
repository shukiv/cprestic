package bugreport

import (
	"strings"
	"testing"
)

// A report becomes an issue anybody may read, and the one section that is
// copied rather than built is the service log. Whatever a log line carries,
// a credential must not leave the server in it.
func TestNothingSecretSurvivesRedaction(t *testing.T) {
	log := strings.Join([]string{
		`level=INFO msg="opened" password=hunter2 account=c1`,
		`RESTIC_PASSWORD=correct-horse-battery`,
		`Authorization: Bearer ghs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`,
		`sftp://backups:s3cr3t@vault.example.com/srv`,
		`token: ghp_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`,
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaA\n-----END OPENSSH PRIVATE KEY-----",
		`msg="backup finished" account=c1 status=success`,
	}, "\n")

	safe := Redact(log)
	for _, leaked := range []string{
		"hunter2", "correct-horse-battery", "ghs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"s3cr3t", "ghp_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "b3BlbnNzaA",
	} {
		if strings.Contains(safe, leaked) {
			t.Errorf("%q survived redaction:\n%s", leaked, safe)
		}
	}
	// What is left has to still be readable, or nobody can act on it.
	if !strings.Contains(safe, `account=c1 status=success`) {
		t.Errorf("redaction ate the log:\n%s", safe)
	}
}

// A journal that has been shouting for a week must not become a report
// nobody reads. The end is what matters: it is nearest what went wrong.
func TestALongSectionKeepsItsEnd(t *testing.T) {
	long := strings.Repeat("an old line that does not matter\n", 500) + "the last thing that happened\n"
	clipped := Clip(long, 200)
	if len(clipped) > 300 {
		t.Errorf("clipped to %d bytes", len(clipped))
	}
	if !strings.Contains(clipped, "the last thing that happened") {
		t.Error("the end was cut instead of the beginning")
	}
	if !strings.Contains(clipped, "earlier lines left out") {
		t.Error("the report does not say that something was left out")
	}
	if short := Clip("two lines\nonly\n", 200); short != "two lines\nonly\n" {
		t.Errorf("a short section was touched: %q", short)
	}
}

func TestOnlyAnOwnerAndRepositoryIsAccepted(t *testing.T) {
	for _, bad := range []string{
		"", "cprestic", "owner/repo/extra", "owner/", "/repo",
		"owner/repo;rm -rf", "owner/repo?x=1", "../../etc",
	} {
		if err := UsableRepository(bad); err == nil {
			t.Errorf("%q was accepted as a repository", bad)
		}
	}
	if err := UsableRepository(DefaultRepository); err != nil {
		t.Errorf("%s was refused: %v", DefaultRepository, err)
	}
}

// The pre-filled form is the way that needs nothing configured, so it has
// to survive a report longer than a URL can carry.
func TestThePrefilledFormCarriesTheReport(t *testing.T) {
	link := NewIssueURL("", "A restore failed", strings.Repeat("x", 20000))
	if !strings.HasPrefix(link, "https://github.com/"+DefaultRepository+"/issues/new?") {
		t.Errorf("link = %s", link)
	}
	if !strings.Contains(link, "title=A+restore+failed") {
		t.Errorf("the subject is not in the link: %s", link)
	}
	if len(link) > 12000 {
		t.Errorf("the link is %d characters, which no browser will take", len(link))
	}
	if !strings.Contains(link, "cut+to+fit+a+link") {
		t.Error("a report cut to fit does not say so")
	}
}

func TestTheIssueBodyCarriesEverySection(t *testing.T) {
	report := Report{
		Subject: "A restore failed",
		Body:    "It said success and the account was not there.",
		Sections: []Section{
			{Title: "Versions and environment", Text: "cprest 0.1.0"},
			{Title: "Empty", Text: "  "},
			{Title: "Service log", Text: "msg=\"restore finished\""},
		},
	}
	markdown := report.Markdown()
	for _, want := range []string{
		"It said success", "### Versions and environment", "cprest 0.1.0",
		"### Service log", "Reported from cP:Restic on",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("the issue body does not carry %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "### Empty") {
		t.Error("a section with nothing in it was written out anyway")
	}
}
