package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/shuki/cprest/internal/certs"
	"github.com/shuki/cprest/internal/repobuild"
	"github.com/shuki/cprest/internal/store"
	"github.com/shuki/cprest/internal/vault"
)

// adminFlags adds the connection flags every administrative command needs.
type adminFlags struct {
	databaseURL   *string
	masterKeyPath *string
}

func addAdminFlags(flags *flag.FlagSet) adminFlags {
	return adminFlags{
		databaseURL: flags.String("database-url", os.Getenv("CPREST_DATABASE_URL"),
			"PostgreSQL connection string"),
		masterKeyPath: flags.String("master-key", os.Getenv("CPREST_MASTER_KEY"),
			"vault master key file"),
	}
}

func (a adminFlags) open(ctx context.Context, needVault bool) (*store.Store, *vault.Vault, error) {
	if *a.databaseURL == "" {
		return nil, nil, errors.New("-database-url is required")
	}
	db, err := store.Open(ctx, *a.databaseURL)
	if err != nil {
		return nil, nil, err
	}

	if !needVault {
		return db, nil, nil
	}
	if *a.masterKeyPath == "" {
		db.Close()
		return nil, nil, errors.New("-master-key is required")
	}
	v, err := openVault(*a.masterKeyPath)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, v, nil
}

