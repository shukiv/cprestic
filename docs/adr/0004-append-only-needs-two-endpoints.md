# 4. Append-only needs two endpoints, and retention needs stable snapshot identity

Date: 2026-08-30
Status: Accepted

## Context

Two assumptions in the original design turned out to be wrong. Both were
found by the end-to-end suite rather than by reading the code, and both fail
silently in production: backups keep succeeding while storage grows without
limit.

**Assumption 1: the maintenance runner can prune an append-only destination
because it holds delete-capable credentials.**

It cannot. `rest-server --append-only` is a property of the running process,
not of a credential. With it enabled the endpoint returns HTTP 403 to every
`DELETE`, whoever is asking. A destination reached only through an
append-only endpoint can never be pruned by anyone.

**Assumption 2: `restic forget` with a keep policy will prune old
snapshots.**

Only if the snapshots being compared fall into the same group. Restic groups
by host and paths by default. Staging was keyed by job id, so every night's
snapshot recorded a different path, landed in a group of its own, and was
kept as that group's most recent snapshot. Adding a `job:<id>` tag would
reintroduce the same fault under `--group-by tags`.

## Decision

1. An append-only destination records two addresses: `base_url`, which
   agents reach, and `maintenance_base_url`, a second rest-server over the
   same data directory with append-only off, reachable only from the
   management network. `repobuild.OpenForMaintenance` applies the override;
   agents never see it.
2. Staging is keyed by account, not by job, so a snapshot's paths repeat
   between runs.
3. Snapshot tags carry only stable facts — `account:<user>` and
   `mode:<payload mode>`. The job a snapshot came from is recorded in the
   database against its snapshot id.
4. The maintenance runner passes `--group-by host,tags`, giving one
   retention group per account per payload mode.

## Rationale

The security boundary for deletes is now network reachability rather than
credential scope. That is weaker than per-credential rights would be, but
per-credential delete rights are not something rest-server offers, and the
alternative — no append-only at all — trades a real ransomware defence for a
theoretical one.

Keying staging by account is safe because the controller already refuses to
queue a second job for an account that has one open, so two runs cannot
share a staging directory.

## Consequences

- An operator who omits `maintenance_base_url` on an append-only destination
  gets a hard failure from `restic forget`, not silent growth. That is the
  intended behaviour; it is not automatically detectable at configuration
  time, because a destination may legitimately be append-only with retention
  handled outside cprest.
- Snapshot paths are now stable, which additionally lets restic find a
  parent snapshot and skip rescanning unchanged files.
- Tracing a snapshot back to its job goes through the database rather than
  through restic tags.
