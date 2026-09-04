# Logs

Everything cP:Restic has done on this server, split by the kind of work.

| Tab | What is in it |
|---|---|
| **Backups** | account backup runs: what was stored, what was new, how long it took |
| **System backups** | the server's own settings — EasyApache, packages, tweak settings |
| **Restores** | restores and rehearsals, with the archive path or the error |
| **cPanel events** | what cPanel told cP:Restic as accounts were created, renamed, suspended or removed |

## While something is running

Every page carries a strip at the top naming what is happening now — the
account, whether it is being backed up or restored, and restic's own
percentage where there is one. It keeps itself up to date, and it is on every
page because somebody who asks for a restore and sees nothing asks again. A
customer's own page shows the same strip for their account and nothing else.

Notifications can say it too: **A backup or restore started** is one of the
events a channel can subscribe to under Settings. It is off unless asked for —
on a server with a nightly schedule it is one message per account per night —
and it exists for the operator who wants to know the moment a customer's
restore begins.

## Reading a backup row

The size column reads like `6.2 MiB new of 152.5 MiB`: what actually left the
server, out of what the account holds. With split mode an unchanged account is
almost entirely the first number being small.

A **partial** run is one where some accounts succeeded and others did not. The
detail says which.

## cPanel events

A removal marked **Blocked** is cP:Restic refusing to let cPanel delete an
account it has no complete copy of — [termination safety](accounts.md#termination-safety)
doing its job. **Allowed** with a note means the copies were there.

If the service was down when cPanel called, the event says so and the removal
was allowed: cPanel administration is never wedged by a stopped backup service.

## Sorting and paging

Every column with an order worth having is sortable — click the header, click
again to reverse. Newest first by default. Rows per page: 20, 50 or 100, and
the choice is remembered in this browser.

Raw restic output is kept for the number of days set in
[Settings](settings.md).