func runAddServer(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("add-server", flag.ExitOnError)
	admin := addAdminFlags(flags)
	hostname := flags.String("hostname", "", "server hostname")
	fingerprint := flags.String("fingerprint", "", "agent certificate fingerprint")
	certPath := flags.String("cert", "", "agent certificate file, as an alternative to -fingerprint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *hostname == "" {
		return errors.New("-hostname is required")
	}
	if *fingerprint == "" && *certPath == "" {
		return errors.New("either -fingerprint or -cert is required")
	}
	if *certPath != "" {
		certPEM, err := os.ReadFile(*certPath)
		if err != nil {
			return fmt.Errorf("read certificate: %w", err)
		}
		if *fingerprint, err = certs.FingerprintPEM(certPEM); err != nil {
			return err
		}
	}

	db, _, err := admin.open(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()

	id, err := db.CreateServer(ctx, *hostname, *fingerprint)
	if err != nil {
		return err
	}
	fmt.Printf("server %s\n", id)
	return nil
}

func runAddAccount(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("add-account", flag.ExitOnError)
	admin := addAdminFlags(flags)
	serverID := flags.String("server", "", "server id")
	user := flags.String("user", "", "cPanel account user")
	domain := flags.String("domain", "", "primary domain")
	size := flags.Int64("size-estimate", 0,
		"account size in bytes; used by the agent's staging space preflight")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *serverID == "" || *user == "" {
		return errors.New("-server and -user are required")
	}

	db, _, err := admin.open(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()

	id, err := db.CreateAccount(ctx, store.Account{
		ServerID: *serverID, CPanelUser: *user,
		PrimaryDomain: *domain, SizeEstimate: *size,
	})
	if err != nil {
		return err
	}
	fmt.Printf("account %s\n", id)
	return nil
}

func runAddDestination(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("add-destination", flag.ExitOnError)
	admin := addAdminFlags(flags)
	name := flags.String("name", "", "human-readable name")
	destType := flags.String("type", "", "local, sftp, rest or s3")
	config := flags.String("config", "{}", "non-secret settings as JSON")
	secretPairs := flags.String("secrets", "",
		"comma-separated key=value credentials, sealed before storage; "+
			"use key=@file or key=$ENVVAR to keep them out of the command line")
	appendOnly := flags.Bool("append-only", false,
		"the endpoint rejects deletes from agent credentials")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" || *destType == "" {
		return errors.New("-name and -type are required")
	}
	if !json.Valid([]byte(*config)) {
		return errors.New("-config is not valid JSON")
	}

	db, v, err := admin.open(ctx, true)
	if err != nil {
		return err
	}
	defer db.Close()

	secrets, err := parsePairs(*secretPairs)
	if err != nil {
		return err
	}

	var secretID string
	if len(secrets) > 0 {
		sealed, err := repobuild.SealCredentials(v, secrets)
		if err != nil {
			return err
		}
		if secretID, err = db.CreateSecret(ctx, store.SecretBackendCredentials, sealed, v.KeyID()); err != nil {
			return err
		}
	}

	id, err := db.CreateDestination(ctx, store.Destination{
		Name: *name, Type: *destType, Config: []byte(*config),
		CredentialsSecretID: secretID, AppendOnly: *appendOnly,
	})
	if err != nil {
		return err
	}
	fmt.Printf("destination %s\n", id)
	return nil
}

func runAddRepository(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("add-repository", flag.ExitOnError)
	admin := addAdminFlags(flags)
	destinationID := flags.String("destination", "", "destination id")
	serverID := flags.String("server", "", "server id whose backups land here")
	path := flags.String("path", "", "repository path inside the destination")
	password := flags.String("password", "",
		"repository password; generated when omitted")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *destinationID == "" || *serverID == "" || *path == "" {
		return errors.New("-destination, -server and -path are required")
	}

	db, v, err := admin.open(ctx, true)
	if err != nil {
		return err
	}
	defer db.Close()

	repoPassword := *password
	if repoPassword == "" {
		if repoPassword, err = vault.GenerateMasterKey(); err != nil {
			return err
		}
	}
	sealed, err := v.SealString(repoPassword)
	if err != nil {
		return err
	}
	passwordID, err := db.CreateSecret(ctx, store.SecretRepositoryPassword, sealed, v.KeyID())
	if err != nil {
		return err
	}

	repo, err := db.CreateRepository(ctx, store.Repository{
		DestinationID: *destinationID, ServerID: *serverID,
		Path: *path, PasswordSecretID: passwordID,
	})
	if err != nil {
		return err
	}

	fmt.Printf("repository %s\n", repo.ID)
	if repo.ChunkerSourceRepoID != "" {
		// Chunker parameters are fixed at creation, so this is decided
		// now or never. See docs/DESIGN.md §7.
		fmt.Printf("chunker parameters will be copied from repository %s\n",
			repo.ChunkerSourceRepoID)
	}
	fmt.Println("run cprest-maintenance -kind provision to create it on the destination")
	return nil
}

func runAddPolicy(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("add-policy", flag.ExitOnError)
	admin := addAdminFlags(flags)
	name := flags.String("name", "", "policy name")
	schedule := flags.String("cron", "0 2 * * *", "five-field cron schedule")
	mode := flags.String("mode", "split", "split or monolithic payload mode")
	compression := flags.String("compression", "auto", "auto, max or off")
	limitUpload := flags.Int("limit-upload-kib", 0, "upload bandwidth cap in KiB/s")
	keepLast := flags.Int("keep-last", 0, "snapshots to keep regardless of age")
	keepDaily := flags.Int("keep-daily", 7, "daily snapshots to keep")
	keepWeekly := flags.Int("keep-weekly", 4, "weekly snapshots to keep")
	keepMonthly := flags.Int("keep-monthly", 6, "monthly snapshots to keep")
	keepYearly := flags.Int("keep-yearly", 0, "yearly snapshots to keep")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-name is required")
	}

	db, _, err := admin.open(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()

	id, err := db.CreatePolicy(ctx, store.Policy{
		Name: *name, ScheduleCron: *schedule, PayloadMode: *mode,
		Compression: *compression, LimitUploadKiB: *limitUpload,
		Retention: store.Retention{
			KeepLast: *keepLast, KeepDaily: *keepDaily, KeepWeekly: *keepWeekly,
			KeepMonthly: *keepMonthly, KeepYearly: *keepYearly,
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("policy %s\n", id)
	return nil
}

func runAttach(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("attach", flag.ExitOnError)
	admin := addAdminFlags(flags)
	policyID := flags.String("policy", "", "policy id")
	repositoryID := flags.String("repository", "", "repository id to add as a backup target")
	accountID := flags.String("account", "", "account id to put on this policy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *policyID == "" {
		return errors.New("-policy is required")
	}
	if *repositoryID == "" && *accountID == "" {
		return errors.New("either -repository or -account is required")
	}

	db, _, err := admin.open(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()

	if *repositoryID != "" {
		if err := db.AttachRepositoryToPolicy(ctx, *policyID, *repositoryID); err != nil {
			return err
		}
		fmt.Printf("repository %s is now a target of policy %s\n", *repositoryID, *policyID)
	}
	if *accountID != "" {
		if err := db.AttachPolicyToAccount(ctx, *accountID, *policyID); err != nil {
			return err
		}
		fmt.Printf("account %s is now on policy %s\n", *accountID, *policyID)
	}
	return nil
}

func runStatus(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	admin := addAdminFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}

	db, _, err := admin.open(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()

	repos, err := db.ListRepositories(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d repositories\n", len(repos))
	for _, repo := range repos {
		state := "not provisioned"
		if repo.InitialisedAt != nil {
			state = "provisioned " + repo.InitialisedAt.Format("2006-01-02")
		}
		fmt.Printf("  %s  path=%-20s %s\n", repo.ID, repo.Path, state)
	}

	policies, err := db.ListPolicies(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d policies\n", len(policies))
	for _, policy := range policies {
		fmt.Printf("  %s  %-16s cron=%-12s mode=%s\n",
			policy.ID, policy.Name, policy.ScheduleCron, policy.PayloadMode)
	}
	return nil
}

// parsePairs reads "key=value,key=value" into a map.
//
// A value of "@path" is read from a file and "$NAME" from an environment
// variable. Prefer those: a credential typed as a literal is visible in
// /proc/<pid>/cmdline while the command runs, and in shell history
// afterwards.
func parsePairs(input string) (map[string]string, error) {
	pairs := map[string]string{}
	if strings.TrimSpace(input) == "" {
		return pairs, nil
	}
	for _, entry := range strings.Split(input, ",") {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			return nil, fmt.Errorf("expected key=value, got %q", entry)
		}
		key = strings.TrimSpace(key)

		switch {
		case strings.HasPrefix(value, "@"):
			body, err := os.ReadFile(strings.TrimPrefix(value, "@"))
			if err != nil {
				return nil, fmt.Errorf("read secret %s: %w", key, err)
			}
			value = strings.TrimRight(string(body), "\r\n")
		case strings.HasPrefix(value, "$"):
			name := strings.TrimPrefix(value, "$")
			resolved, present := os.LookupEnv(name)
			if !present {
				return nil, fmt.Errorf("secret %s references unset environment variable %s", key, name)
			}
			value = resolved
		}
		pairs[key] = value
	}
	return pairs, nil
}
