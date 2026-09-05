# cP:Restic — high-severity security and bug review

Date: 2026-09-05  
Status: findings only; fixes have not been implemented by this review.

## Scope and evidence

Reviewed the local codebase, concentrating on cPanel/WHM privilege boundaries, account lifecycle isolation, backup preparation, restore extraction, and fleet job authorization.

The repository changed during the review. The initial inspected revision was `fc016a7`; the final reproduction run used the working tree at `bb2d368923a5dcc824ad487607bf4559e7d982cd`. Findings below were reproduced against that final working tree.

All probes used temporary local files, synthetic account records, or a disposable PostgreSQL cluster. No connection was made to `182.54.236.144`, no real credentials were read, and no production backup or account was changed. Application source files were not modified by the audit; the only repository file added is this report.

Six local reproductions confirmed five security issues and one restore reliability bug. Severity is assessed as **High** for each, with prerequisites stated explicitly. Two issues can have root-level consequences; an end-to-end production server takeover was not attempted or established.

| ID | Severity | Finding | Affected area |
| --- | --- | --- | --- |
| SEC-01 | High | Tenant-controlled exclusion file exposes privileged file contents in process arguments | Standalone backup preparation |
| SEC-02 | High | Archive symlinks escape extraction and overwrite files outside staging | Account reconstruction, granular recovery, restore drills |
| SEC-03 | High | Recreated account inherits access to the previous owner's recovery downloads | cPanel self-service recovery |
| SEC-04 | High | Tenant-controlled FIFO blocks the shared backup worker | Standalone scheduling and backup processing |
| SEC-05 | High | One registered fleet server can forge another server's job results | Fleet controller API |
| BUG-01 | High | Monolithic account recovery reports success with a nonexistent download path | Full-account download workflow |

## SEC-01 — Privileged file disclosure through backup exclusions

**Locations:** [internal/cpanel/excludes.go](../internal/cpanel/excludes.go), `NativeExcludes` and `readExcludeConf`, lines 33–79; [internal/node/node.go](../internal/node/node.go), `assignmentFor`, line 539; [internal/resticrun/args.go](../internal/resticrun/args.go), `BackupArgs`, lines 74–75.

The root service opens `<account-home>/cpbackup-exclude.conf` using `os.Open`. That file is controlled by the account. The reader follows symlinks, accepts files outside the home directory, and does not check whether the account itself could read the target.

Each non-comment line is converted to an exclusion pattern and eventually passed to restic as a literal `--exclude` command-line argument. A symlink to a privileged configuration file therefore copies its contents into a process's argument list.

**Prerequisites:** an attacker can create or replace the exclusion file in their own home directory, a scheduled backup reaches that account, and the host exposes the backup process's arguments to that tenant. A compromised website running as the account can meet the filesystem prerequisite without a cPanel browser session. Process visibility must be checked separately on hosts using additional `/proc` isolation.

**Impact:** disclosure of root-readable configuration and secrets. In particular, the shipped service explicitly uses `HOME=/root` because cPanel stores MySQL administration credentials in `/root/.my.cnf`. Disclosure of those credentials can compromise the server's database isolation. The proof did not use that real file.

**Local reproduction:** create a mode-0600 synthetic private configuration file outside a temporary account home, containing a unique password marker; make the account's exclusion file a symlink to it; call the real `NativeExcludes` and `resticrun.BackupArgs`. The resulting command arguments contain the password marker.

**Confirmed result:** `TestAuditExcludeSymlinkLeaksPrivateFileToArgv` passed. This proves the privileged-file-to-argument data flow; cross-UID observation of a real root process was not exercised.

**Recommended fix:** read the per-account file under the account's filesystem permissions, or open it through a directory-confined descriptor with symlink refusal and explicit ownership/type checks. Handle the administrator-owned exclusion file separately. Do not rely on process-argument hiding as the primary fix. Add a regression test asserting that a symlink to a private file never contributes its contents to arguments, diagnostics, or logs.

## SEC-02 — Archive extraction can write outside staging

**Locations:** [internal/reassemble/tar.go](../internal/reassemble/tar.go), `extractTarFiltered`, especially lines 112–138 and `safeJoin`; [internal/reassemble/reassemble.go](../internal/reassemble/reassemble.go), `restoreSplit`; [internal/agent/items.go](../internal/agent/items.go), `restoreItems`; [internal/maintenance/drill.go](../internal/maintenance/drill.go), `Drill`, line 80.

The extractor validates entry paths lexically, then performs ordinary filesystem operations that follow symlinks. Its symlink validation checks a joined, normalized path, but `os.Symlink` receives the original link target.

An absolute symlink target passes the current check: Go's `filepath.Join` combines the components into an apparently in-tree path, while the actual symlink still points to the original absolute location. A subsequent regular archive entry beneath that link reaches `os.OpenFile(..., O_TRUNC, ...)` outside the extraction directory.

