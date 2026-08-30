# 3. Agents pull over mTLS, identified by pinned fingerprint

Date: 2026-08-30
Status: Accepted

## Context

The controller must reach every cPanel server to schedule work, and every
agent must receive credentials for the destinations it writes to. Two
questions follow: who dials whom, and how the controller knows which server
it is talking to.

cPanel servers routinely sit behind NAT, provider firewalls, or CSF rules
that make inbound reachability a per-customer support ticket.

## Decision

1. The agent always initiates. It long-polls `GET /v1/jobs/next` and posts
   results; the controller never dials an agent.
2. The transport is mutual TLS, minimum TLS 1.3, with no plaintext or
   password-authenticated fallback.
3. Authorisation is by the SHA-256 fingerprint of the client certificate,
   registered against a server record by an administrator. A certificate
   signed by the CA is *not* sufficient on its own.
4. There is no self-service enrolment endpoint. Certificates are issued by
   `cprest-controller issue-cert` and registered by `add-server`.

## Rationale

Outbound HTTPS works everywhere and adds no inbound attack surface to the
machine we trust least. Long polling keeps dispatch latency near a second
without a fleet of agents polling every second.

Pinning the fingerprint rather than trusting the certificate subject means a
CA compromise or a mis-issued certificate does not silently become a valid
agent: the fingerprint must also have been registered. The cost is that
certificate rotation is an explicit operation, which is the right trade for
credentials that unlock every destination.

## Consequences

- Job dispatch latency is bounded by the long-poll interval, not by a
  controller-side push.
- A crashed agent is handled by lease expiry, not by connection state: the
  scheduler returns the job to the queue when its lease lapses. Restic
  tolerates the partially written repository, and a later prune reclaims the
  orphaned pack data.
- Adding a server is a two-step operator action (issue, then register).
  Automating it would mean building a self-service enrolment path, which is
  exactly the thing we decided not to have.
