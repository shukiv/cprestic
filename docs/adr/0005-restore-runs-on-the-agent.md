# 5. Restore runs on the agent, and does not apply itself

Date: 2026-08-30
Status: Accepted

## Context

Restore had a designed shape but no implementation. Three questions had to
be settled before writing any of it: where the restic process runs, how the
work is dispatched, and what happens to the result.

## Decision

1. **Restore runs on the cPanel server**, in the agent, through the same
   lease and credential path as a backup. One long-poll endpoint returns
   either kind of assignment, discriminated by `kind`.
2. **The controller chooses the source repository.** The agent is handed a
   repository and a snapshot id and never selects either.
3. **Applying to the live account is opt-in.** By default the agent rebuilds
   the cpmove archive and leaves it on disk. `restorepkg` runs only for a
   job created with `-apply`, and never for a single-file restore.
4. **Restores are serialised against backups per account**, because both
   stage under the account's name.
5. **Drills are structural.** The maintenance runner rehearses a restore
   into scratch space and asserts what can be asserted without cPanel.

## Rationale

Running on the agent reuses everything: mutual TLS, per-job credential
resolution, lease expiry and re-queue, the staging space preflight. A
destination-side runner would be faster for a very large account — it would
ship only the finished archive rather than pulling the whole payload through
the machine being restored — but it is a second deployment unit and a second
credential path for a benefit nobody has measured yet.

The controller choosing the repository keeps the trust boundary where backup
already put it: the agent is told where to write, so it should also be told
where to read. An agent that picked its own source could be steered to a
repository it should not open.

Opt-in apply is the important one. A restore that overwrites a live account
is destructive and irreversible; a restore that leaves an archive on disk is
not. Making the safe thing the default costs an operator one flag and
removes a whole class of accident.

## Consequences

- Restoring a large account moves its full payload through the cPanel
  server. This is recorded as an open question in DESIGN §14.
- The reassembly step depends on cPanel's cpmove layout. The top-level
  directory name is discovered from the extracted archive rather than
  assumed, and the `homedir/` and `mysql/` subdirectory names are
  constants. Those were verified on cPanel 136: a rebuilt archive's
  top-level entries are the same set that native `pkgacct` produces, with
  the home directory restored into `homedir/`. What remains unverified is
  whether `restorepkg` accepts one, which only a real restore can answer.
- Because reassembly extracts an archive produced on a server that may be
  compromised, extraction refuses path traversal and escaping symlinks and
  drops setuid bits.
- A drill passing means the backup can be rebuilt into something
  account-shaped. It does not mean cPanel would accept it.
