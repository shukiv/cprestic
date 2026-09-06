package bugreport

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"os/exec"
	"strings"
	"time"
)

// Sendmail is the local mail submission program. Every cPanel server runs
// a mail server -- it is what tells a customer their site is up -- so a bug
// report needs nothing configured to leave: no SMTP settings, no account
// anywhere, no token. The operator's own notification channels are theirs,
// and a server that has none can still report a problem.
const Sendmail = "/usr/sbin/sendmail"

// Mail hands one report to the local mail server.
//
// The whole message is built here rather than by a mail library: it is one
// plain-text part to one address, and the headers are the few a message
// needs to be delivered and replied to.
func Mail(ctx context.Context, program, from, to, subject, body string) error {
	if strings.TrimSpace(to) == "" {
		return fmt.Errorf("bugreport: no address to send to")
	}
	if err := UsableAddress(to); err != nil {
		return err
	}
	if program == "" {
		program = Sendmail
	}
	if from == "" {
		from = "gniza@localhost"
	} else if err := UsableAddress(from); err != nil {
		return err
	}

	var message bytes.Buffer
	fmt.Fprintf(&message, "From: Gniza <%s>\r\n", from)
	fmt.Fprintf(&message, "To: %s\r\n", to)
	// Encoded, because a subject is whatever an operator typed and a
	// header is ASCII. The line breaks go first: encoding already turns
	// them into text, and a header that cannot end early cannot be used
	// to write another one.
	subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	fmt.Fprintf(&message, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&message, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	message.WriteString("MIME-Version: 1.0\r\n")
	message.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	message.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	message.WriteString("X-Mailer: Gniza\r\n\r\n")
	// A line of a single dot ends a message. A log line that happens to be
	// one must not truncate the report.
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if line == "." {
			line = ".."
		}
		message.WriteString(line)
		message.WriteString("\r\n")
	}

	// -t reads the recipients from the headers, so the address never
	// reaches a command line. -i keeps a lone dot from ending the message
	// early, twice over.
	send := exec.CommandContext(ctx, program, "-t", "-i")
	send.Stdin = bytes.NewReader(message.Bytes())
	var stderr bytes.Buffer
	send.Stderr = &stderr
	if err := send.Run(); err != nil {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return fmt.Errorf("bugreport: hand the report to %s: %s", program, reason)
	}
	return nil
}

// UsableAddress checks an address is one, and carries nothing that would
// mean something to a mail header.
func UsableAddress(address string) error {
	address = strings.TrimSpace(address)
	local, domain, found := strings.Cut(address, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return fmt.Errorf("bugreport: %q is not an email address", address)
	}
	if len(address) > 254 {
		return fmt.Errorf("bugreport: that email address is too long to be one")
	}
	// A newline in a header is a way to add headers of one's own.
	if strings.ContainsAny(address, "\r\n\t <>,;\"") {
		return fmt.Errorf("bugreport: %q is not an email address", address)
	}
	if !strings.Contains(domain, ".") {
		return fmt.Errorf("bugreport: %q has no domain", address)
	}
	return nil
}
