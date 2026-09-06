package node

import (
	"context"
	"path/filepath"

	"github.com/shukiv/gniza/internal/bugreport"
)

// BugIntakeKeyPath is server-side only; neither the key nor a freely chosen
// destination URL is accepted from a bug-report form.
func (e *Engine) BugIntakeKeyPath() string {
	return filepath.Join(e.settings.ConfigDir, bugreport.IntakeKeyFile)
}

func (e *Engine) BugIntakeSetupError() string {
	_, err := bugreport.ReadIntakeKey(e.BugIntakeKeyPath())
	if err != nil {
		return err.Error()
	}
	return ""
}

// CanSendBugReport describes local readiness, not a remote health probe.
// No network request is made merely to open the page.
func (e *Engine) CanSendBugReport() bool { return e.BugIntakeSetupError() == "" }

func (e *Engine) SendBugReport(ctx context.Context, report bugreport.Report) (bugreport.IntakeReceipt, error) {
	key, err := bugreport.ReadIntakeKey(e.BugIntakeKeyPath())
	if err != nil {
		return bugreport.IntakeReceipt{}, err
	}
	return bugreport.Submit(ctx, e.bugReportHTTPClient, key, report)
}
