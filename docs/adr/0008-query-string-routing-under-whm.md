# 8. The WHM plugin routes by query string

Date: 2026-08-30
Status: Accepted

## Context

The interface is served over a unix socket and reached through a WHM plugin
CGI ([ADR 7](0007-standalone-mode.md)). That left one question nobody had
answered: how does a request for a second page reach the CGI at all?

The first attempt assumed the ordinary CGI arrangement — the script at
`/cgi/cprest.cgi`, further path in `PATH_INFO`, so `/cgi/cprest.cgi/restore`
reaches the plugin and proxies to `/restore`.

Tested against cPanel 136 on AlmaLinux 8, through a real WHM session:

| Request | Result |
|---|---|
| `/cgi/probe.cgi` | 200 |
| `/cgi/probe.cgi/` | **404** |
| `/cgi/probe.cgi/destinations` | **404** |
| `/cgi/probe.cgi?p=destinations` | routed (403 only because the probe was unregistered) |

cpsrvd does not route anything after the script name. It is not that
`PATH_INFO` arrives empty — the URL does not resolve at all. Every plugin
already installed on that server agrees: JetBackup registers four separate
`.cgi` files rather than one with sub-paths.

## Decision

Every route travels in a `p` query parameter. The CGI turns `?p=restore`
back into the path `/restore` before proxying, and passes the remaining
query parameters through untouched. Internal links are bare query strings —
`href="?p=destinations"` — so the browser resolves them against the current
script and WHM's per-session `/cpsessNNN` token in the path survives without
the plugin having to know about it.

`p` is validated against a conservative character set before it becomes a
path, since it arrives from the browser and steers a proxy.

The interface itself is unchanged: it still routes on ordinary paths, is
still testable over a plain socket, and knows nothing about WHM. The
translation lives entirely in the CGI.

## Consequences

- URLs are `cprest.cgi?p=destinations` rather than `cprest.cgi/destinations`.
  Uglier, and the only shape that works here.
- Anything that emits a link or a redirect must go through the query form.
  The redirect helper builds it centrally, and the templates were converted
  in one pass, but a hand-written `href` to a bare path would 404 with no
  other symptom.
- A future controller web UI, served by our own HTTP server rather than
  cpsrvd, has no such constraint. If the two ever share templates, the link
  form has to become a template function rather than a literal.

## Two more things cpsrvd does

**It strips `Content-Type` from what the plugin returns**, and sets
`X-Content-Type-Options: nosniff` itself. A stylesheet fetched as its own
request therefore arrives with no type and the browser declines to apply it,
producing a working but entirely unstyled interface. Measured at the socket
the response is a correct `text/css; charset=utf-8`; it does not survive the
proxy. The stylesheet and script are now inlined into every page, which
sidesteps this and saves two round trips.

**The service gets no `HOME`.** systemd sets none, cPanel keeps root's MySQL
credentials in `/root/.my.cnf`, and so every `SHOW DATABASES` failed with
"Access denied". The unit now sets `Environment=HOME=/root`.

## What this cost, and the lesson

Four releases went onto a live server before the interface worked: no menu
item, then a 404, then an unstyled page, then a page insisting the server
had no cPanel accounts. Every one was an assumption about a system that was
one `curl` or one `systemctl show` away from answering: whether AppConfig
needs `entryurl`, whether cpsrvd routes `PATH_INFO`, whether it preserves
`Content-Type`, whether a systemd unit has `HOME`.

The installer now verifies its own AppConfig registration and the routing
has a test that runs the CGI the way WHM does. But the general lesson is the
cheaper one: when the target system is reachable, ask it before shipping
to it.
