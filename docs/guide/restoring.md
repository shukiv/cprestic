# Restoring

Three tabs, because there are three different situations.

| Tab | The situation |
|---|---|
| **Restore account(s)** | the account is on this server, or was, and you want it back |
| **Deleted accounts** | cPanel no longer has it; the backups are still here |
| **Disaster recovery** | the machine is gone and this one is taking its place |

## Nothing is overwritten unless you ask

By default a restore **rebuilds** the archive and leaves it on this server for
you to inspect. Overwriting the live account, or handing the archive to
cPanel's own restore, is a separate tick with its own confirmation.

## Restore account(s)

Choose an account and a destination. The page then shows:

- **Accounts in this destination** — read from the backups themselves rather
  than from cPanel, so an account this server no longer has is in the list.
  Tick several and restore them together; they still run one after another.
- **Available backups** of the chosen account — pick one, or press *Pick files
  from it* to browse what is actually inside and tick the paths you want.
  Folders come back whole, and what you pick stays picked as you move between
  folders.
- **Restore one thing** — a single mailbox, database, DNS zone, cron job, SSL
  item or FTP setting, taken out of a backup without rebuilding the account.
  What comes back is left on this server to collect.

Restores run as restricted restores by default, because the archive holds the
account's own home directory and cPanel's restore runs as root. The
**unrestricted** tick exists for the case where a restricted restore refuses
something the account legitimately had.

## Deleted accounts

Accounts removed from this server that still have backups here. cP:Restic knows
they existed because it recorded the cPanel removal event, and it only offers
names that actually have a successful backup.

Restoring one hands the archive to cPanel's own restore, which **creates the
account again** — files, databases, mail and settings as they were. Nothing is
overwritten, because the account is not there to overwrite.

Tick several and restore them in one go; the destination they come from is the
select in the bulk bar.

## Disaster recovery

For a server that is gone. Attach the destination the old server wrote to, plus
that server's **recovery key** — without the key nothing can read those backups,
which is the point of the encryption.

Once attached, its accounts and its system backup show up. Restore **the
server's own settings first**: an account restored onto a machine that has not
been set up lands somewhere that cannot serve it. Then restore the accounts
from the *Restore account(s)* tab.

## Where restores show up

On the [Logs](logs.md) page, under **Restores**: what was asked for, what
happened, and either the error or where the rebuilt archive is.

## One known fidelity note

A sparse file comes back fully allocated. `pkgacct`/tar do not preserve
sparseness, so a 20 MB sparse file restores as 20 MB of real blocks. The
contents are identical.
