# 0015 — Preserve suspended accounts without delaying cPanel

Status: accepted
Date: 2026-09-03

## Context

cPanel exposes post-stage `Whostmgr::Accounts::suspendacct` and
`Whostmgr::Accounts::unsuspendacct` Standardized Hooks. A suspension often
marks the boundary between an active account and later termination, making it
a useful moment for one more complete backup. It is not itself proof that the
account will be removed: billing systems can suspend many accounts
automatically and can later unsuspend them.

The hook runs inside an administrative cPanel operation. Waiting for
`pkgacct`, restic, or a remote destination there would make WHM and automated
billing workflows depend on a potentially long backup. The suspension payload
can also contain an operator-supplied reason and command output that are not
needed to protect the account.

## Decision

The installed hook executable declares post-stage descriptors for suspension
and unsuspension. Both events are recorded in the bounded lifecycle history,
using only the validated cPanel account name and cprest's result. Raw payloads,
suspension reasons, and command output are never persisted.

Suspension preservation is disabled by default and can be enabled in WHM
Settings. This avoids unexpectedly starting a burst of backups when an upgrade
is installed on a server whose billing system performs bulk suspensions.

When enabled, the suspension handler considers every enabled full-account
policy that covers the account. It computes the exact smallest set of those
policies whose repositories cover every destination promised by all eligible
policies, then inserts all pending jobs in one bbolt transaction. Stable
ordering makes the standalone worker run them sequentially. A partial policy
cannot be used, because a preservation backup must include home files,
databases, and email.

Queue admission shares the account work lock used by manual backup, restore,
termination preparation, and coverage repair. If work for the account is
already pending or running, the hook records that no duplicate was added.

The hook returns as soon as pending jobs have been written. It never waits for
archive creation or destination access, and it does not block the cPanel
suspension if preservation is disabled or no full-account policy applies.
Unsuspension records the lifecycle transition but does not queue another
backup.

## Consequences

- Enabling the feature can increase storage and backup activity when accounts
  are suspended, but it is an explicit operator choice.
- A multi-destination promise may require more than one job when no single
  schedule covers every destination.
- A repeated suspension notification cannot stack duplicate work for an
  account that is already busy.
- The lifecycle dashboard uses explicit event and outcome text in addition to
  color, so suspension state remains understandable without color perception.
- Operators can diagnose whether preservation was disabled, queued, skipped as
  duplicate work, or lacked an applicable policy without retaining sensitive
  cPanel hook payloads.
