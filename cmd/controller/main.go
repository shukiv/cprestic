// Command cprest-controller is the control plane and its administration
// CLI. It owns servers, accounts, policies, destinations, repositories,
// schedules, the credential vault and job history, and serves the agent
// API.
//
// Backup data never passes through it: agents write directly to
// destinations, so adding servers does not make the controller a bandwidth
// bottleneck. See docs/DESIGN.md §2.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string) error
}

func main() {
	commands := []command{
		{"serve", "run the agent API and scheduler", runServe},
		{"migrate", "apply database migrations and exit", runMigrate},
		{"keygen", "print a new vault master key", runKeygen},
		{"init-ca", "create the certificate authority for agent mTLS", runInitCA},
		{"issue-cert", "issue a server or agent certificate", runIssueCert},
		{"add-server", "register a cPanel server and pin its agent certificate", runAddServer},
		{"add-account", "register a cPanel account for backup", runAddAccount},
		{"add-destination", "register a storage endpoint", runAddDestination},
		{"add-repository", "create a repository record inside a destination", runAddRepository},
		{"add-policy", "create a schedule with retention settings", runAddPolicy},
		{"attach", "connect a policy to a repository or an account", runAttach},
		{"status", "show servers, repositories and recent jobs", runStatus},
	}

	if len(os.Args) < 2 {
		usage(commands)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for _, cmd := range commands {
		if cmd.name != os.Args[1] {
			continue
		}
		if err := cmd.run(ctx, os.Args[2:]); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			fmt.Fprintf(os.Stderr, "cprest-controller %s: %v\n", cmd.name, err)
			os.Exit(1)
		}
		return
	}

	fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
	usage(commands)
	os.Exit(2)
}

func usage(commands []command) {
	fmt.Fprintln(os.Stderr, "usage: cprest-controller <command> [flags]")
	fmt.Fprintln(os.Stderr, "\ncommands:")
	for _, cmd := range commands {
		fmt.Fprintf(os.Stderr, "  %-16s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprintln(os.Stderr, "\nrun a command with -h for its flags")
}

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}
