# For account holders

Your customers get a tile in cPanel: their own restore points, and nothing
else.

## What they can do

Restore point → category → item. The categories are:

- a full cPanel account archive
- the home directory
- cron jobs
- databases and database users
- domains and DNS
- SSL material
- mail
- FTP settings

Each of them can be downloaded as a **private package**. Four of them can also
be **put back into the live account**: the home directory, the website, mail
and a database. Those are the ones where restoring the backup is the whole
operation, and the customer is told plainly, and has to tick a box, before the
live copy is replaced.

The rest stay download-only, because reinstating them is a change the control
panel has to make rather than a file to copy: a DNS zone, an installed
certificate, an FTP login. So does the full account archive, whatever else is
selected — that one goes to cPanel's `restorepkg`, which runs as root over a
home directory the customer controls, and it remains an operator's decision.

Putting the home directory back is done as the account, never as root: the
service reads the staged copy, which the customer cannot, and the customer's
own process writes into their home directory. What lands there is owned by
them, and a link planted beforehand leads nowhere they could not already
write. A database is loaded only after the server, not the backup, confirms
the account owns it.

## Who is let in

The account owner, and cPanel Administrator Team users with the right
capability. The root service verifies the live cpsrvd session itself and hands
the proxy a 20-second, one-use token for that exact request — sharing the
account's Unix uid is not enough to use the public socket.

A WHM root administrator arriving through cPanel's **Log in to cPanel** flow is
accepted only when the root-owned session record carries cPanel's temporary-user
and cPHulk markers.

See [ADR 12](../adr/0012-the-account-facing-socket.md) and
[ADR 13](../adr/0013-cpanel-session-capabilities.md).
