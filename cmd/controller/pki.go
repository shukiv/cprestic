package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/certs"
	"github.com/shuki/cprest/internal/vault"
)

func runKeygen(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := flags.String("out", "", "write the key to this file instead of stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	key, err := vault.GenerateMasterKey()
	if err != nil {
		return err
	}
	if *out == "" {
		fmt.Println(key)
		fmt.Fprintln(os.Stderr,
			"store this key outside the database; losing it makes every stored credential unreadable")
		return nil
	}
	// This file decrypts every credential in the system.
	if err := os.WriteFile(*out, []byte(key+"\n"), 0o600); err != nil {
		return fmt.Errorf("write master key: %w", err)
	}
	fmt.Printf("wrote master key to %s (mode 0600)\n", *out)
	return nil
}

func runInitCA(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("init-ca", flag.ExitOnError)
	dir := flags.String("dir", "pki", "directory to write ca.pem and ca-key.pem into")
	commonName := flags.String("cn", "cprest agent CA", "certificate authority name")
	validFor := flags.Duration("valid-for", 10*365*24*time.Hour, "certificate lifetime")
	if err := flags.Parse(args); err != nil {
		return err
	}

	certPath := filepath.Join(*dir, "ca.pem")
	if _, err := os.Stat(certPath); err == nil {
		// Overwriting the CA would orphan every certificate already issued.
		return fmt.Errorf("%s already exists; refusing to replace an existing CA", certPath)
	}

	authority, err := certs.NewAuthority(*commonName, *validFor)
	if err != nil {
		return err
	}
	if err := authority.Pair.WriteFiles(certPath, filepath.Join(*dir, "ca-key.pem")); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\n", certPath, filepath.Join(*dir, "ca-key.pem"))
	return nil
}

func runIssueCert(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("issue-cert", flag.ExitOnError)
	dir := flags.String("ca-dir", "pki", "directory holding ca.pem and ca-key.pem")
	kind := flags.String("kind", "agent", "agent or server")
	name := flags.String("name", "", "common name, typically the hostname")
	hosts := flags.String("hosts", "", "comma-separated DNS names and IPs for a server certificate")
	out := flags.String("out-dir", "", "directory to write the certificate and key into")
	validFor := flags.Duration("valid-for", 2*365*24*time.Hour, "certificate lifetime")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-name is required")
	}
	if *out == "" {
		*out = *dir
	}

	authority, err := certs.LoadAuthority(
		filepath.Join(*dir, "ca.pem"), filepath.Join(*dir, "ca-key.pem"))
	if err != nil {
		return err
	}

	var pair certs.Pair
	switch *kind {
	case "agent":
		pair, err = authority.IssueClient(*name, *validFor)
	case "server":
		var hostList []string
		for _, host := range strings.Split(*hosts, ",") {
			if trimmed := strings.TrimSpace(host); trimmed != "" {
				hostList = append(hostList, trimmed)
			}
		}
		if len(hostList) == 0 {
			hostList = []string{*name}
		}
		pair, err = authority.IssueServer(*name, hostList, *validFor)
	default:
		return fmt.Errorf("-kind must be agent or server, got %q", *kind)
	}
	if err != nil {
		return err
	}

	certPath := filepath.Join(*out, *name+".pem")
	keyPath := filepath.Join(*out, *name+"-key.pem")
	if err := pair.WriteFiles(certPath, keyPath); err != nil {
		return err
	}

	fingerprint, err := certs.FingerprintPEM(pair.CertPEM)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\n", certPath, keyPath)
	if *kind == "agent" {
		// Authorisation is by pinned fingerprint, not by certificate
		// subject: a name is a label, a fingerprint is an identity.
		fmt.Printf("fingerprint: %s\n", fingerprint)
		fmt.Printf("register it with:\n  cprest-controller add-server -hostname %s -fingerprint %s\n",
			*name, fingerprint)
	}
	return nil
}
