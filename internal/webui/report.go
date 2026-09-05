package webui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/agent"
	"github.com/shuki/cprest/internal/bugreport"
	"github.com/shuki/cprest/internal/job"
)

// gatherReport collects what a maintainer needs to act on a report.
//
// Everything here is chosen rather than swept up: it goes into an issue
// that may be read by anybody. No credential reaches it by construction --
// the settings section names fields one at a time, and the destinations
// section carries kinds and names, never their configuration. The service
// log is the one part that is copied rather than built, so it goes through
// the redactor.
func (s *Server) gatherReport(ctx context.Context, subject, body string) bugreport.Report {
	report := bugreport.Report{Subject: subject, Body: body}
	add := func(title, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		report.Sections = append(report.Sections, bugreport.Section{
			Title: title,
			Text:  bugreport.Clip(bugreport.Redact(text), bugreport.MaxSectionBytes),
		})
	}

	add("Versions and environment", s.reportEnvironment(ctx))
	add("Recent failures", s.reportFailures())
	add("Settings", s.reportSettings())
	add("Service log", s.reportLog(ctx))
	return report
}

func (s *Server) reportEnvironment(ctx context.Context) string {
	var out strings.Builder
	fmt.Fprintf(&out, "cprest       %s\n", agent.Version)
	fmt.Fprintf(&out, "go           %s (%s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if version, err := os.ReadFile("/usr/local/cpanel/version"); err == nil {
		fmt.Fprintf(&out, "cpanel       %s\n", strings.TrimSpace(string(version)))
	}
	if release, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(release), "\n") {
			if name, value, found := strings.Cut(line, "="); found && name == "PRETTY_NAME" {
				fmt.Fprintf(&out, "os           %s\n", strings.Trim(value, `"`))
			}
		}
	}
	if settings, err := s.engine.Store().Settings(); err == nil {
		binary := settings.ResticBinary
		if binary == "" {
			binary = "restic"
		}
		if version, err := exec.CommandContext(ctx, binary, "version").Output(); err == nil {
			fmt.Fprintf(&out, "restic       %s\n", strings.TrimSpace(string(version)))
		}
		fmt.Fprintf(&out, "hostname     %s\n", settings.Hostname)
	}
	return out.String()
}

// reportFailures is what this server has recently failed to do. A report
// that arrives with the errors already in it is one nobody has to ask
// three questions about first.
func (s *Server) reportFailures() string {
	store := s.engine.Store()
	var lines []string

	jobs, err := store.Jobs(0)
	if err == nil {
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].QueuedAt.After(jobs[j].QueuedAt) })
		shown := 0
		for _, run := range jobs {
			if run.Status != job.StatusFailed && run.Status != job.StatusPartialSuccess {
				continue
			}
			trouble := failureOf(run)
			lines = append(lines, fmt.Sprintf("backup  %s  %s  %s  %s",
				stampOf(run.QueuedAt), run.Account, run.Status, trouble))
			if shown++; shown >= 10 {
				break
			}
		}
	}
	restores, err := store.Restores(0)
	if err == nil {
		sort.Slice(restores, func(i, j int) bool {
			return restores[i].QueuedAt.After(restores[j].QueuedAt)
		})
		shown := 0
		for _, run := range restores {
			if run.Status != job.StatusFailed {
				continue
			}
			lines = append(lines, fmt.Sprintf("restore %s  %s  %s  %s",
				stampOf(run.QueuedAt), run.Account, restoreRow{Restore: run}.Parts(), run.Error))
			if shown++; shown >= 10 {
				break
			}
		}
	}
	if len(lines) == 0 {
		return "nothing has failed recently"
	}
	return strings.Join(lines, "\n")
}

func stampOf(at time.Time) string { return at.UTC().Format("2006-01-02 15:04") }

