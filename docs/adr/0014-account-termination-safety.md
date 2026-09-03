# 0014 — Account termination needs evidence before deletion

Status: accepted
Date: 2026-09-03

## Context

The post-stage `Accounts::Remove` hook can retire an identity and reconcile
named schedules, but it runs after cPanel has deleted the customer's live
data. At that point a missing or stale backup is only a diagnosis.

cPanel exposes a pre-stage `Whostmgr::Accounts::Remove` Standardized Hook and
allows it to block the operation. Blocking is not valid on a manually
registered script: the executable must answer `--describe`, declare
`blocking: 1`, and include `BAILOUT` in its failure message. This is the
documented [script descriptor protocol](https://api.docs.cpanel.net/guides/guide-to-standardized-hooks/guide-to-standardized-hooks-the-describe-method)
and [blocking action contract](https://api.docs.cpanel.net/guides/guide-to-standardized-hooks/guide-to-standardized-hooks-hook-action-code).

A pre-hook is synchronous. Starting `pkgacct` there would leave an account
deletion waiting for minutes or hours, risk cPanel timing it out, and couple a
destructive administrative action to remote storage availability.

## Decision

The installed hook executable describes six registrations; four concern
creation and removal safety:

- post-create records the ownership boundary and queues an initial backup;
- post-modify reconciles renames;
- pre-remove performs the optional safety decision;
- post-remove retires the identity and reconciles named schedules.

The suspension and unsuspension registrations are covered by ADR 15.

Termination protection is off by default and is enabled in WHM Settings. An
upgrade therefore does not silently change whether cPanel can remove an
account.

When enabled, the pre-hook reads local bbolt state only and allows removal
when all of these are true:

1. At least one enabled schedule covers the account, names a destination, and
   includes home files, databases and email.
2. Every repository promised by those full-account schedules has a successful,
   non-incomplete target from a wholly successful job.
3. Each copy is newer than its schedule's freshness window: the explicit
   no-backup threshold when configured, otherwise twice the schedule interval.

Each job records whether that particular run staged the whole account. The
decision does not infer this from the current schedule, because an operator may
have changed exclusions after the snapshot was made. Records written before
this field existed conservatively do not count, so a newly enabled gate needs
one new successful full backup.

The handler returns HTTP 409 for a safety refusal. The hook turns definite
4xx decisions into a single-line `0 BAILOUT ...` result after removing line
breaks and any injected `BAILOUT` text from the detail. Decisions are retained
in the bounded lifecycle history without the raw cPanel payload.

The WHM Accounts list evaluates every account from one policy-and-job-history
snapshot and displays the same decision as the hook. An account's detail page
also states why termination is safe or what evidence is missing. These are
advisory previews: the pre-hook always evaluates persisted state again at the
moment cPanel asks to remove the account.

When copies are missing or stale, **Prepare for termination** computes the
smallest set of enabled full-account schedules whose repositories cover every
gap. This is an exact set-cover plan, not merely the widest first schedule.
The plan is inserted into bbolt atomically with stable ordering, and the
standalone worker runs its jobs sequentially. The action only queues backups;
it never invokes cPanel account removal. If there is no applicable full-account
schedule, WHM explains that configuration is required instead of offering an
action that cannot make the gate pass.

Queue admission is protected around both the busy check and insertion. This
prevents simultaneous WHM requests from each observing an idle account and
then staging a backup and restore over the same directory.

Service unavailability and internal 5xx responses fail open: the hook logs the
problem and returns success to cPanel. This gives up strict protection during a
service outage, but prevents a restart, damaged state database, or broken
plugin upgrade from wedging WHM account administration. Operators requiring a
hard fail-closed deletion control need an independent policy layer outside
this plugin.

## Consequences

- The check is fast and does not contact cPanel, restic, or any destination.
- Rendering many accounts performs one batched safety evaluation rather than
  rereading all job history for every account.
- A multi-schedule preparation plan remains one logical account operation:
  another backup or restore cannot be queued over it, and its jobs run in
  sequence.
- A successful copy made under a partial-payload schedule cannot authorize
  deletion.
- Adding a destination to a full schedule immediately makes that destination
  part of the deletion promise; a backup must reach it before removal.
- An operator can intentionally bypass the safety policy by disabling it in
  WHM Settings. That action requires the same root-only interface as the rest
  of the plugin administration.
- Install and uninstall use `manage_hooks` descriptor mode, while retaining
  cleanup for the three legacy manual registrations during upgrades.
