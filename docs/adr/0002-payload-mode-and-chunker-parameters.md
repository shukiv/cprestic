# 2. Payload mode and chunker parameters

Date: 2026-08-30
Status: Accepted

## Context

`pkgacct` produces a compressed archive by default. Restic deduplicates by
chunking the byte stream; a gzip stream has no stable chunk boundaries
between runs, so a nightly `.tar.gz` stores close to the full archive size
every night, in every destination repository.

Separately, restic can only deduplicate between two repositories that share
chunker parameters, and those parameters are fixed when a repository is
created and cannot be changed afterwards.

## Decision

1. Default payload mode is `split`: account metadata, home directory and
   per-database SQL dumps are backed up as separate parts, so restic sees
   real files.
2. `monolithic` mode is supported for archive fidelity, but only with
   pkgacct compression disabled. The flag is probed from `pkgacct --help`
   at enrolment rather than assumed, and a server that cannot disable
   compression is reported as degraded.
3. Every repository after a server's first is created with
   `restic init --from-repo <first> --copy-chunker-params`.

## Rationale

Point 3 costs nothing today and cannot be added later. We do not use
`restic copy` in v1 — agents upload to each destination independently — but
switching to "back up once, replicate" would otherwise require re-creating
every repository and re-uploading all history.

## Consequences

- `split` mode owns the reconstruction problem: restore must reassemble a
  cpmove structure before calling `restorepkg`, and must track cPanel's
  archive layout across versions.
- Repository provisioning is centralised in the maintenance runner, which
  enforces the chunker rule; the database enforces it a second time with a
  trigger.
- A server whose pkgacct cannot disable compression still gets backed up,
  loudly and expensively, rather than silently.
