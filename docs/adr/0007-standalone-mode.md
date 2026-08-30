# 7. Standalone mode: one cPanel server, no controller

Date: 2026-08-30
Status: Accepted

## Context

Everything so far assumes a fleet: a controller with PostgreSQL, agents that
poll it, a maintenance host that holds delete-capable credentials. That is
the right shape for fifty cPanel servers. It is a great deal of apparatus
for one.

The common first case is a single cPanel server backing up to a single
remote destination, administered by the person sitting in WHM. Asking them
to stand up PostgreSQL and a second host to get there is asking too much.

## Decision

**A standalone mode**, run by the same agent binary, in which one cPanel
server is self-contained:

| | Fleet mode | Standalone mode |
|---|---|---|
| State | PostgreSQL on the controller | bbolt file on the cPanel server |
| Scheduling | controller's scheduler | the agent's own |
| Configuration | controller CLI or web UI | WHM plugin |
| Credentials | controller vault, delivered per job | local vault, same envelope encryption |
| Maintenance | separate trusted host | the cPanel server itself |

**The UI is served over a unix domain socket**, not a TCP port. A WHM plugin
CGI proxies to it. cPanel servers are multi-tenant by definition — the
untrusted users are already on the box — and a loopback TCP port holding
every destination credential in the fleet would be reachable by all of them.
The socket lives in a `0700` directory, is itself `0600`, and the CGI
additionally refuses any request whose WHM user is not root.

**On-disk layout is identical to fleet mode.** Same staging root default,
same `stage-<account>` key, same `account:` and `mode:` snapshot tags, same
rule of pointing restic at the database *directory*. This is a compatibility
invariant, not a coincidence: a server that later joins a fleet must keep
using the repositories it already has, and its snapshots must fall in the
same retention groups they always did.

## Rationale

Reusing the agent's internals rather than writing a second implementation is
most of the argument. The parts that make backups correct — payload
planning, staging preflight, the restic runner, reassembly, the chunker rule
— are the parts hardest to get right, and there is now an end-to-end suite
proving them. Standalone mode changes where state lives and who schedules,
and nothing else.

## Consequences

- **Append-only protection is weakened, and this must be said plainly.**
  In fleet mode the credential that can delete backups lives on a separate
  host, so an attacker with root on a cPanel server cannot destroy history.
  In standalone mode retention runs on the cPanel server, so that credential
  is on the machine we were defending against. Standalone trades the
  ransomware guarantee for having one machine instead of three. An operator
  who needs the guarantee runs fleet mode, or points standalone at an
  append-only endpoint and prunes from somewhere else entirely. The UI says
  so where a destination is configured as append-only.
- **Moving the staging root orphans retention groups.** Snapshot paths embed
  it, and restic groups by path. Changing it after the first backup means
  the old snapshots form a group that current retention will never prune.
  Treat the staging root as fixed once chosen.
- The chunker-parameter rule from ADR 2 has to be reimplemented over bbolt.
  The database trigger that enforced it in fleet mode does not exist here.
- The WHM plugin's AppConfig registration was verified on a live WHM
  (AlmaLinux 8, cPanel 136). Two keys decide whether the plugin is visible
  at all, and both fail silently when wrong: `acls` is required and an
  invalid value is discarded rather than rejected, and `entryurl` is what
  WHM links to from its Plugins section. The installer now checks that WHM
  kept both, because a registration that exists and shows nothing is worse
  than one that fails. Registration is against `software-cprest`, an ACL no
  reseller holds and root does; the CGI enforces root itself as well.
- How a request reaches the plugin is settled separately in
  [ADR 8](0008-query-string-routing-under-whm.md): cpsrvd will not route a
  path after the script name, so routes travel in a query parameter.
- Two state stores now exist for the same concepts. Their types deliberately
  share field names and semantics, so migrating a standalone server into a
  fleet is a data copy rather than a translation.
