# Destinations

A destination is a place backups go, plus the credentials to reach it and the
key that encrypts what lands there. One server can write to several.

## The four kinds

| Kind | Use it for | Needs |
|---|---|---|
| **Another Linux server (SFTP)** | a second box you already own | host, port, SSH user, and a key cP:Restic can generate for you |
| **Backup server (restic REST)** | a dedicated restic REST server, which can be made append-only | URL, credentials |
| **S3 or S3-compatible** | Backblaze B2, Wasabi, MinIO, AWS | endpoint, bucket, access key, secret |
| **Local disk or mounted NAS** | an attached disk or an already-mounted share | a path |

Each destination also takes a **folder inside the destination** — the
repository path. It defaults to this server's hostname, so several cPanel
servers can share one bucket without colliding.

## Adding one

**Destinations → Add a destination.** cP:Restic tests the connection, then
initialises the repository, before the destination is saved. A destination that
appears in the list is one that answered.

Then it shows the **recovery key** once. Keep it with your other break-glass
material. A destination whose key is lost is a destination whose backups are
noise.

## The list

Each row carries the name, where the backups actually are — repository path,
the machine under it, whether it was reachable and when that was last checked —
and how much room is left there.

Free space is measured honestly: `statfs` for a local path, `df -Pk` over SSH
for SFTP. Object stores do not report a size, so they say so rather than
inventing one.

**Edit** sits on the row. Everything else — test the connection, browse what it
holds, remove it — is under the row menu.

## Credentials

Stored encrypted with the key in `/etc/cprest/master.key`. **Back that file up
somewhere other than this server.** Without it the stored credentials cannot be
read, which on a rebuilt machine means re-entering every destination by hand —
and re-entering a recovery key you may not have kept.

## Reading backups another server made

That is [disaster recovery](restoring.md#disaster-recovery), on the Restore
page: attach the old server's destination and its recovery key, and its
accounts become restorable here.
