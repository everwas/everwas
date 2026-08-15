# ADR-0001: NATS over websocket as the agent transport

Status: accepted (2026-08-15)

## Context

Agents live behind NAT and corporate firewalls and must connect outbound only.
Candidates: NATS (tcp 4222 or websocket), MQTT, plain WebSockets to the API.

## Decision

NATS + JetStream, with the **websocket listener proxied by Caddy on 443**
(`wss://`) as the default agent path. Plain `tls://:4222` remains a config
option for LAN deployments.

## Rationale

- Request/reply and per-subject authorization map directly onto "send command
  to agent X and await response" — no hand-rolled correlation layer.
- JetStream gives durable job delivery to offline agents (JOBS stream) and
  ingest buffering without adding Redis/Celery.
- Port 443 traverses corporate egress policies; outbound 4222 frequently does
  not. nats.go also has no HTTP CONNECT proxy support, which wss avoids.
- TLS terminates at Caddy alongside every other HTTP service — one cert story.

## Consequences

- The NATS auth-callout responder (dispatcher) is on the agent connect path;
  its failure mode must be understood before M1 completes.
- Caddy must proxy websocket upgrades to the NATS listener (prod overlay).
