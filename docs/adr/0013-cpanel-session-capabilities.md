# 0013 — A Unix uid is not a cPanel login

Status: accepted
Date: 2026-09-01

## Context

ADR 0012 used `SO_PEERCRED` to bind the public account socket to a cPanel
account. That prevents one account selecting another account in a URL, but
it does not prove that a request came from the cPanel interface. A cron job,
a compromised website and the account owner all run with the same uid.
cPanel Team users also share that uid with the account owner, so the daemon
could not enforce a Team role.

The PHP page's `TEAM_USER` environment value is useful for presentation but
is not an authorization boundary. The account owns the PHP process and can
connect to the socket without running the shipped page.

cpsrvd stores live cPanel, WHM and Webmail sessions under
`/var/cpanel/sessions`. A cPanel session record binds an opaque session name
to the account, its `/cpsess...` security token, the cpsrvd application and,
for a Team login, the Team principal. The records are root-owned; an account
process cannot mint or edit one.

## Decision

Every account-facing operation requires two steps:

1. The LivePHP proxy calls the installed `Cprest/issue_capability` UAPI.
   cPanel deliberately does not expose the browser's `cpsession` cookie or
   `REMOTE_PASSWORD` to LivePHP on current releases, so a PHP cookie parser
   is not a viable integration boundary.
2. The UAPI enters cPanel's documented AdminBin privilege-separation path.
   The root-owned cP:Restic Admin module uses cPanel's session restoration
   routine to resolve the current server-side session from its account or
   Team principal and per-session security token. It sends the opaque
   session name, account, principal, HTTP method and exact
   request target to `POST /_cprest/user-capability` on cP:Restic's
   root-only admin socket. The account-facing PHP process never receives the
   session name or the root-owned session record.
3. The root daemon opens the corresponding root-owned cpsrvd session record
   and verifies the account and principal supplied by the root-only bridge,
   record user, security token,
   authenticated origin, cpaneld service and expiry. An owner is accepted.
   A root-possessed temporary account session created by WHM's “Log in to
   cPanel” flow is also accepted when its root-owned record carries cPanel's
   cPHulk registration, temporary-user credentials and `handle_form_login`
   origin. A Team principal is accepted only when the supported
   `Team/list_team` UAPI currently reports the Administrator role and the
   entry is active.
4. The daemon returns an HMAC-authenticated capability containing the
   account, principal, scope, method, target, session hash, issue time,
   expiry and a random nonce. It lasts 20 seconds and is consumed once.
5. The proxy presents that capability with the real request. The daemon
   matches it to `SO_PEERCRED` and to the exact method and target before the
   account handler runs.

The daemon still checks Feature Manager behind this boundary. The session
capability answers “is this an authorized cPanel principal now?”; Feature
Manager answers “did the host enable cP:Restic for this account?” Both must
allow the request.

Non-Administrator Team roles are refused. Restoring a backup can replace
files, databases, DNS, mail and credentials together; mapping that operation
to the Web, Database or Email role would give a narrow role account-wide
write power.

## Failure behavior

- Missing, malformed, pre-authentication, expired and mismatched session
  records fail closed.
- Session files must be regular, root-owned and not group- or world-writable.
- Malformed or unsuccessful Team UAPI responses fail closed.
- A capability is useless to another account, another route, another query,
  another method, after 20 seconds or after its first use.
- Session names and tokens are never written to the log.

If cPanel changes its session record schema, account access stops; the
daemon does not fall back to uid-only access. This integration must
therefore be exercised on each supported cPanel release during release
qualification.

## Consequences

The account socket remains mode `0666`, but an arbitrary account process can
no longer use it merely by sharing the owner's uid. It must also possess a
currently valid cPanel browser session. A process that steals that session
can act as the browser until cPanel revokes the session; the one-use,
request-bound capability prevents a captured cP:Restic capability from
being repurposed or replayed.

The daemon reads cPanel's root-only session state directly because cPanel
does not expose a portable API that validates an existing browser session
for an unrelated local root service while also returning its opaque
authenticator. The bridge uses cPanel's documented UAPI and AdminBin
extension points; the small session-restoration call follows cPanel's own
AdminBin base implementation so Team identity is derived from the same
server-side record. Team role data uses the supported Team UAPI. This is a
deliberate fail-closed cPanel integration, not a portable authentication
protocol.

cPanel's session loader cannot decrypt the record's `pass` field without an
obfuscation fragment that exists only in the browser cookie—and that cookie is
intentionally absent from LivePHP. The root-only bridge therefore sends the
random session name and per-session security token, not a blank or weakened
password substitute. cP:Restic re-opens that named root-owned record and checks
its remaining authentication markers. This also supports cPanel's documented
`create_user_session` flow, whose fully authenticated session records may omit
`pass`. The legacy account-socket exchange, if used by an older proxy during a
rolling upgrade, still requires and compares the full session authenticator.
