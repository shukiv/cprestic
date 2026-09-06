# Troubleshooting

## Reporting a problem

The bug icon at the foot of the rail — beside the documentation and GitHub
links — opens a form: a subject, and what happened in your own words.

Pressing **Show me the report** gathers the rest and shows you the whole
thing: versions and environment, the recent failures this server has
recorded, its settings without any credential, and the last 200 lines the
service logged. Passwords, keys, tokens and private keys are removed from
those lines first, and each section is capped so a noisy week does not become
a report nobody reads.

Nothing is transmitted from the server. **Download it** hands the report over
as `gniza-report-<when>.md`, and you file it yourself at
<https://bugs.jabali-panel.com/report>, picking **cprestic** as the product —
the tracker's own key for this project, which did not follow the rename to
Gniza. Attach the file as the log file, or paste it into the description.

There is nothing to configure and no credential to install. A plugin that is
published to every cPanel server cannot hold a key to an authenticated
endpoint without publishing the key too, so it does not have one: reporting
works the same on every server, out of the box.

The reviewed report is signed and expires after twenty minutes or a service
restart, so a download hands over exactly the diagnostics you read rather
than freshly gathered logs that may say something else. If the subject or
description changes, show the report again first.

Redaction is best-effort, and you are the last check before the report
becomes public: read it for secrets in unusual formats or sensitive customer
details before you file it. Legacy `bug_email` and `sendmail_path` settings
remain readable for compatibility but are not used.

Customers with no access to WHM can use the same form directly; it needs
nothing installed.

## First three commands

```bash
systemctl status gniza
journalctl -u gniza -n 100 --no-pager
ls -l /var/run/gniza/admin/ui.sock
```

## The plugin page is blank, or WHM 500s

The CGI proxies to the unix socket. If the service is down, there is nothing to
proxy to. Check `systemctl status gniza` first, then that the socket exists.

If the plugin is missing from the WHM sidebar entirely, look under
**Development → Apps Managed by AppConfig** — not **Manage Plugins**, which
lists cPanel's own RPM addons.

## A backup fails at staging

Almost always room. Staging rebuilds one account in full before upload, so the
staging volume needs room for your largest account, not your average one.
Settings → Staging shows what is left; the Overview shows the same number.

Second most common: an account whose home directory is being written to
heavily. The run reports partial and names the account.

## A destination stops answering

The destination row says when it was last reachable. Test it from the row menu
— that runs the real connection, not a cached verdict.

For SFTP, the usual causes are a rotated host key or a key that was removed on
the far side. For S3, an expired access key. For a local path, a mount that is
no longer mounted — a destination pointing at an unmounted mountpoint will
happily write to the underlying directory instead, which is why free space is
shown per destination.

## Restore says the account has no backups here

Check the destination select. An account is often in one destination and not
another, and *Restore account(s)* lists what the chosen destination actually
holds — read from the backups, not from cPanel.

If the account was deleted and recycled, the current holder of the name does
not inherit the previous one's backups. That is deliberate.

## cPanel will not let me delete an account

[Termination safety](accounts.md#termination-safety) is on and the account is
missing a promised copy. Either **Prepare for termination**, which queues the
smallest set of backups that fixes it, or turn the setting off.

## Everything is fine but the pages are slow

The Restore page reads the destination when it loads — that is a real `restic
snapshots` against remote storage. A cold read against SFTP takes seconds; a
warm one about a second. Other pages read local state and are instant.

## Restic output for a specific run

Logs → the run → its detail. Raw output is kept for the number of days set in
Settings.

## Rebuilding this server from nothing

You need three things: `/etc/gniza/master.key` (or the destination credentials
typed again by hand), the **recovery key** for the destination, and the folder
name the old server used inside it. Then
[disaster recovery](restoring.md#disaster-recovery): system settings first,
accounts after.

If the master key is gone, the stored credentials are unreadable and must be
re-entered. If the recovery key is gone, the backups themselves are unreadable
by anyone, including you. Keep both somewhere that is not this server.
