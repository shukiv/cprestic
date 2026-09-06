# cP:Restic — critical and high-severity security review, round 2

Date: 2026-09-06  
Revision reviewed: `e5ac8040094af2f03a5f8126b4434f1cc5c83d68`  
Status: **all five findings fixed**, each with a regression test. The report
below is kept as it was written, as the record of what was found; the table
under [What was done](#what-was-done) says where each one was fixed.

## What was done

Each finding was reproduced first, then fixed, and each regression test was
run against the code as it was before the fix to confirm it catches the
defect rather than merely passing.

| ID | Fixed in | How |
| --- | --- | --- |
| SEC-06 | `d047677` | The mysql client is run with `--binary-mode` and `--local-infile=0`, so a dump cannot reach `\!`, `system`, `source` or a local file read. |
| SEC-07 | `043bd3a` | Every claim generates a token that travels in the assignment and must come back in the report; a report is applied only for the attempt that holds the job, and the token is cleared on requeue and on finish. |
| SEC-08 | `de59eaa` | A plan records the keep policy it was taken under, approval copies it, and a destructive forget runs only while the policy in force still equals the one approved. The page says what changed. |
| SEC-09 | `f9ec10d` | Account creations and removals the hook could not deliver are written to a root-only spool before it reports success, and replayed idempotently at startup and before every reconciliation. |
| SEC-10 | `5813c3f` | go 1.26.0 with toolchain go1.26.8 and `golang.org/x/crypto` v0.56.0; `make vuln` and `make provenance` gate the release, the second by reading the toolchain stamped in the built binary. |

The regression tests are permanent and live beside the code they cover:
`internal/cpanel/putback_sql_test.go`,
`internal/store/claim_token_test.go`,
`internal/node/retention_test.go`,
`internal/node/lifecycle_test.go`,
`internal/hookspool/hookspool_test.go`,
`cmd/agent/hook_test.go`.

A follow-up pass over the fixes themselves found three more things, fixed in
`4647fb4` with the same regression discipline:

- The spool stamped an event with the time the socket gave up rather than the
  time cPanel ran the hook. A stopped service takes the full 30-second timeout
  to answer, and a boundary recorded 30 seconds late files the new owner's
  first backups under the customer before them.
- A create spooled for a name that was removed again before the service came
  back could never be recorded, so it was retried on every reconciliation for
  ever. It is now discarded — a name with no holder has no boundary to protect,
  and a name created later brings its own hook. A lookup that failed for any
  other reason is still kept.
- The destinations page offered Approve for a plan `ApproveRetention` refuses,
  because the button asked only whether a plan existed. It now asks the same
  question the approval does.

The same pass found that the deployed hook binary at
`/usr/local/cpanel/3rdparty/bin/cprest-hook` is a second copy of the agent and
had not been replaced by the earlier deploys, so the spooling half of SEC-09
was not yet live on `.144`. Both paths now hold the same build.

Two things an operator has to know about the deployed server:

- Retention approval now records the policy it was given for, so every
  destination approved before this asks to be planned and approved once more.
  Nothing is deleted until it is, which is the safe direction.
- The claim-token migration returns anything then running to the queue, since
  work claimed before there were tokens has no attempt to be told apart from.

## Executive summary

This review found one Critical vulnerability and four High-severity security or
reliability defects. The Critical issue allows a database dump restored by the
root agent to execute operating-system commands as root. Two High issues can
make the controller accept a result or destructive retention policy other than
the one an operator intended. One High issue can expose a departed cPanel
customer's backups to a replacement customer. The final High issue leaves the
public controller on a Go TLS version with a known unauthenticated denial of
service.

| ID | Severity | Finding | Primary impact |
| --- | --- | --- | --- |
| SEC-06 | Critical | A restored SQL dump can execute local commands through the root MySQL/MariaDB client | Root command execution on the recovery server |
| SEC-07 | High | Reports are not bound to a particular lease attempt | Stale work can complete or overwrite a newer backup/restore attempt |
| SEC-08 | High | Retention approval survives unreviewed policy changes | Irreversible deletion under a policy the operator did not approve |
| SEC-09 | High | Missed cPanel hooks plus username/UID reuse collapse two customers into one identity | Cross-customer snapshot and restore exposure |
| SEC-10 | High | Release configuration and local binaries use vulnerable Go/runtime dependencies | Unauthenticated controller DoS and other reachable known vulnerabilities |

The six findings in [the 2026-09-05 review](security-review-2026-09-05.md)
were rechecked where their code paths overlap this round. Their regression
tests still pass; none is being reopened here.

## Scope and method

The review concentrated on the boundaries with the largest blast radius:

- cPanel/WHM lifecycle hooks and self-service account identity;
- granular restore operations that cross from an untrusted backup into a root
  process;
- controller lease ownership, retries, and reports;
- destructive retention approval; and
- the Go runtime and direct dependencies used by release artifacts.

Each application finding was followed from input to privileged effect. Four
were reproduced with temporary positive tests or disposable local services.
Those tests prove the unsafe behavior: a passing audit test means the defect
was reproduced, not that it is fixed. The temporary audit files and containers
were removed after use.

The ordinary `go test ./...` suite passed, including its disposable PostgreSQL
integration tests. That confirms the existing behavior is internally
consistent; it does not invalidate the security reproductions. No connection
was made to `182.54.236.144`, and no production or test-server data was read or
changed.

## SEC-06 — SQL restore executes client commands as root

**Severity: Critical**

**Locations:** [internal/cpanel/putback.go](../internal/cpanel/putback.go),
`LoadDatabase`, especially the `mysql --one-database` invocation; the granular
apply path in [internal/agent/items.go](../internal/agent/items.go).

`LoadDatabase` opens the SQL dump and sends it to the `mysql`/`mariadb` command
line client while the cP:Restic agent is running as root:

```go
load := exec.CommandContext(ctx, r.mysql(), "--one-database", database)
load.Stdin = dump
```

The SQL client has commands which are interpreted locally rather than by the
database server. In particular, `system`/`\!` starts an operating-system
command. The invocation does not enable `--binary-mode`, MariaDB sandbox mode,
or an equivalent command-disable control. `--one-database` is not a sandbox;
MariaDB documents that its filtering is based only on `USE` statements.

This crosses the project's stated threat boundary. The design explicitly
assumes a source cPanel server may be compromised and that its backup archive
must be treated as untrusted. A compromised source can append a crafted
snapshot using the credentials already present on that source. Restoring a
database from such a snapshot then interprets the dump on the recovery server
as root before cPanel's Restricted Restore protections are relevant.

**Local reproduction:** an official MariaDB 10.11 client was connected to a
disposable fake loopback server that completed only the client handshake. The
client was invoked with the same relevant arguments as `LoadDatabase` and fed:

```sql
\! /usr/bin/touch /tmp/cprest-mysql-client-command-ran
SELECT 1;
```

The marker file was created. Repeating the same probe with `--binary-mode`
returned `Unknown command '\!'` and did not create the marker. No real database
or system file was touched.

**Impact:** arbitrary operating-system commands execute as root on the cPanel
recovery server. The dump can also issue SQL outside the intended database
because the client authenticates as MySQL root and `--one-database` does not
enforce server-side privileges or reject fully-qualified statements.

**Recommended fix:**

1. Invoke the client with `--binary-mode` for every noninteractive restore.
   Where the installed MariaDB version supports it, also use `--sandbox` as
   defense in depth.
2. Do not rely on `--one-database` for isolation. Import with a temporary,
   least-privileged database principal restricted to the exact destination
   schema; disable `LOCAL INFILE` for that session.
3. Add real-client regression tests for `\!`, `source`, `tee`, and attempts to
   modify a second schema. The restore must reject them without any local side
   effect.
4. Treat already-stored dumps as untrusted after deploying the fix; changing
   only future dump generation does not make historical backups safe.

Primary references:

- [MySQL client commands](https://dev.mysql.com/doc/refman/8.4/en/mysql-commands.html)
  documents `system`/`\!` and warns that commands can be embedded in stored
  definitions.
- [MySQL client options](https://dev.mysql.com/doc/refman/8.4/en/mysql-command-options.html)
  documents that `--binary-mode` disables client commands in noninteractive
  input.
- [MariaDB command-line client](https://mariadb.com/docs/server/clients-and-utilities/mariadb-client/mariadb-command-line-client)
  documents both client commands and the limited, `USE`-based behavior of
  `--one-database`.
- [MariaDB's sandbox compatibility change](https://mariadb.org/mariadb-dump-file-compatibility-change/)
  describes the shell-command risk in dump files.

## SEC-07 — A stale report can complete a newer lease attempt

**Severity: High**

**Locations:** [internal/store/jobs.go](../internal/store/jobs.go),
`ClaimNextJob`, `ApplyReport`, `jobIsRunningOn`, and
`ReclaimExpiredLeases`; [internal/store/restores.go](../internal/store/restores.go),
`ClaimNextRestore`, `ApplyRestoreReport`, and
`ReclaimExpiredRestoreLeases`; [internal/protocol/protocol.go](../internal/protocol/protocol.go),
the assignment and report wire types.

The controller now correctly rejects a report from a different registered
server, but it cannot tell two attempts by the same server apart. A claim sets
the job to `running` and records an expiry. Reclamation changes it back to
`pending`. A later claim puts the same job ID back into `running`. Reports carry
the job ID but no claim nonce or job-attempt identifier.

`ApplyReport` and `ApplyRestoreReport` therefore check only that the job is
currently `running` for the certificate's server. A report from the expired
first attempt satisfies that condition while the second attempt is running.

The `Attempt` field on an individual backup target is not a lease identity: it
is not present on the job assignment/report as a claim token, and the service
does not use it when applying reports. Restore assignments increment a stored
attempt counter but do not return or require it in the restore report.

**Local reproduction:** against a disposable real PostgreSQL cluster:

1. Claim a backup with an already-expiring lease.
2. Reclaim it and claim the same job again.
3. Submit the first worker's late report.
4. Observe that it is accepted and completes the job; the current worker's
   report is then rejected.

The same sequence was repeated for a restore and produced the same result.
Both positive audit tests passed.

**Impact:** a slow, partitioned, duplicated, or reconnecting agent can replace
the result of the work that currently owns the lease. For backups, old failure
can mask new success or old success can mask current failure. For an applied
restore, the controller can record completion while a different destructive
attempt is still operating on the live cPanel account.

**Recommended fix:** create a cryptographically random claim token whenever a
backup or restore is leased. Include it in the assignment and every progress
and final report. Apply a report only when job ID, authenticated server ID,
claim token, `running` state, and unexpired lease all match atomically. Rotate
the token on every requeue. Add tests proving that attempt A is rejected while
attempt B remains running and can still report.

## SEC-08 — Retention approval is not bound to the policy reviewed

**Severity: High**

**Locations:** [internal/node/retention.go](../internal/node/retention.go),
`PlanRetention`, `ApproveRetention`, `ApplyRetention`, and `retentionFor`;
[internal/nodestore/types.go](../internal/nodestore/types.go), `Repository` and
`RetentionState`; [internal/nodestore/queries.go](../internal/nodestore/queries.go),
`PutPolicy` and `DeletePolicy`; [internal/webui/handlers.go](../internal/webui/handlers.go),
`handleSaveSchedule`.

Approval is stored as only `Repository.RetentionApprovedAt`. It does not record
the merged retention values, a policy revision, or a fingerprint of the rules
shown in the dry-run plan. Editing, disabling, deleting, or moving a policy does
not clear the approval.

`ApplyRetention` checks that the timestamp is non-nil, recomputes the current
merged policy, and immediately runs a destructive `forget`. It can therefore
apply a materially less generous policy than the operator reviewed.

**Local reproduction:** plan a repository with `KeepDaily=30`, approve the
plan, edit the same enabled policy to `KeepDaily=1`, and call
`ApplyRetention`. The fake restic executor recorded the first dry run with 30
and the destructive call with 1. No second plan or approval was required. The
positive audit test passed.

**Impact:** valid backups can be irreversibly forgotten and pruned under a
policy the operator never approved. This needs no malicious actor; a typo,
schedule edit, or policy reassignment after an old approval is enough.

**Recommended fix:** store an approved canonical fingerprint of all enabled
retention inputs affecting the repository. Before any destructive call,
recompute and compare the fingerprint; refuse and require a new plan when it
differs. Invalidate approval on policy create/update/delete and repository
membership changes. A fingerprint of policy semantics is preferable to an
exact snapshot list, because new backups legitimately arrive between plan and
apply. Add a maximum approval age and show the policy delta to the operator.

## SEC-09 — Lost cPanel lifecycle events can expose the previous owner

**Severity: High**

**Locations:** [cmd/agent/hook.go](../cmd/agent/hook.go), the standardized
`Accounts::Create` and `Accounts::Remove` hooks; [cmd/agent/main.go](../cmd/agent/main.go),
the fail-open hook handling; [internal/node/identity.go](../internal/node/identity.go),
`noteIdentity`, `ReconcileAccounts`, `AccountCreated`, `UserSnapshots`, and
`BelongsToCurrentHolder`.

The account-incarnation boundary is correct when the create or remove hook
reaches the service. It fails when both events occur while the service is
unavailable and cPanel/Linux reuse both the username and numeric UID.

The hook process deliberately prints success and exits successfully when it
cannot reach the service. It does not persist the event anywhere durable for
later replay. Polling is expected to reconcile the missed work, but polling
sees only the final `(username, UID)` pair. If both values match the active old
identity record, `ReconcileAccounts` and `noteIdentity` treat it as the same
customer and leave `Recycled=false` with the old visibility boundary.

The source comments correctly acknowledge that polling cannot infer a new
owner when the name and UID are both reused. The missing piece is durable
delivery of the cPanel event that supplies that proof.

**Local reproduction:** seed the identity store with account `webshop`, UID
1042, and an ownership start 30 days ago. Present cPanel's current account list
with a newly-created `webshop`, also UID 1042, without delivering either hook.
Run `ReconcileAccounts`. The identity remains non-recycled with its old start,
and `BelongsToCurrentHolder` accepts a restore belonging to the former owner.
The positive audit test passed.

**Prerequisites:** the service or lifecycle socket is unavailable for both the
remove and create events, the username is recreated, and the operating system
reuses the UID. The conditions are narrower than ordinary username reuse, but
they are realistic during upgrades, maintenance, or a service failure—the
exact cases the fail-open branch handles.

**Impact:** the replacement cPanel customer can list and request restore or
download actions against the previous customer's snapshots. The live cPanel
session and Unix-peer checks still pass because the replacement legitimately
owns the reused name and UID.

**Recommended fix:** make the hook executable durably append every lifecycle
event to a root-owned local journal before returning success, even when the
daemon socket is unavailable. The daemon should replay and acknowledge events
idempotently on startup, with a monotonic event ID. Do not let polling erase an
unreplayed ownership boundary. If durable recording fails, fail closed for
create/remove or explicitly disable user self-service for the ambiguous
account until an administrator resolves it. Add a regression covering service
outage, delete, same-name/same-UID create, restart, and self-service listing.

## SEC-10 — Release artifacts are pinned to vulnerable Go versions

**Severity: High**

**Locations:** [go.mod](../go.mod), the `go 1.25.0` directive and
`golang.org/x/crypto v0.55.0`; [.github/workflows/release.yml](../.github/workflows/release.yml),
`actions/setup-go` with `go-version-file`; [cmd/controller/serve.go](../cmd/controller/serve.go),
the public TLS listener.

The official `govulncheck ./...` scan reports **39 reachable vulnerabilities
from one module and the Go standard library**. Symbol reachability does not
prove every reported advisory is exploitable in this application, so this
finding does not claim all 39 independently. One directly exposed issue is
enough to rate the release unsafe:

- [GO-2026-6090 / CVE-2026-56862](https://pkg.go.dev/vuln/GO-2026-6090)
  affects `crypto/tls` before Go 1.25.13. A malicious client can repeatedly
  send `KeyUpdate` messages and force indefinite key derivation. The controller
  listens with `ListenAndServeTLS`; this work happens during TLS processing,
  before the HTTP handler's registered-agent authorization can help.

The source declares Go 1.25.0, the locally built binaries (including the plugin
artifact) were built with Go 1.25.1, and all are affected. The release workflow
reads the version from `go.mod`. The official
[setup-go version-file documentation](https://github.com/actions/setup-go/blob/main/docs/advanced-usage.md#using-the-go-version-file-input)
says a patch version in the `go` directive selects that exact patch, so a new
release built by this workflow can regress from the local 1.25.1 artifact to
1.25.0 rather than selecting the current security patch.

The scan also reaches two current SSH denial-of-service advisories,
[GO-2026-6354](https://pkg.go.dev/vuln/GO-2026-6354) and
[GO-2026-6355](https://pkg.go.dev/vuln/GO-2026-6355), through
`golang.org/x/crypto/ssh v0.55.0`; both are fixed in v0.56.0.

**Impact:** an unauthenticated network client can consume controller resources
before mutual-TLS identity is established. Other advisories affect outbound
TLS/SSH, parsing, and template paths with varying application-specific
preconditions.

**Recommended fix:**

1. Build and release with Go 1.25.13 or newer. Either pin that exact security
   patch or use a `go 1.25` selector plus `check-latest: true` and record the
   resolved version in the release evidence.
2. Upgrade `golang.org/x/crypto` to v0.56.0 or newer and run `go mod tidy`.
3. Add `govulncheck ./...` as a release gate and fail on reachable known
   vulnerabilities. Verify each produced artifact with `go version -m` before
   signing and publishing it.
4. Rebuild and redeploy every binary; changing `go.mod` alone does not repair
   already-built executables.

## Remediation order

1. **SEC-06:** disable SQL client commands and remove MySQL-root imports.
2. **SEC-09:** persist cPanel lifecycle events before enabling user
   self-service on accounts whose ownership may be ambiguous.
3. **SEC-10:** rebuild all artifacts with patched Go and `x/crypto` versions.
4. **SEC-07:** bind every assignment and report to a unique lease attempt.
5. **SEC-08:** bind destructive retention approval to the reviewed policy.

Before deploying to the `.144` server, convert each positive reproduction into
a permanent negative regression test, build a signed candidate, and exercise
the cPanel create/remove/recreate and granular database-restore paths using
disposable accounts and databases. Do not test SEC-06 against a live account or
with a command that changes real system state.

*Afterwards:* the five fixes were made in that order and each regression test
was checked against the unfixed code. The `.144` server runs the rebuilt agent.
The create/remove/recreate and granular database-restore paths were exercised
in tests with a fake cPanel and a fake mysql client, not against live accounts
or databases on that server: creating and removing real cPanel accounts, and
stopping the service to reproduce the outage, are the operator's call to make
on a production host.

## Round three — SEC-11 to SEC-13

Three more findings, reported in conversation rather than as a file. Same
treatment: reproduced, fixed, and a permanent regression test checked against
the code as it was before the fix.

| ID | Severity | What | Fixed in |
| --- | --- | --- | --- |
| SEC-11 | Critical | A backup of less than the whole account was indistinguishable from a full one | `44daf4e` |
| SEC-12 | Critical (fleet) | A restore outliving its six-hour lease was reclaimed and the destructive attempt queued again | `5b13f64` |
| SEC-13 | High | A granular restore that failed partway reported only the failure, not what it had already overwritten | `312a5e1` |

### SEC-11 — partial backups could masquerade as full ones

A schedule can leave the databases, the home directory or the mail out. The
snapshot carried no sign of it, so three readers could not tell:
whole-account recovery picks by account and date and took the newest, handing
an operator rebuilding a customer an account with no databases and a restore
that reported success; the nightly rehearsal read "no dumps" as "this account
has no databases" and passed, lighting the same verified mark a whole account
gets; and restic groups retention by tags, so a partial run counted towards
the keeps and could evict a complete backup.

The backup now tags the snapshot with what it was taken without
(`skip:databases`, `skip:homedir`, `skip:email`), and all three use it.
Recovery takes the newest backup that holds the whole account and names what
is missing when there is none. The rehearsal checks a backup against what it
claims to hold and records that the account is not proved. Partial runs land
in a retention group of their own.

Only what the schedule asked to skip is tagged — an account that simply has
no databases is a full backup of that account. Snapshots written before this
carry no tag and are read as complete; there is nothing in them to say
otherwise. No policy on `.144` skips anything, so its existing history is all
full.

Tests: `internal/agent/skip_tags_test.go`, `internal/node/audit_bug_test.go`,
`internal/reassemble/verify_scope_test.go`.

Two places still read a snapshot without asking what it holds. They matter
only once a schedule is set to skip something, which none does today, so they
are recorded rather than fixed:

- The snapshot list on the restore page does not mark a partial one. The
  refusal guards the by-date path; an operator picking a snapshot by hand can
  still pick one that is missing part of the account.
- The rehearsal chooses the repository by newest snapshot of any kind, then
  the newest complete one inside it. A repository whose newest snapshot is
  partial wins the choice, and a complete backup in another repository is
  never rehearsed.

### SEC-12 — a restore could outlive its lease and be run twice

An agent now says it is still working, at half the remaining lease, and only
the attempt holding the job may extend it: the renewal carries the claim
token and is checked against the same server ownership a report is. A refusal
cancels the work's context — continuing to write into a live account with no
claim on it is what the token was added to forbid. An unreachable controller
is not a refusal, and the work carries on.

And if a lease does run out anyway, a restore that was writing into the live
account is no longer requeued: it is marked failed, saying it may have partly
run. A restore that only produces a copy is requeued as before.

The failure message is a fallback, not an account of what happened. When the
lease is taken back the claim token goes with it, so the agent's own report —
which does list what it wrote — is refused when it finally arrives. The row
says the restore may have partly run; it cannot say which part. Renewal is
what keeps this path rare.

Tests: `internal/store/lease_test.go`, `internal/agent/hold_lease_test.go`
(the last under `-race`).

### SEC-13 — a half-finished restore disclosed nothing

Writing part of an account back is not transactional and cannot be: cPanel
has no way to undo a loaded database. Every failure path after the first
write discarded what had already happened and reported only the error.

What was written is now carried out of every failure path. The error names
the item that failed and says the account has already been changed, the
report carries the list in its detail, and `Applied` is true whenever the
live account was touched. Migration 0007 adds the `detail` and `applied`
columns fleet mode had nowhere to put them in; standalone has recorded both
since it was written.

The customer is told too. The hint is the one line shown on their own page in
place of the operator's error, and it now names what was already put back and
says to check the account first. Reporting the change to the operator alone
would be the same non-disclosure one audience over, and the list holds nothing
but the account's own names.

Tests: `internal/agent/partial_putback_test.go`.

### What an operator has to do

- Fleet mode only: run migration 0007 before deploying a controller built
  from this. `.144` is standalone and unaffected.
- Rehearsals of accounts whose schedule skips part of the account will stop
  showing as verified, and say what was left out instead. That is the true
  state, not a regression.

### Also fixed alongside these

A cPanel account created while the service was down had its ownership
boundary recorded on replay and nothing else: the initial backup the live
hook queues was never asked for, so the account waited for the next nightly
run with no copy of it anywhere. The replay now queues it, on the same terms
as the hook. Test: `TestAReplayedCreateQueuesTheInitialBackup` in
`internal/node/lifecycle_test.go`.

*Not exercised on the live host:* a restore that outlives its lease needs a
controller and a fleet, which `.144` does not run; and overwriting a real
customer's databases to watch a granular restore fail halfway is not a test
to run on a production server.
