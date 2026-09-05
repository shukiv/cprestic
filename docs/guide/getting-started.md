# Getting started

One cPanel server, backing itself up, managed from WHM. No controller, no
second machine, no database to run.

## Before you install

- **root on the cPanel server.** The plugin refuses every WHM user that is not root.
- **restic.** The installer fetches it if this server has none, checked
  against restic's own published checksum.
- **Somewhere to put backups.** Another Linux box over SSH, an S3 bucket, a restic
  REST server, or a mounted disk. See [Destinations](destinations.md).
- **Room to stage.** A backup rebuilds one account in full on local disk before it
  is uploaded, so the staging volume needs room for your largest account.

## Install

On the cPanel server, as root:

```bash
curl -fsSL https://github.com/shukiv/cprestic/releases/latest/download/get.sh | sh
```

One command. It fetches the newest release, checks it against the checksums
published beside it, and runs the installer inside it. To read the script
before a root shell does, download it with its checksums first:

```bash
curl -fsSLO https://github.com/shukiv/cprestic/releases/latest/download/get.sh
curl -fsSLO https://github.com/shukiv/cprestic/releases/latest/download/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
less get.sh
sh get.sh
```

`CPREST_VERSION=v1.2.3` before `sh` installs that release rather than the
newest.

The installer installs restic if this server has none, installs the service,
registers the plugin with WHM through AppConfig, confirms WHM kept the
registration, and registers cPanel hooks for account create, modify, suspend,
unsuspend and remove. Running it again upgrades in place.

Then open WHM and look for **cP:Restic Backups** in the sidebar's **Plugins**
group. Not under **Manage Plugins** — that page lists cPanel's own RPM addons;
an AppConfig plugin appears in the sidebar, and its registration under
**Development → Apps Managed by AppConfig**.

### From source instead

For a change you have made, or a machine you would rather not download to.
On a machine with Go:

```bash
make plugin           # builds bin/cprest-plugin-amd64.tar.gz
scp bin/cprest-plugin-amd64.tar.gz root@your-server:/root/
```

Then on the cPanel server, as root:

```bash
tar xzf cprest-plugin-amd64.tar.gz
sh cprest-plugin/install.sh
```

Through `sh` because cPanel mounts `/tmp` and `/var/tmp` noexec: an installer
unpacked there will not run otherwise.

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