**Prerequisites:** the restore or drill processes a malicious metadata archive, for example a backup produced by a compromised source server. The review did not establish that an ordinary tenant can arbitrarily replace the metadata archive generated by `pkgacct`.

**Impact:** creation or overwriting of files with the extracting process's privileges. On the cPanel agent this is root. A malicious backup can therefore affect the recovery host before `restorepkg` runs. Download-only recovery is also affected, because extraction happens while preparing the download. cPanel's restricted-restore option cannot protect operations that occur before its invocation.

**Local reproduction:** build a tar containing an absolute symlink at `cpmove-c1/dnszones/link`, pointing to a temporary directory outside the extraction root, followed by a regular entry at `cpmove-c1/dnszones/link/marker`. Call `ExtractMembers` with the legitimate `dnszones/` selection. The existing outside marker is overwritten and extraction returns success.

**Confirmed result:** `TestAuditAbsoluteSymlinkWritesOutsideExtraction` passed. Only a disposable marker was overwritten; no operating-system file was targeted.

**Recommended fix:** enforce containment during filesystem operations, not just string normalization. Use directory-confined operations such as suitable `os.Root` APIs or descriptor-relative opens that reject symlink escapes. Reject absolute and escaping link targets. Cover symlink ancestors, duplicate entries, link chains, and both full and filtered extraction in regression tests; rejecting only absolute targets is not a complete containment strategy.

## SEC-03 — Account-name reuse exposes the former customer's downloads

**Locations:** [internal/webui/user.go](../internal/webui/user.go), `handleUserHome`, lines 542–559, and `handleUserDownload`, lines 958–969; [internal/node/identity.go](../internal/node/identity.go), `AccountRemoved`, `AccountCreated`, and `UserSnapshots`.

Snapshot listing and new restore requests apply the account's identity/ownership window. Existing recovery outputs do not. The homepage lists restore records by the account-name string, and the download handler checks only `restore.Account == accountOf(r)`.

Account removal retires the identity and clears baskets, but leaves retained outputs and their restore records available. Recreating the same username establishes a new identity without preventing access through these two handlers.

**Prerequisites:** a former customer has a retained recovery archive, the username is later assigned to a different customer, and the new account can use cP:Restic. A different Unix UID does not prevent the issue.

**Impact:** the new customer can download the former customer's recovered files, databases, mail, or full-account archive. The homepage reveals the restore identifier when the record falls within its recent-history window, so the customer need not guess that identifier.

**Local reproduction:** store a successful recovery output for `customer1` under UID 1001, call `AccountRemoved`, recreate the same name under UID 2002 with `AccountCreated`, and invoke the account homepage and download handlers as the new `customer1`. The homepage contains the old restore ID and the download returns HTTP 200 with the former customer's marker data.

**Confirmed result:** `TestAuditRecycledUsernameDownloadsPreviousOwnersArchive` passed, including verification that the identity had been marked recycled.

**Recommended fix:** bind restore requests, outputs, history visibility, and download authorization to an account incarnation rather than a reusable username. Preserve historical outputs for administrators while denying them to subsequent owners. Revalidate the incarnation when queued self-service work executes, as well as when it is requested and downloaded. A timestamp-only check against when the output was generated is insufficient for an administrator rebuilding an older customer's snapshot after the name is reused.

## SEC-04 — An account-controlled FIFO can stop all backup processing

**Locations:** [internal/cpanel/excludes.go](../internal/cpanel/excludes.go), `readExcludeConf`, lines 63–79; [internal/node/node.go](../internal/node/node.go), `assignmentFor`, line 539; [internal/node/run.go](../internal/node/run.go), `runBackup` and `Run`, lines 875–904.

The same account-controlled exclusion path is opened synchronously without a regular-file check, nonblocking open, or read deadline. If it is a FIFO with no writer, the open waits indefinitely. This happens during assignment preparation, before the backup is handed to the worker's timed storage operations.

The standalone engine drains work sequentially through `RunOnce`. A blocked exclusion read therefore prevents subsequent accounts' backups and restores from being processed and prevents the engine from reaching its next scheduling pass. The web interface may remain available, masking the scope of the stalled worker.

**Prerequisites:** an account process can replace its own exclusion file with a FIFO and a backup of that account is attempted. A cPanel session is not required.

**Impact:** persistent loss of backup and restore processing across the standalone server. Restarting alone does not remove the FIFO; another attempt to back up that account can block again.

**Local reproduction:** create a FIFO at the temporary account exclusion path and call `NativeExcludes` in a goroutine. It remains blocked; connecting and closing a writer releases the probe. The unbounded wait follows from the blocking open and absence of a cancellation mechanism, rather than merely from a slow test run.

**Confirmed result:** `TestAuditExcludeFIFOBlocksBackupPreparation` passed. The test released its FIFO and did not leave a blocked worker behind.

**Recommended fix:** open account-controlled inputs without blocking on special files, validate the opened descriptor as a permitted regular file, and impose input-size/line-count limits. Avoid an `Lstat`-then-ordinary-`Open` sequence, which leaves a replacement race. Ensure preparation failures are isolated to the affected account.