// reportSettings names the settings that shape behaviour, one at a time.
// Building it field by field is what keeps a credential out of it: there
// is no path here that prints something merely because it is stored.
func (s *Server) reportSettings() string {
	settings, err := s.engine.Store().Settings()
	if err != nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "staging root           %s\n", settings.StagingRoot)
	fmt.Fprintf(&out, "accounts at once       %d\n", settings.MaxConcurrent)
	fmt.Fprintf(&out, "safety margin          %.2f\n", settings.SafetyMargin)
	fmt.Fprintf(&out, "keep restored files    %d days\n", keepDays(settings))
	fmt.Fprintf(&out, "keep deleted accounts  %d days\n", deletedDays(settings))
	fmt.Fprintf(&out, "block unsafe removal   %t\n", settings.ProtectAccountRemoval)
	fmt.Fprintf(&out, "back up on suspension  %t\n", settings.BackupOnSuspension)

	if destinations, err := s.engine.Store().Destinations(); err == nil {
		fmt.Fprintf(&out, "\ndestinations (%d)\n", len(destinations))
		for _, dest := range destinations {
			fmt.Fprintf(&out, "  %s  kind=%s  append_only=%t  last_error=%q\n",
				dest.Name, dest.Type, dest.AppendOnly, dest.LastCheckError)
		}
	}
	if policies, err := s.engine.Store().Policies(); err == nil {
		fmt.Fprintf(&out, "\nschedules (%d)\n", len(policies))
		for _, policy := range policies {
			fmt.Fprintf(&out, "  %s  cron=%q  enabled=%t  accounts=%d\n",
				policy.Name, policy.ScheduleCron, policy.Enabled, len(policy.Accounts))
		}
	}
	return out.String()
}

// reportLog is the tail of what the service logged. It is the most useful
// section and the only one copied rather than built, which is why it goes
// through the redactor on the way out.
func (s *Server) reportLog(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", "-u", "cprest",
		"-n", "200", "--no-pager", "--output", "short-iso").Output()
	if err != nil {
		return "the service log could not be read: " + err.Error()
	}
	return string(out)
}

// reportView is the bug-report form and, after sending, what became of it.
type reportView struct {
	Subject string
	Body    string
	// Repository is where it will go, and Direct says this server can
	// open the issue itself rather than handing it to the browser.
	Repository string
	Direct     bool
	// Preview is the whole report as it would be sent, shown before it is
	// sent because an issue may be read by anybody.
	Preview string
	// IssueURL is where a sent report ended up; Form is GitHub's own
	// new-issue address with the report filled in, for a server with no
	// token.
	IssueURL string
	Form     string
	Error    string
}

// handleReport draws the bug-report form.
//
// It is a page as well as a dialog: the dialog fetches this and lifts the
// form out of it, and a browser with scripting switched off gets the same
// form on a page of its own. Nothing about reporting a bug should need
// JavaScript -- the thing being reported may be the reason it is off.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	view := reportView{
		Repository: s.engine.BugRepository(),
		Direct:     s.engine.HasBugToken(),
		Subject:    r.URL.Query().Get("subject"),
	}
	s.render(w, r, "report.html", "Report a problem", "", view)
}

// handleSendReport gathers the debug information, and either opens the
// issue or hands the operator GitHub's form with it filled in.
func (s *Server) handleSendReport(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(r.PostFormValue("subject"))
	body := strings.TrimSpace(r.PostFormValue("body"))
	view := reportView{
		Subject:    subject,
		Body:       body,
		Repository: s.engine.BugRepository(),
		Direct:     s.engine.HasBugToken(),
	}
	if subject == "" || body == "" {
		view.Error = "A report needs a subject and a description of what happened."
		s.render(w, r, "report.html", "Report a problem", "", view)
		return
	}

	report := s.gatherReport(r.Context(), subject, body)
	view.Preview = report.Markdown()

	// Sending publishes this to GitHub, where an issue may be read by
	// anybody, so it happens because somebody pressed send on a page that
	// showed them what was in it -- never as a side effect of opening the
	// form.
	if r.PostFormValue("send") != "1" {
		view.Form = bugreport.NewIssueURL(view.Repository, subject, view.Preview)
		s.render(w, r, "report.html", "Report a problem", "", view)
		return
	}
	if !view.Direct {
		view.Form = bugreport.NewIssueURL(view.Repository, subject, view.Preview)
		s.render(w, r, "report.html", "Report a problem", "", view)
		return
	}

	issue, err := s.engine.ReportBug(r.Context(), subject, view.Preview)
	if err != nil {
		s.log.Error("send a bug report", "error", err)
		view.Error = err.Error()
		view.Form = bugreport.NewIssueURL(view.Repository, subject, view.Preview)
		s.render(w, r, "report.html", "Report a problem", "", view)
		return
	}
	s.log.Info("bug report sent", "issue", issue)
	view.IssueURL = issue
	s.render(w, r, "report.html", "Report a problem", "", view)
}
