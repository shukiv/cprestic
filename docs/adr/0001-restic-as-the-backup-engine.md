# 1. restic as the backup engine

Date: 2026-08-30
Status: Accepted

## Context

We need encrypted, deduplicated, incremental backups of cPanel accounts to
several kinds of remote storage, driven from many source servers.

Candidates considered: restic, borg, duplicity, rclone alone, and cPanel's
own backup transport.

## Decision

Use restic, invoked as a child process by our agent.

## Rationale

- Client-side encryption by default; the destination never sees plaintext.
- Content-defined chunking gives real deduplication across runs.
- One tool covers local, SFTP, REST, S3-compatible, Azure and GCS backends,
  so the destination abstraction stays thin.
- `rest-server --append-only` gives ransomware resistance without trusting
  the source server.
- borg needs its own agent on the far end and has weaker cloud-object-store
  support. duplicity's full/incremental chain makes long retention costly.
  rclone alone offers no deduplication or encryption of this kind. cPanel's
  own transport does not give us a fleet-wide, multi-destination story.

## Consequences

- We inherit restic's operational model, including per-repository locking
  and the requirement that pruning be done by a delete-capable client.
- Repository format and chunker parameters become long-lived commitments
  (see ADR 2).
- We must pin and track the restic version across the fleet, since
  repository features depend on it.