## SEC-05 — Fleet job reports are not authorized against their server

**Locations:** [internal/controller/service.go](../internal/controller/service.go), `ReportRestore`, lines 111–141, and `Report`, particularly line 226; [internal/store/restores.go](../internal/store/restores.go), `ApplyRestoreReport`, lines 180–191; [internal/store/jobs.go](../internal/store/jobs.go), `ApplyReport`.

The API authenticates an agent certificate and obtains its server ID. The reporting service receives that ID but only uses it in logs. Database updates select jobs by the submitted job ID, without restricting the job to the authenticated server or checking that it is currently leased to that server.

**Prerequisites:** a registered agent can submit reports and knows a target job UUID belonging to another server. The UUID-discovery prerequisite remains; this review did not demonstrate an identifier-enumeration endpoint.

**Impact:** a compromised fleet agent can falsify another server's backup or restore results. A pending restore can be marked successful before any agent performs it, removing it from work eligible for execution. Backup targets can also be forced to fail through a forged staging-error report. This undermines both recovery and the controller's status history.

**Local reproduction:** create two registered servers in a disposable PostgreSQL database. Queue a restore and a backup for the first server. Pass the second server's ID to the real reporting service with the first server's job IDs. The unclaimed restore becomes successful and the backup becomes failed.

**Confirmed result:** `TestAuditDifferentServerCanForgeJobReports` passed against the real service and persistence implementations. Certificate transport was not part of this probe; the API-to-service call path was inspected separately.

**Recommended fix:** authorize and update reports transactionally against the authenticated server, the account owning the job, and the active lease/attempt. Reject reports for unclaimed, foreign, expired, or already completed attempts. Apply the rule to backup targets as well as restore outcomes and test it with two independently registered servers.

## BUG-01 — Monolithic full-account downloads point to a missing file

**Locations:** [internal/agent/restore.go](../internal/agent/restore.go), `RunRestore`, lines 119–128; [internal/reassemble/reassemble.go](../internal/reassemble/reassemble.go), `restoreMonolithic`, lines 224–247.

A monolithic snapshot's account archive is restored under `<workdir>/archive/`. Retaining the work directory preserves that layout, but the reported archive path is rebuilt using only `filepath.Base(result.ArchivePath)`. That discards the required `archive/` directory.

The resulting layout is:

```text
Actual:   keep-restore-c1/archive/cpmove-c1.tar
Reported: keep-restore-c1/cpmove-c1.tar
```

**Trigger:** a download-only full-account restore from a monolithic snapshot. This includes accounts that fall back to monolithic backups because they have PostgreSQL databases, as well as explicitly selected compatibility-mode backups.

**Impact:** the job reports success but the interface cannot collect its output. The archive still exists on disk and an administrator can retrieve it manually; this finding does not establish loss of the stored backup or failure of the direct Apply workflow.

**Local reproduction:** run the real `Agent.RunRestore` with a synthetic monolithic snapshot and a restic executor that writes the archive to the requested target. The result says `success`, the reported file does not exist, and the real archive exists one directory deeper.

**Confirmed result:** `TestAuditMonolithicDownloadPointsToMissingFile` passed.

**Recommended fix:** compute the archive's relative path beneath the original work directory and preserve that relative path beneath the retained directory. Verify the final reported file exists before reporting success. Exercise the download workflow with both split and monolithic snapshots.

## Reproduction artifacts

The audit tests were supplied through Go's overlay mechanism, without adding test files to the application packages. Their temporary location is:

```text
/tmp/cprest-audit-20260905-5eVV6A/
  overlay.json
  cpanel_review_test.go
  reassemble_review_test.go
  webui_review_test.go
  store_review_test.go
  agent_review_test.go
```

From the project root, while those temporary files remain available:

```bash
go test -overlay=/tmp/cprest-audit-20260905-5eVV6A/overlay.json \
  ./internal/reassemble ./internal/cpanel ./internal/webui \
  ./internal/store ./internal/agent \
  -run '^TestAudit' -v -count=1
```

These are **positive reproduction tests**: a pass means the vulnerable behavior was reproduced, not that the implementation is secure. The PostgreSQL probe needs a local PostgreSQL installation; the helper otherwise skips it. PostgreSQL was available and the probe executed during this review. Restic execution was simulated where noted, and actual cPanel/WHM binaries were not exercised.

## Recommended order

1. Fix the account-controlled exclusion reader, addressing both credential disclosure and FIFO blocking.
2. Replace lexical-only archive containment with safe filesystem operations.
3. Bind retained downloads and queued self-service work to account incarnations.
4. Enforce server and lease ownership on fleet reports before relying on multi-server isolation.
5. Correct the monolithic retained-output path and validate successful downloads.

Before deploying fixes to the production/test `.144` server, convert the reproductions into permanent regression tests that assert rejection or correct behavior, and validate the changed cPanel integrations with a controlled account.
