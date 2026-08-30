# 6. A web UI on its own listener, and token enrolment

Date: 2026-08-30
Status: Accepted, deferred — superseded in ordering by [ADR 7](0007-standalone-mode.md)
Amends: [ADR 3](0003-agent-pull-with-pinned-certificates.md)

> The decisions below stand, but the controller web UI is now the second
> thing to be built rather than the first. Migration 0005 ships the `users`,
> `sessions` and `enrolment_tokens` tables ahead of the code that uses them.
> Nothing reads them yet.

## Context

Everything was operable only from a CLI on the controller host. Adding a
server meant issuing a certificate, copying three files to the cPanel box,
and running `add-server` with a fingerprint. That is a reasonable thing to
ask of the person who built the system and an unreasonable thing to ask of
everyone else.

ADR 3 ruled out self-service enrolment: "anything that hands out credentials
to whoever asks is a way into every backup." That reasoning still holds. A
graphical *Add server* button, however, has to hand out credentials to
something.

## Decision

**1. Two listeners, with different authentication and no overlap.**

| Listener | Authenticates with | Serves |
|---|---|---|
| agent API | mutual TLS, `RequireAndVerifyClientCert` | job dispatch and reporting |
| web | server TLS, session cookies or an enrolment token | the UI, and enrolment |

The agent listener is not weakened. It keeps ADR 3's property that an
anonymous request never reaches a handler. Enrolment cannot live there — an
agent being enrolled has no certificate yet, by definition — so it lives on
the web listener, which already accepts requests without one.

**2. Enrolment tokens are one-time, expiring, and hostname-bound.**

- An operator creates the server in the UI. That writes the `servers` row,
  in `pending`, exactly as `add-server` always did.
- The UI mints a token for that row: random, single-use, ~30 minutes,
  bound to the hostname. Only its SHA-256 is stored.
- The agent presents the token once and receives a client certificate. The
  redeem is a single conditional UPDATE, so two agents racing the same
  token cannot both win.
- The certificate's fingerprint is pinned to the pre-created row. No server
  row is ever created by an unauthenticated request.

**3. Passwords are argon2id**, salted per user, encoded with their
parameters so they can be raised later. Sessions are random tokens stored
by hash, so a database copy does not yield live sessions. Every mutating
form carries a per-session CSRF token.

## Rationale

ADR 3's concern was that credentials must not be issued to whoever asks.
That property survives: the operator still decides which hostnames exist,
in advance, one row at a time. What is automated is only the *handling* of
the certificate — the copying of three files that a human previously did by
hand, and did occasionally get wrong.

The window is bounded and small. A leaked token is good for one certificate,
for one named hostname, for half an hour, once. A leaked CA key — the thing
the old flow had operators carrying around — is good for everything,
forever.

## Consequences

- The controller now has a public-facing surface it did not have before.
  It holds every destination credential in the fleet, so its own TLS,
  session handling and CSRF protection are load-bearing rather than
  incidental.
- Redeem is rate-limited per IP and failures are logged with hostname and
  address, because it is the one endpoint that trades a bearer secret for a
  credential.
- `add-server` and `issue-cert` remain. An operator who prefers to keep
  certificates entirely in their own hands loses nothing.
- The web listener needs a certificate a browser will accept, which for
  most deployments means a real one rather than the internal CA that signs
  agent certificates.
