package bugreport

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestTheReportCarriesEverySection(t *testing.T) {
	report := Report{
		Subject: "A restore failed",
		Body:    "It said success and the account was not there.",
		Sections: []Section{
			{Title: "Versions and environment", Text: "gniza 0.1.0"},
			{Title: "Empty", Text: "  "},
			{Title: "Service log", Text: "msg=\"restore finished\""},
		},
	}
	markdown := report.Markdown()
	for _, want := range []string{
		"It said success", "### Versions and environment", "gniza 0.1.0",
		"### Service log", "Reported from Gniza on",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("the report does not carry %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "### Empty") {
		t.Error("a section with nothing in it was written out anyway")
	}
}

// A bug report is handed to the local mail server, so the message is built
// here. A header takes whatever an operator typed into the subject, and a
// log line can be anything at all.
func TestTheMessageCannotBeUsedToWriteHeaders(t *testing.T) {
	dir := t.TempDir()
	saved := filepath.Join(dir, "message")
	fake := filepath.Join(dir, "sendmail")
	// A sendmail that keeps what it was given.
	script := "#!/bin/sh\ncat > " + saved + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	err := Mail(context.Background(), fake, "gniza@host.example.com", "maintainer@example.com",
		"Restore failed\r\nBcc: somebody@elsewhere.example",
		"a line\n.\nafter the dot\n")
	if err != nil {
		t.Fatalf("Mail: %v", err)
	}
	written, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	message := string(written)

	headers, body, found := strings.Cut(message, "\r\n\r\n")
	if !found {
		t.Fatalf("no body:\n%s", message)
	}
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Errorf("a subject wrote a header of its own:\n%s", headers)
		}
	}
	if strings.Count(headers, "Subject:") != 1 {
		t.Errorf("the subject did not stay one header:\n%s", headers)
	}
	if !strings.Contains(headers, "To: maintainer@example.com") {
		t.Errorf("headers = %q", headers)
	}
	// A lone dot ends a message. The line after it has to survive.
	if !strings.Contains(body, "after the dot") {
		t.Errorf("the body was cut at a dot:\n%q", body)
	}
}

func TestOnlyAnAddressIsAccepted(t *testing.T) {
	for _, bad := range []string{
		"", "nobody", "nobody@", "@example.com", "a@b",
		"someone@example.com\r\nBcc: else@example.com",
		"someone@example.com, other@example.com",
		"<someone@example.com>", "a b@example.com",
	} {
		if err := UsableAddress(bad); err == nil {
			t.Errorf("%q was accepted as an address", bad)
		}
	}
	for _, good := range []string{
		"someone@example.com", "first.last+tag@sub.example.co.uk",
	} {
		if err := UsableAddress(good); err != nil {
			t.Errorf("%q was refused: %v", good, err)
		}
	}
}

func TestSafeReportRemainsTheSameDuringPreviewAndDownload(t *testing.T) {
	report := Report{Subject: "Failure", Body: "password=redact-me", Sections: []Section{
		{Title: "Single long line", Text: strings.Repeat("א", MaxSectionBytes)},
		{Title: "Long log", Text: strings.Repeat("an earlier log line\n", MaxSectionBytes)},
	}}
	safe := report.Safe()
	if !reflect.DeepEqual(safe, safe.Safe()) {
		t.Fatal("preparing a reviewed report changed its content")
	}
	if strings.Contains(safe.Body, "redact-me") {
		t.Fatal("the operator's own text was not redacted")
	}
	for _, section := range safe.Sections {
		if len(section.Text) > MaxSectionBytes || !utf8.ValidString(section.Text) {
			t.Fatal("diagnostic section exceeds its cap or splits a UTF-8 character")
		}
	}
}

// TestAReportIsRefusedHereRatherThanOnTheForm covers the limits the public
// form imposes. Nothing this program sends enforces them any more, so this
// is the only place a too-long or multi-line subject is caught before an
// operator carries it there and is turned away.
func TestAReportIsRefusedHereRatherThanOnTheForm(t *testing.T) {
	for name, report := range map[string]Report{
		"no subject":       {Body: "something"},
		"no body":          {Subject: "something"},
		"subject too long": {Subject: strings.Repeat("x", 256), Body: "something"},
		"subject wrapped":  {Subject: "two\nlines", Body: "something"},
		"body too long":    {Subject: "one line", Body: strings.Repeat("x", 20001)},
	} {
		if err := report.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := (Report{Subject: "One line", Body: "What happened"}).Validate(); err != nil {
		t.Errorf("a usable report was refused: %v", err)
	}
}
