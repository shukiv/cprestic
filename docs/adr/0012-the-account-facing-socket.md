# 0012 — What an account is allowed to reach

Status: accepted
Date: 2026-09-01

## Context

The account-facing interface is a unix socket at
`/var/run/cprest/account/user.sock`, mode `0666`, served by the same
process that runs backups as root. cPanel executes an account's
`.live.php` as that account, the socket reads who is asking with
`SO_PEERCRED`, and the account name never appears in a URL. That is the
good part: a customer cannot ask for somebody else's backups by editing a
parameter, because there is no parameter to edit.

The uncomfortable part is what sits behind the socket. Every cPanel
account on the server owns processes that can open it — a PHP script, a
cron job, anything the customer runs. Those processes are talking
directly to a daemon running as root, one that can read every account's
files and write to the backup destinations. The socket is a boundary
drawn inside a single process rather than between two.

A review proposed replacing this with a broker: a small unprivileged
process per account, or one process holding no credentials, that accepts
requests from accounts and forwards only what it is willing to ask the
root daemon for. The root daemon would then listen only on a socket no
account can reach.

## Decision

Not yet. The direct socket stays, with the boundaries it has now:

- **The peer must be a cPanel account.** `SO_PEERCRED` gives the uid,
  `user.LookupId` gives the name, and the name must have a file in
  `/var/cpanel/users`. root is refused; a system user is refused.
- **A suspended account is refused.** Its files are still backed up;
  the person behind it does not go on driving the service.
- **Feature Manager is an authorization boundary.** The daemon asks
  cPanel `verify_user_has_feature` itself and fails closed, because the
  cPanel-side page check can be bypassed by talking to the socket
  directly.
- **Team users are refused** at the cPanel page — best effort, and worth
  saying so plainly. A team user shares the owner's uid, so the daemon
  cannot tell one from the account owner; the page is the only place that
  sees `TEAM_USER`. It therefore stops a team user who uses the
  interface, and does not stop one who can run code as the account and
  open the socket directly. That is the shared-uid limitation, and unlike
  the checks above it is not enforced by the kernel.
- **One request in flight per account**, submitted forms are capped at a
  megabyte, and the socket has read and idle deadlines — a local process
  cannot hold the root service open or make it read forever.
- **A recycled username sees nothing from before the handover.**
- **What an account may ask for is a fixed list**, checked in the
  handler and not only rendered on the page.

## Why not the broker, yet

A broker is the right shape and a large change. It moves the attack
surface rather than removing it — something still has to decide what an
account may ask, and that logic is the same logic — and it buys its
safety by adding a process boundary around code that is already written
to assume it is the boundary. Doing it in the middle of a run of
correctness fixes would mean rewriting the part of the program that has
had the least testing, for a benefit that is real but second-order to a
program whose restores had never once been run against cPanel.

The honest statement of the current position: **an account that runs
code on this server is talking to a root daemon through a narrow,
enumerated interface.** Every request is attributed by the kernel, the
list of things it can ask is short and checked, and the daemon fails
closed. It is not the same as being unable to reach root at all.

## When to revisit

- If the enumerated interface grows much beyond restore-my-own-things —
  anything that takes a path, a pattern or a destination from the account
  rather than choosing from a list.
- If cP:Restic is ever run somewhere the operator does not control every
  account on the machine.
- Before adding any feature that would have an account's input reach
  restic's argument list.

## Consequences

The socket keeps its 0666 mode, because cPanel gives us no group shared
with every account. The mitigations above are what stands in its place,
and each of them is a thing that can be tested — several are, in
`internal/webui/access_test.go` and `internal/node/identity_test.go`.
The residual risk is stated here rather than in a comment nobody reads:
if one of those checks is wrong, the failure is an account reaching root,
not an account seeing a page it should not.
