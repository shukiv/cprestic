# Troubleshooting

## Reporting a problem

The bug icon at the foot of the rail — beside the documentation and GitHub
links — opens a form: a subject, and what happened in your own words.

Pressing **Show me what would be sent** gathers the rest and shows you the
whole thing before anything leaves the server: versions and environment, the
recent failures this server has recorded, its settings without any credential,
and the last 200 lines the service logged. Passwords, keys, tokens and private
keys are removed from those lines first, and each section is capped so a noisy
week does not become a report nobody reads.

Then, either:

- **Send to intake** — HTTPS to `https://bugs.jabali-panel.com/api/v1/intake`,
  explicitly routed to program `cprestic` (Plane project `CPRESTIC`). Success
  shows the issue number, a tracker link, and any nonfatal intake warnings.
  The link may need access to the internal Plane network. Email and notification
  channels are not involved; a failed delivery never falls back to email.
- **Download it** — `cprest-report-<when>.md`, the same text as a file, to send
  however you like. Available even without intake credentials.

Sending is a second, separate press on a page that showed you what is in it.
The reviewed diagnostics are signed and expire after twenty minutes or a
service restart. Sending does not gather fresh logs. If the subject or
description changes, preview again first. Downloads from a preview use the
same reviewed diagnostics.

### Enable intake delivery

The intake must register `cprestic` and issue a dedicated intake token through
its `INTAKE_TOKENS` configuration. Program readiness at `/healthz` or
`/api/v1/programs` is not authentication: submitting still needs that token.

Install **only the token value**, not `cprestic:` or `Bearer `, in
`/etc/cprest/bugs-intake.key` on the cPanel server. The file must be a regular
file owned by root with mode `0600`, inside the root-controlled config
directory. Do not put it in source code, the browser, a command argument, or
chat. For example, once the token is in a secure local file:

```bash
install -o root -g root -m 0600 /secure/path/cprestic-intake-token /etc/cprest/bugs-intake.key
```

With a custom `config_dir`, the file is `bugs-intake.key` in that directory;
Settings displays the exact path and local readiness. The key is reread for
each send, so installing, rotating, or removing it needs no restart. Removing
the key disables sending without disabling preview or download. Legacy
`bug_email` and `sendmail_path` settings remain readable for compatibility but
are no longer used for reporting.

Only explicitly submitted reports leave the server; routine backup failure
notifications keep their existing channel configuration. The report title and
description are redacted locally, as are the diagnostic sections, before
being sent as JSON. The intake redacts again. Redaction is best-effort: review
the preview for secrets in unusual formats or sensitive customer details.

An invalid token produces an actionable error. Rate limits show the returned
`Retry-After` delay. A timeout or ambiguous response means delivery is
**unconfirmed**, not definitely absent: check the intake before retrying. The
plugin does not automatically retry or assign fingerprints to manual reports,
so repeating a successful submission can create another issue. Neither HTTP
redirects nor a success response for a different program are accepted.

## First three commands

```bash
systemctl status cprest
journalctl -u cprest -n 100 --no-pager
ls -l /var/run/cprest/admin/ui.sock
```

## The plugin page is blank, or WHM 500s

The CGI proxies to the unix socket. If the service is down, there is nothing to
proxy to. Check `systemctl status cprest` first, then that the socket exists.

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

You need three things: `/etc/cprest/master.key` (or the destination credentials
typed again by hand), the **recovery key** for the destination, and the folder
name the old server used inside it. Then
[disaster recovery](restoring.md#disaster-recovery): system settings first,
accounts after.

If the master key is gone, the stored credentials are unreadable and must be
re-entered. If the recovery key is gone, the backups themselves are unreadable
by anyone, including you. Keep both somewhere that is not this server.
