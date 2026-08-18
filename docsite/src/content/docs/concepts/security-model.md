---
title: Security model
description: Per-agent isolation enforced by the broker, instant revocation, confirmation-gated automation, and an audit trail that covers assistants too.
---

An RMM is, structurally, remote code execution as a product. The design
question is not whether that is dangerous (it is) but where the
enforcement lives and how small each credential's blast radius can be.

## Agents are isolated by the broker, not by convention

Each agent authenticates to NATS as itself, and NATS delegates the
authorization decision to the server on every connection: the
auth-callout responder in the dispatcher verifies the agent's secret
against PostgreSQL and returns permissions pinned to that one agent's
subjects. **Every grant names the agent; there are no shared subjects.**

The practical consequence: a fully compromised endpoint holds a
credential that can impersonate only itself. It cannot see another
agent's job stream (which contains script bodies about to run as root),
cannot drain the fleet's work queue, and cannot forge acknowledgements
on the server's ingest. The three specific grants that carry this
isolation, and the ways each would break if widened, are documented in
the [wire protocol reference](/reference/wire-protocol/).

Subject identifiers are validated before they are ever interpolated into
a subject string, because a malformed identifier is as dangerous as a
bad grant. Conformance tests on both the Go and Python sides assert that
a foreign-subject publish is refused.

## Enrollment and revocation

Enrollment happens over HTTPS with a one-time token (24 hours, single
use by default). The server assigns the identity and issues the secret;
the token itself never becomes a credential.

Because authorization is checked against the database at connect time,
**revoking an agent is one database flip, effective immediately**. There
is no certificate revocation list to distribute and no window where a
revoked credential still works. This property drove the decision to keep
a shared-secret credential for the management plane even as a device CA
arrives for network authentication; the reasoning is in
[ADR-0003](/decisions/0003-device-ca/).

Credential rotation is agent-initiated: the agent renews using the
credential it still holds, so nothing is ever pushed to a machine that
might be offline at the moment of rotation.

## Humans, sessions, and one origin

The web app and API share a single origin, so the session cookie can be
HttpOnly and SameSite=Lax with no CORS surface at all. A separate API
hostname would buy nothing and cost SameSite=None plus a cross-site
cookie.

Actions carry the requesting human through the system: a shell open or a
script run names `requested_by` on the wire, and the audit events written
at the far end name a person, not just a server.

## Assistants get the same treatment, smaller

The [MCP server](/guides/enable-mcp/) is off by default and its keys are
scoped: a key minted with `devices:read` can never run a script,
regardless of what the assistant is asked. Mutations are two-step
(dry-run plan first, then `confirm: true`), and every call, successful
or refused, lands in the same audit log as human actions, under the
key's name.

## Agent updates are verified

Self-update offers are signed (sha256 plus a minisign signature); the
agent verifies before swapping binaries and rolls back automatically if
the new version fails to start. An update channel that can push unsigned
code to a fleet would otherwise be the single best target in the system.
