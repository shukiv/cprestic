package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/shuki/cprest/internal/controller"
	"github.com/shuki/cprest/internal/store"
	"github.com/shuki/cprest/internal/vault"
)

func runServe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := flags.String("listen", ":8443", "listen address for the agent API")
	databaseURL := flags.String("database-url", os.Getenv("CPREST_DATABASE_URL"),
		"PostgreSQL connection string")
	masterKeyPath := flags.String("master-key", "", "vault master key file")
	tlsCert := flags.String("tls-cert", "", "controller server certificate")
	tlsKey := flags.String("tls-key", "", "controller server private key")
	clientCA := flags.String("client-ca", "", "CA that signs agent client certificates")
	lease := flags.Duration("lease", 6*time.Hour, "how long a claimed job stays leased")
	logLevel := flags.String("log-level", "info", "debug, info, warn or error")
	migrate := flags.Bool("migrate", true, "apply pending migrations at startup")
	if err := flags.Parse(args); err != nil {
		return err
	}

	switch {
	case *databaseURL == "":
		return errors.New("-database-url is required")
	case *masterKeyPath == "":
		return errors.New("-master-key is required")
	case *tlsCert == "" || *tlsKey == "" || *clientCA == "":
		// The agent API carries credentials for every destination. It is
		// mTLS only; there is no plaintext or password-authenticated mode.
		return errors.New("-tls-cert, -tls-key and -client-ca are required: the agent API is mTLS only")
	}

	log := newLogger(*logLevel)

	db, err := store.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	if *migrate {
		applied, err := db.Migrate(ctx)
		if err != nil {
			return err
		}
		if len(applied) > 0 {
			log.Info("applied migrations", "migrations", applied)
		}
	}

	v, err := openVault(*masterKeyPath)
	if err != nil {
		return err
	}

	service := controller.New(db, v, log)
	service.LeaseDuration = *lease

	tlsConfig, err := serverTLS(*tlsCert, *tlsKey, *clientCA)
	if err != nil {
		return err
	}

	api := controller.NewAPI(service, log)
	server := &http.Server{
		Addr:              *listen,
		Handler:           api.Handler(),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		// Long polling holds a job request open, so the write timeout must
		// exceed the poll window.
		WriteTimeout: api.LongPollWait + 30*time.Second,
		IdleTimeout:  2 * time.Minute,
	}

	scheduler := controller.NewScheduler(service, log)
	go func() {
		if err := scheduler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("scheduler stopped", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Info("controller listening", "address", *listen, "vault_key_id", v.KeyID())
	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func serverTLS(certPath, keyPath, clientCAPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(clientCAPath)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("client CA %s contains no certificates", clientCAPath)
	}
	return controller.ServerTLSConfig(cert, pool), nil
}

func openVault(masterKeyPath string) (*vault.Vault, error) {
	key, err := vault.LoadMasterKey(masterKeyPath)
	if err != nil {
		return nil, err
	}
	return vault.New(key)
}

func runMigrate(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("migrate", flag.ExitOnError)
	databaseURL := flags.String("database-url", os.Getenv("CPREST_DATABASE_URL"),
		"PostgreSQL connection string")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *databaseURL == "" {
		return errors.New("-database-url is required")
	}

	db, err := store.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("database is up to date")
		return nil
	}
	for _, name := range applied {
		fmt.Printf("applied %s\n", name)
	}
	return nil
}
