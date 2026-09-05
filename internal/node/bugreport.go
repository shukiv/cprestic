package node

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/shuki/cprest/internal/bugreport"
)

// DefaultBugEmail is where a report goes when nobody has said otherwise:
// the address that maintains this program.
const DefaultBugEmail = ""

// BugEmail is where this server sends bug reports.
func (e *Engine) BugEmail() string {
	if settings, err := e.store.Settings(); err == nil &&
		strings.TrimSpace(settings.BugEmail) != "" {
		return strings.TrimSpace(settings.BugEmail)
	}
	return DefaultBugEmail
}

// CanSendBugReport says whether pressing send would do anything.
//
// It needs an address and a mail server. Every cPanel server runs one --
// it is how a customer's site sends anything at all -- so this is normally
// true without anybody configuring a thing. A server where it is false
// still hands the report over as a file.
func (e *Engine) CanSendBugReport() bool {
	if bugreport.UsableAddress(e.BugEmail()) != nil {
		return false
	}
	return e.mailProgram() != ""
}

// mailProgram is the local mail submission program, or nothing if this
// server has none.
func (e *Engine) mailProgram() string {
	program := bugreport.Sendmail
	if e.settings.SendmailPath != "" {
		program = e.settings.SendmailPath
	}
	if info, err := os.Stat(program); err != nil || info.IsDir() {
		return ""
	}
	return program
}

// SendBugReport mails one report to whoever maintains this program.
//
// Through the server's own mail server rather than through a notification
// channel: the channels belong to the operator and are configured for
// their own alerts, and a server that has none must still be able to
// report a problem.
func (e *Engine) SendBugReport(ctx context.Context, subject, body string) error {
	address := e.BugEmail()
	if err := bugreport.UsableAddress(address); err != nil {
		return fmt.Errorf(
			"node: there is no address to send bug reports to: set one under Settings")
	}
	program := e.mailProgram()
	if program == "" {
		return fmt.Errorf(
			"node: this server has no mail server to send through, so the report can only "+
				"be taken as a file (looked for %s)", bugreport.Sendmail)
	}
	from := "cprest@" + e.settings.Hostname
	if bugreport.UsableAddress(from) != nil {
		from = ""
	}
	return bugreport.Mail(ctx, program, from, address,
		"cP:Restic bug report: "+subject, body)
}
