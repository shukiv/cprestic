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

Each of them can be downloaded as a **private package**. Five of them can also
be **put back into the live account**: the home directory, the website, mail,
a database and the database users. Those are the ones where the backup holds
everything needed to make the account whole again, and the customer is told
plainly, and has to tick a box, before the live copy is replaced.

The rest stay download-only. Not because putting them back is impossible, but
because each needs a change the control panel makes rather than a file copied
over another — a DNS zone, an installed certificate, an FTP login, the
account's own settings — and none of that is built yet. So does the full
account archive, whatever else is selected: that one goes to cPanel's
`restorepkg`, which runs as root over a home directory the customer controls,
and it remains an operator's decision.

Putting the home directory back is done as the account, never as root: the
service reads the staged copy, which the customer cannot, and the customer's
own process writes into their home directory. What lands there is owned by
them, and a link planted beforehand leads nowhere they could not already
write. A database is loaded only after the server, not the backup, confirms
the account owns it.

## Database users

A database restored without the user that reads it is a site that still cannot
start, so the users come back with the passwords they had rather than as names.
Three things have to happen and no single interface does all of them:

1. **MySQL** is given the login and the stored password hash. cPanel's own API
   needs the password in the clear, and nobody has it — a backup holds the hash.
2. **`dbmaptool`** records that the account owns the user. Without it MySQL has
   a user the panel has never heard of: it does not appear under MySQL
   Databases, and the customer cannot change or delete it.
3. **cPanel, as the account**, gives back the privileges, so the panel's own
   record of who may read what is written too — and so cPanel refuses a
   database that is not the account's, whatever this program believed.

The account's own MySQL user is the exception: it owns that record rather than
appearing in it, so cPanel refuses both of the last two steps for it, and its
grants are written directly instead.

Two checks are made before anything runs, and either one fails the whole
request rather than skipping an entry: no other account may currently hold the
user name, and every database granted must be one the account has now. A
restore that quietly left out the grant an application connects with is a
restore that reports success and leaves the site down.

Restoring the users before the database they read is the order a customer will
try, and it is answered with "restore or create those databases first" rather
than with a failure they cannot act on.

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
