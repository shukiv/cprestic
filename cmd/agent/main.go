// Command cprest-agent runs on a cPanel server. It long-polls the
// controller for backup jobs, stages a payload once, and uploads it to
// every repository the job names.
//
// The agent never receives delete-capable credentials: retention and
// pruning belong to cprest-maintenance. See docs/DESIGN.md §8.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shuki/cprest/internal/agent"
	"github.com/shuki/cprest/internal/cpanel"
	"github.com/shuki/cprest/internal/resticrun"
	"github.com/shuki/cprest/internal/staging"
)

type config struct {
	controllerURL string
	clientCert    string
	clientKey     string
	caBundle      string
	stagingRoot   string
	runtimeDir    string
	maxConcurrent int
	safetyMargin  float64
	resticBinary  string
	resticCache   string
	resticCACert  string
	pollInterval  time.Duration
	targetTimeout time.Duration
	hostname      string
	logLevel      string
	fakeRoot      string
	preflightOnly bool
}

func main() {
	cfg := parseFlags()
	log := newLogger(cfg.logLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	hostname, _ := os.Hostname()

	flag.StringVar(&cfg.controllerURL, "controller", "", "controller base URL (https)")
	flag.StringVar(&cfg.clientCert, "client-cert", "", "mTLS client certificate")
	flag.StringVar(&cfg.clientKey, "client-key", "", "mTLS client private key")
	flag.StringVar(&cfg.caBundle, "ca-bundle", "", "CA bundle used to verify the controller")
	flag.StringVar(&cfg.stagingRoot, "staging-root", "/var/lib/cprest/staging",
		"directory pkgacct stages into; size this volume deliberately")
	flag.StringVar(&cfg.runtimeDir, "runtime-dir", "/run/cprest",
		"directory for the transient restic password file")
	flag.IntVar(&cfg.maxConcurrent, "max-concurrent", 1, "accounts staged at once")
	flag.Float64Var(&cfg.safetyMargin, "safety-margin", 0.2,
		"extra free space required beyond the payload estimate, as a fraction")
	flag.StringVar(&cfg.resticBinary, "restic", "restic", "path to the restic binary")
	flag.StringVar(&cfg.resticCache, "restic-cache", "/var/cache/cprest/restic",
		"restic cache directory; a warm cache markedly speeds up repeat backups")
	flag.StringVar(&cfg.resticCACert, "restic-cacert", "",
		"CA bundle restic should trust, for a destination behind a private CA")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", 5*time.Second,
		"pause after an empty poll")
	flag.DurationVar(&cfg.targetTimeout, "target-timeout", 4*time.Hour,
		"how long one repository upload may take before it is abandoned")
	flag.StringVar(&cfg.hostname, "hostname", hostname, "hostname reported to the controller")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	flag.StringVar(&cfg.fakeRoot, "fake-cpanel-root", "",
		"use a synthetic cPanel provider rooted here, for development without cPanel")
	flag.BoolVar(&cfg.preflightOnly, "preflight", false, "check local prerequisites and exit")
	flag.Parse()
	return cfg
}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	resticVersion, err := resticVersion(ctx, cfg.resticBinary)
	if err != nil {
		return err
	}
	if err := ensureDir(cfg.stagingRoot); err != nil {
		return err
	}
	if err := ensureDir(cfg.runtimeDir); err != nil {
		return err
	}

	stagingManager := &staging.Manager{
		Root:              cfg.stagingRoot,
		SafetyMarginRatio: cfg.safetyMargin,
		MaxConcurrent:     cfg.maxConcurrent,
	}
	available, err := staging.AvailableBytes(cfg.stagingRoot)
	if err != nil {
		return err
	}

	provider, err := buildProvider(cfg)
	if err != nil {
		return err
	}
	capabilities, err := provider.Capabilities(ctx)
	if err != nil {
		return err
	}

	if cfg.preflightOnly {
		fmt.Printf("restic:       %s\n", resticVersion)
		fmt.Printf("staging root: %s (%d bytes available)\n", cfg.stagingRoot, available)
		fmt.Printf("pkgacct:      nocompress=%q skiphomedir=%q skipdb=%q\n",
			capabilities.NoCompressFlag, capabilities.SkipHomedirFlag, capabilities.SkipDBFlag)
		fmt.Println("preflight ok")
		return nil
	}

	if cfg.controllerURL == "" {
		return errors.New("-controller is required")
	}
	client, err := agent.NewClient(agent.ClientConfig{
		BaseURL:        cfg.controllerURL,
		ClientCertPath: cfg.clientCert,
		ClientKeyPath:  cfg.clientKey,
		CABundlePath:   cfg.caBundle,
		// The controller holds a job request open, so polling needs a
		// deadline longer than its long-poll window.
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		return err
	}

	runner := resticrun.New(resticrun.Config{
		Binary:     cfg.resticBinary,
		RuntimeDir: cfg.runtimeDir,
		CacheDir:   cfg.resticCache,
		CACertPath: cfg.resticCACert,
	}, nil)

	worker := agent.New(agent.Config{
		Client: client, Provider: provider, Staging: stagingManager,
		Runner: runner, Log: log, Hostname: cfg.hostname,
		ResticVersion: resticVersion, PollInterval: cfg.pollInterval,
		TargetTimeout: cfg.targetTimeout,
	})

	if err := worker.Enrol(ctx); err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	if err := worker.CleanStaleStaging(); err != nil {
		log.Error("clean stale staging", "error", err)
	}

	log.Info("agent running", "hostname", cfg.hostname,
		"controller", cfg.controllerURL, "staging_root", cfg.stagingRoot)
	return worker.Run(ctx)
}

func buildProvider(cfg config) (cpanel.Provider, error) {
	if cfg.fakeRoot == "" {
		return &cpanel.Real{}, nil
	}
	// A synthetic provider makes the agent runnable on a developer
	// machine and in the end-to-end suite, where no cPanel exists.
	if err := ensureDir(cfg.fakeRoot); err != nil {
		return nil, err
	}
	return &cpanel.Fake{Root: cfg.fakeRoot}, nil
}

func resticVersion(ctx context.Context, binary string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, "version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run %s version: %w", binary, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Clean(path), err)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}
