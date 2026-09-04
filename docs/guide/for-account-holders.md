# For account holders

Your customers get a tile in cPanel: their own restore points, and nothing
else.

## What they can do

Restore point → category → item. They can prepare:

- a full cPanel account archive
- the home directory
- cron jobs
- databases and database users
- domains and DNS
- SSL material
- mail
- FTP settings

Every self-service request produces a **private download**. A customer cannot
apply anything to their live account and cannot overwrite anything. The
operator-only *Apply* flag is not reachable from that socket at all.

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
