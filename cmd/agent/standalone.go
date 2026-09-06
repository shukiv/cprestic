package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/shuki/cprest/internal/node"
	"github.com/shuki/cprest/internal/nodestore"
	"github.com/shuki/cprest/internal/vault"
	"github.com/shuki/cprest/internal/webui"
)

// runStandalone serves one cPanel server with no controller: local state,
// local scheduling, and an interface behind the WHM plugin.
// See docs/adr/0007-standalone-mode.md.
func runStandalone(ctx context.Context, cfg config, log *slog.Logger) error {
	store, err := nodestore.Open(cfg.statePath)
	if err != nil {
		return err
	}
	defer store.Close()

	v, err := openOrCreateVault(cfg.masterKeyPath, log)
	if err != nil {
		return err
	}

	settings, err := store.Settings()
	if err != nil {
		return err
	}
	// Command-line values win over stored ones, so an operator can correct
	// a bad setting without being able to reach the interface.
	if cfg.stagingRoot != "" {
		settings.StagingRoot = cfg.stagingRoot
	}
	if cfg.resticBinary != "" {
		settings.ResticBinary = cfg.resticBinary
	}
	if cfg.resticCache != "" {
		settings.ResticCache = cfg.resticCache
	}
	if cfg.resticCACert != "" {
		settings.ResticCACert = cfg.resticCACert
	}
	if settings.Hostname == "" {
		settings.Hostname = cfg.hostname
	}
	if cfg.maxConcurrent > 0 {
		settings.MaxConcurrent = cfg.maxConcurrent
	}
	settings.SafetyMargin = cfg.safetyMargin
	if err := store.SaveSettings(settings); err != nil {
		return err
	}

	provider, err := buildProvider(cfg)
	if err != nil {
		return err
	}

	engine, err := node.New(node.Config{
		Store: store, Vault: v, Provider: provider, Log: log,
		HookSpool: cfg.hookSpoolDir,
	})
	if err != nil {
		return err
	}
	if err := engine.ProbeCapabilities(ctx); err != nil {
		// A host whose pkgacct cannot be probed can still be configured;
		// the interface shows the gap.
		log.Error("probe pkgacct", "error", err)
	}
	// What the hooks could not deliver while this service was down, put
	// back before anything reads the account list. An account created or
	// removed in that window cannot be recovered from the list itself,
	// and recording it late is the whole point of the spool.
	if err := engine.ReplayHookSpool(); err != nil {
		log.Error("replay the cPanel events left by hooks", "error", err)
	}
	// Which unix account each cPanel name means, recorded before anything
	// serves a customer. Everything that decides whether a name has
	// changed hands leans on this having happened.
	if err := engine.BackfillIdentities(ctx); err != nil {
		log.Warn("record which unix account each cPanel name means", "error", err)
	}
	if created, err := engine.EnsureProvisioned(ctx); err != nil {
		log.Error("create repositories", "error", err)
	} else if created > 0 {
		log.Info("created repositories", "count", created)
	}

	ui, err := webui.New(engine, log)
	if err != nil {
		return err
	}

	errs := make(chan error, 4)
	go func() { errs <- ui.Listen(ctx, cfg.socketPath) }()
	// The account-facing interface, which cPanel users reach through
	// their own plugin. It is a separate socket because it answers a
	// different question: not "what is on this server" but "what is mine".
	if cfg.userSocketPath != "" {
		go func() { errs <- ui.ListenUser(ctx, cfg.userSocketPath) }()
	}
	if cfg.lifecycleSocketPath != "" {
		go func() { errs <- ui.ListenLifecycle(ctx, cfg.lifecycleSocketPath) }()
	}
	go func() { errs <- engine.Run(ctx) }()

	log.Info("standalone node running",
		"state", cfg.statePath, "socket", cfg.socketPath,
		"staging_root", settings.StagingRoot)

	err = <-errs
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// openOrCreateVault loads the local master key, creating one on first run.
//
// Without it there is nowhere safe to put the credentials an operator types
// into the interface, so the node cannot start.
func openOrCreateVault(path string, log *slog.Logger) (*vault.Vault, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		generated, err := vault.GenerateMasterKey()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(generated+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write master key %s: %w", path, err)
		}
		log.Warn("generated a new encryption key; back it up, "+
			"because without it the stored destination credentials cannot be read",
			"path", path)
	}

	key, err := vault.LoadMasterKey(path)
	if err != nil {
		return nil, err
	}
	return vault.New(key)
}
