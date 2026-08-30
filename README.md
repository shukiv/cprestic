# cprest

cPanel fleet backup orchestration on top of [restic](https://restic.net/).

Backup data flows straight from each cPanel server to its destinations. The
controller schedules work and records state; it never carries backup bytes,
so adding servers does not turn it into a bandwidth bottleneck.

Full design, including the reasoning behind each component:
**[docs/DESIGN.md](docs/DESIGN.md)**.

## What runs

```
Controller ──control only──> Agent (per cPanel server) ──data──> Destinations
                                                                      ▲
                             Maintenance runner ───────delete─────────┘
```

| Binary | Runs on | Does |
|---|---|---|
| `cprest-controller` | trusted infrastructure | agent API over mTLS, scheduler, credential vault, administration CLI |
| `cprest-agent` | every cPanel server | polls for jobs, stages a payload once, uploads it to each target repository |
| `cprest-maintenance` | trusted infrastructure | provisions repositories, applies retention, verifies integrity |

The **maintenance runner** is not optional. Destinations we control run
`rest-server --append-only`, which rejects deletes — so nothing on a cPanel
server can prune, and without a separate trusted actor repositories grow
forever.

One deployment detail matters and is easy to get wrong: `--append-only` is a
property of the rest-server **process**, not of a credential, so it blocks
the maintenance runner too. An append-only destination needs a second
rest-server over the same data directory with append-only off, reachable
only from the management network. Record it as `maintenance_base_url` on the
destination; the maintenance runner uses it, agents never see it. See
DESIGN §8.

## Status

The pipeline works end to end and is covered by a suite that drives real
PostgreSQL, real restic and a real append-only rest-server.

| Area | State |
|---|---|
| Controller: agent API, mTLS auth, job leasing, scheduler, vault | working |
| Agent: enrolment, polling, staging, multi-target upload, reporting | working |
| Maintenance: provision, retention, integrity check | working |
| Administration CLI (`cprest-controller <command>`) | working |
| Real cPanel provider (`pkgacct`, `mysqldump`) | written, **unvalidated** — no cPanel host available |
| Restore | not built; the design is in DESIGN §10 |
| Web UI | not built; the API and CLI are the interface |
| Restore drills, Azure/GCS/rclone destinations | not built |

## Three decisions worth knowing before reading the code

1. **Never feed restic a compressed archive.** `pkgacct` compresses by
   default, and a gzip stream defeats content-defined chunking: every
   nightly run then stores a full copy, in every destination. Payload mode
   `split` is the default for this reason, and the end-to-end suite asserts
   that a second unchanged backup stores under a tenth of what it reads.
   See DESIGN §4.
2. **Chunker parameters are chosen once, forever.** Every repository after a
   server's first is created with `--copy-chunker-params`, so switching to
   "back up once, replicate with `restic copy`" stays possible. Retrofitting
   is impossible. The store fills the source in automatically and a database
   trigger enforces it. See DESIGN §7.
3. **Secrets never reach argv.** `/proc/<pid>/cmdline` is world-readable on a
   multi-tenant cPanel server. Backend credentials go in the child
   environment; repository passwords go in transient mode-0600 files. File
   *paths* are not secrets, so ssh's identity and known-hosts files do travel
   in `-o sftp.args`. See DESIGN §5, §11.

## Build and test

```bash
make            # fmt, vet, test, build
make test       # unit tests; suites needing external services skip themselves
make tools      # install the pinned restic and rest-server used by make e2e
make e2e        # full pipeline against real PostgreSQL, restic and rest-server
```

Go 1.25. Dependencies: `pgx/v5` and `robfig/cron/v3`, nothing else.

`make e2e` needs PostgreSQL (any local installation; the suite starts its own
throwaway cluster on a unix socket), restic v0.19.1 and rest-server v0.14.0.
It writes scratch data to `.tmp/` rather than `/tmp`, which is often a tmpfs
too small for a Postgres cluster plus restic repositories. The suite is
behind the `e2e` build tag so `make test` stays fast and hermetic.

What it proves, against the real binaries:

- repositories provisioned, with the second one's chunker polynomial
  verified equal to the first's via `restic cat config`
- an account backed up to a local destination and an append-only
  rest-server, snapshots and tags read back with `restic snapshots --json`
- a second unchanged run storing under a tenth of what it reads
- the append-only endpoint returning 403 to a delete, and the maintenance
  endpoint over the same data directory pruning successfully
- retention actually removing a snapshot, which requires the paths and tags
  of two runs to be identical
- one unreachable destination producing `partial_success`, with the
  reachable copy intact
- a client certificate that is not registered being refused
- an account too large for the staging volume refused before pkgacct runs

## Getting started

```bash
# 1. Secrets and PKI
cprest-controller keygen -out /etc/cprest/master.key
cprest-controller init-ca -dir /etc/cprest/pki
cprest-controller issue-cert -kind server -name controller.example.com \
    -ca-dir /etc/cprest/pki -hosts controller.example.com

export CPREST_DATABASE_URL=postgres://cprest@localhost/cprest
export CPREST_MASTER_KEY=/etc/cprest/master.key
cprest-controller migrate

# 2. Register a cPanel server. The agent certificate's fingerprint is the
#    identity: a certificate signed by the CA is not enough on its own.
cprest-controller issue-cert -kind agent -name cp01.example.com -ca-dir /etc/cprest/pki
cprest-controller add-server -hostname cp01.example.com -cert /etc/cprest/pki/cp01.example.com.pem

# 3. Destinations. Keep credentials out of the command line with @file or $ENVVAR.
cprest-controller add-destination -name "Bogota rest-server" -type rest -append-only \
    -config '{"base_url":"https://backup.example.com",
              "maintenance_base_url":"https://backup.internal:8000"}' \
    -secrets 'username=cp01,password=@/etc/cprest/rest.pass'
cprest-controller add-destination -name "Wasabi Miami" -type s3 \
    -config '{"endpoint":"s3.us-east-1.wasabisys.com","bucket":"cp-backups","region":"us-east-1"}' \
    -secrets 'access_key_id=$WASABI_KEY,secret_access_key=$WASABI_SECRET'

# 4. Repositories, policy, account
cprest-controller add-repository -server <server-id> -destination <dest-id> -path cp01
cprest-controller add-policy -name nightly -cron '0 2 * * *' -keep-daily 7 -keep-monthly 6
cprest-controller attach -policy <policy-id> -repository <repo-id>
cprest-controller add-account -server <server-id> -user customer1
cprest-controller attach -policy <policy-id> -account <account-id>

# 5. Create the repositories on their destinations, then serve
cprest-maintenance -kind provision
cprest-controller serve -listen :8443 \
    -tls-cert /etc/cprest/pki/controller.example.com.pem \
    -tls-key  /etc/cprest/pki/controller.example.com-key.pem \
    -client-ca /etc/cprest/pki/ca.pem \
    -master-key /etc/cprest/master.key
```

On the cPanel server:

```bash
cprest-agent -preflight    # check restic, staging space and pkgacct flags
cprest-agent -controller https://controller.example.com:8443 \
    -client-cert /etc/cprest/cp01.example.com.pem \
    -client-key  /etc/cprest/cp01.example.com-key.pem \
    -ca-bundle   /etc/cprest/ca.pem
```

Then, from cron on the maintenance host:

```bash
cprest-maintenance -kind forget            # retention and prune
cprest-maintenance -kind check -read-data-subset 5
```

`cprest-agent -fake-cpanel-root <dir>` swaps in a synthetic cPanel provider,
which is how the agent is exercised on a machine with no cPanel.

## Layout

```
cmd/
  agent/         runs on each cPanel server
  controller/    agent API, scheduler and administration CLI
  maintenance/   provisioning, retention, integrity checks
internal/
  agent/         the job loop and the controller client
  certs/         CA and certificate issuance for agent mTLS
  controller/    service layer, agent API, scheduler
  cpanel/        payload production: real pkgacct provider and a synthetic one
  destination/   storage endpoints: URI, environment, ssh options, preflight
  e2e/           the whole pipeline against real dependencies
  job/           job and per-target status model
  maintenance/   repository upkeep with delete-capable credentials
  pkgacct/       capability probing and payload planning
  protocol/      wire types shared by agent and controller
  repobuild/     the single path from sealed credentials to a usable repository
  resticrun/     the single restic execution path
  staging/       scratch space allocation and reclamation
  store/         PostgreSQL queries and the migration runner
  testsupport/   throwaway PostgreSQL and binary discovery for tests
  vault/         envelope encryption for stored credentials
migrations/      PostgreSQL schema
docs/            DESIGN.md and architecture decision records
```

Notes:

- The module path `github.com/shuki/cprest` is a placeholder; no remote
  exists yet.
- `make clean` uses `trash` rather than `rm`.
