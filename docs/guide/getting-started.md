# Getting started

One cPanel server, backing itself up, managed from WHM. No controller, no
second machine, no database to run.

## Before you install

- **root on the cPanel server.** The plugin refuses every WHM user that is not root.
- **restic.** One command, below. The installer checks for it and prints that
  command if it is missing.
- **Somewhere to put backups.** Another Linux box over SSH, an S3 bucket, a restic
  REST server, or a mounted disk. See [Destinations](destinations.md).
- **Room to stage.** A backup rebuilds one account in full on local disk before it
  is uploaded, so the staging volume needs room for your largest account.

## Install

Five steps, on two machines: any machine with Go to build on, and the cPanel
server itself.

**1. Install restic on the cPanel server**, as root. cP:Restic drives restic;
it does not carry a copy of it.

```bash
curl -L https://github.com/restic/restic/releases/download/v0.19.1/restic_0.19.1_linux_amd64.bz2 \
  | bunzip2 > /usr/local/bin/restic
chmod 755 /usr/local/bin/restic
```

**2. Build the plugin** on a machine with Go. Nothing in this step needs
cPanel, and the tarball is statically linked, so it runs on any cPanel server.

```bash
make plugin           # builds bin/cprest-plugin-amd64.tar.gz
```

**3. Copy the tarball to the cPanel server.**

```bash
scp bin/cprest-plugin-amd64.tar.gz root@your-server:/root/
```

**4. Unpack it and run the installer there, as root.**

```bash
tar xzf cprest-plugin-amd64.tar.gz
cprest-plugin/install.sh
```

**5. Open WHM** and look for **cP:Restic Backups** in the sidebar's **Plugins**
group. Not under **Manage Plugins** — that page lists cPanel's own RPM addons;
an AppConfig plugin appears in the sidebar, and its registration under
**Development → Apps Managed by AppConfig**.

The installer refuses anything that is not a cPanel server, checks for restic
and prints step 1 again if it is missing, installs the service, registers the
plugin with WHM through AppConfig, confirms WHM kept the registration, and
registers cPanel hooks for account create, modify, suspend, unsuspend and
remove. Running it again upgrades an existing install: steps 2 to 4 are the
whole of an upgrade.

## The first ten minutes

1. **Destinations → Add a destination.** Fill in where the backups go. cP:Restic
   tests the connection and initialises the repository before saving anything.
2. **Write down the recovery key.** It is shown once, on the recovery card. Without
   it those backups cannot be read — not by this program, not by the machine
   holding them, not by you.
3. **Schedules → Add a schedule.** Nightly, every account, split mode, whatever
   retention you want. See [Schedules](schedules.md).
4. **Accounts → Back up now** on one account, to watch a real run finish rather
   than waiting until 02:00 to find out something was wrong.
5. **Overview** should then read *n of n accounts have a usable backup*.

## Where things live

| Path | What |
|---|---|
| `/usr/local/bin/cprest-agent` | the service |
| `/usr/local/cpanel/whostmgr/docroot/cgi/cprest.cgi` | the WHM plugin |
| `/etc/cprest/master.key` | the key that encrypts stored destination credentials |
| `/var/lib/cprest/state.db` | jobs, schedules, destinations, account identities |
| `/var/lib/cprest/staging` | where an account is rebuilt before upload |
| `/var/run/cprest/admin/ui.sock` | the interface, root only |
| `/var/run/cprest/account/user.sock` | the account-facing socket |

The interface listens on a unix socket, not a port. cPanel servers are
multi-tenant and this interface can read every stored credential, so it is not
reachable over the network at all: the WHM plugin proxies to it.

```bash
systemctl status cprest
journalctl -u cprest -f
```
