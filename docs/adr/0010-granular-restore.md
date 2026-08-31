# 0010 — Granular restore

Status: accepted
Date: 2026-08-31

## Context

Until now a restore was all or nothing: rebuild the account archive, then
either collect it or hand it to `restorepkg`. The common case is not that.
Something was deleted this morning — one mailbox, one database, a zone
file, a directory under `public_html` — and everything else on the account
is current. Replacing the whole account to recover one of those loses
every change made since the backup.

The split payload already decomposes an account, so a snapshot holds the
parts separately:

    /var/lib/cprest/staging/stage-backup-<user>/metadata     cpmove-<user>.tar, no homedir, no databases
    /home/<user>                                             the home directory, backed up in place
    /var/lib/cprest/staging/stage-backup-<user>/databases    one .sql per database

Everything an operator asks for maps onto those three, and the metadata
archive's own layout was read off a live cPanel 136.0.37 rather than
assumed:

| Asked for | Comes from |
|---|---|
| Files or folders | `/home/<user>/…` |
| Website files | `/home/<user>/public_html` |
| A mailbox | `/home/<user>/mail/<domain>/<box>`, plus `va/`, `vad/`, `vf/`, `meta/mailserver` |
| A database | `databases/<name>.sql` |
| DNS records | `dnszones/` |
| SSL certificates | `apache_tls/`, `ssl/`, `sslcerts/`, `sslkeys/`, `has_sslstorage`, `autossl.json` |
| Account settings | `cp/`, `userdata/`, `meta/`, `cron/`, `quota`, `shell`, `version` |

## Decision

`internal/granular` turns a request into snapshot paths and archive
members. It runs nothing and touches no disk, so the mapping is tested
against known snapshot layouts instead of against a live cPanel.

Two rules are enforced there rather than left to the caller:

- **A path must stay inside the account's home directory.** Every account
  on a cPanel server is a different customer, and a restore steered at
  `/home/someone-else` would hand one customer's files to another. A
  database is a name, never a path, for the same reason.
- **An impossible request fails.** Asking for a database from a snapshot
  with no dumps, or DNS from one with no metadata, is an error — not an
  empty plan that would report success having restored nothing. The agent
  repeats the check on the result: zero files out means the restore
  failed, whatever restic said.

What comes back is left on the server as a tree and an archive of it:

    homedir/mail/example.com/sales/…
    databases/customer1_wp.sql
    metadata/cpmove-customer1/dnszones/example.com.db

## Nothing is applied

A granular restore never writes into the live account. That is a decision,
not an omission:

- restic restores as root, and `/home/<user>` needs the account's own
  ownership — copying files back in place without fixing that leaves an
  account that cannot read its own site.
- Putting a zone file back is a DNS change, and installing a certificate
  is an SSL change. Both belong to the interfaces that own them.
- Importing a dump over a live database destroys whatever is in it now.

So the archive is the deliverable, the same way it is for a whole-account
restore that was not asked to apply itself. Applying each kind in place is
worth doing on purpose, later, with its own confirmation.

## Verified

Each kind was run against the production server's own backups: one file,
`public_html` (3,871 files), a domain's mail, a database dump, the zone
file, the certificates, and the account configuration — then downloaded
through the plugin and unpacked.
