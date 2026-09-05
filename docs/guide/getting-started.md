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

The checksums are signed and `get.sh` carries the public half of the release
key, so the check is not "these two files from the same page agree" but "this
release came from whoever holds the key". A download that fails either check
installs nothing.

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

## Keeping it current

Every page says so when a newer cP:Restic has been released: the version, what
changed, and the version this server runs.

**Settings → This copy of cP:Restic** installs it. The button names the
version; the page that follows says what happens and asks for a tick, because
this replaces the program on the server and restarts it. Then the card follows
it through — downloading, installing, and what the installer said — and keeps
following it across the restart, so the page an operator is watching is the
page that tells them it worked.

What it does is what a hand install does: download the release, check it, run
`install.sh`. What is different is what happens before the installer is handed
anything. The checksums published with a release are signed with the cP:Restic
release key, which is compiled into this program; a release whose signature
does not verify, or which arrives without one, stops there, with nothing
unpacked and nothing run. Then the tarball is checked against those signed
checksums. Only then is anything unpacked, and only ordinary files under
`cprest-plugin/` are written — a path leading out of that directory, or a
symlink, is refused rather than followed.

The installer runs outside this service, as a transient systemd unit, because
installing restarts the service that started it. Backups already queued stay
queued and run afterwards; a backup that is *running* is why the button
refuses until it has finished, since a restart would fail it.

The check asks GitHub once a day for a version number and installs nothing.
Settings → **Check for new versions** turns the daily ask off, and the banner
above every page with it; this card stays, saying what was last found, and
**Check now** asks again whenever you press it. A build that is not exactly a
release — a working tree, `v0.1.0-3-gabc1234-dirty` — is never offered an
upgrade.

The install command still works. Upgrading by hand is the same thing without
the button.

## Uninstalling

**Settings → Remove cP:Restic from this server** is at the bottom of the
settings page. It asks first, on a page that says the two things worth
knowing: this removes the interface you are standing in, and it does not
touch a single backup. What runs is the script below, started as a transient
systemd unit a few seconds later — otherwise it would stop the service
halfway through answering you.

In a root shell it is:

```bash
sh /usr/local/share/cprest/uninstall.sh
```

The installer leaves that copy on the server, so this never means finding the
package again. It stops the service and unregisters the WHM plugin, the cPanel
hooks and the account tile, and clears restic's cache.

It keeps `/etc/cprest/master.key` and `/var/lib/cprest/state.db`, so a
reinstall comes back with the same destinations, schedules and history.
Deleting the key deletes the only way to read the backups in those
destinations; that one is left for you to do on purpose, and only once you can
read them another way.

Backups already written to a destination are not touched either way.

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
