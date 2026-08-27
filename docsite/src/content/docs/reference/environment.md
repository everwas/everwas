---
title: Environment variables
description: Every variable in .env.example, what it does, and which ones are load-bearing.
---

All configuration flows through `.env` at the repo root, consumed by
Docker Compose. Copy `.env.example` and adjust; never commit `.env`.

## Compose

| Variable | Default | Notes |
|---|---|---|
| `COMPOSE_PROJECT_NAME` | `everwas` | Keeps this stack's resources from clashing with others on the host |
| `EVERWAS_MODE` | `dev` | `dev` = hot reload, bind mounts, Mailpit. `prod` = built images. Selects the compose overlay file |

## Public hostnames

All four hostnames are published through caddy-docker-proxy labels on the
external `caddy` network, which handles TLS.

| Variable | Default | Notes |
|---|---|---|
| `EVERWAS_DOMAIN` | (required) | The app and the API share this one origin. The session cookie is HttpOnly and SameSite=Lax; a separate API hostname would mean CORS plus SameSite=None plus a cross-site cookie, to solve nothing |
| `EVERWAS_NATS_DOMAIN` | `nats-$EVERWAS_DOMAIN` | Agents connect here. It must be its own hostname serving at the root: the Go NATS client ignores the path in a websocket URL, so `wss://host/nats` lands on whatever handles `/` and never completes the handshake |
| `EVERWAS_MCP_DOMAIN` | `mcp-$EVERWAS_DOMAIN` | MCP clients present a bearer key, not the browser session, so the MCP server gets its own origin |
| `EVERWAS_MAIL_DOMAIN` | `mail-$EVERWAS_DOMAIN` | Dev only; the Mailpit UI |

## PostgreSQL

| Variable | Default | Notes |
|---|---|---|
| `POSTGRES_DB` | `everwas` | |
| `POSTGRES_USER` | `everwas` | |
| `POSTGRES_PASSWORD` | (required) | |

## Server

| Variable | Default | Notes |
|---|---|---|
| `EVERWAS_SECRET_KEY` | (required) | Session cookie signing and CSRF. Generate with `openssl rand -hex 32` |
| `EVERWAS_MCP_ENABLED` | `false` | The MCP server is opt-in; see [enabling it](/guides/enable-mcp/) |
| `EVERWAS_POSTURE_EGRESS_SUBJECT` | (empty; egress off) | NATS subject each device's posture collection is pushed to for an access verifier (l2trace consumes `l2trace.posture`). Empty publishes nothing, which is safe: a verifier reads absence as not-assessed, and not-assessed never gates. See the [wire protocol](/reference/wire-protocol/#server-to-verifier-posture-egress) |

## NATS auth

| Variable | Default | Notes |
|---|---|---|
| `EVERWAS_NATS_AUTH_SEED` | (required) | Auth-callout signing seed. Generate the pair with `cd server && uv run everwas gen-nats-keys` |
| `EVERWAS_NATS_AUTH_ISSUER` | (required) | The matching public key, configured into the NATS server |
| `EVERWAS_NATS_SERVER_PASSWORD` | (required) | Password for the internal `server` NATS user used by the api and dispatcher. `openssl rand -hex 24` |
| `EVERWAS_NATS_PUBLIC_URL` | `wss://$EVERWAS_NATS_DOMAIN` | What enrolled agents are told to dial. **Baked into each agent's config at enrollment**, so changing it later does not move agents already in the field |

## SMTP

Prod alert email. Dev mode ignores these and delivers to Mailpit.

| Variable | Default |
|---|---|
| `EVERWAS_SMTP_HOST` | (empty; email delivery off) |
| `EVERWAS_SMTP_PORT` | `587` |
| `EVERWAS_SMTP_USER` / `EVERWAS_SMTP_PASSWORD` | (empty) |
| `EVERWAS_SMTP_FROM` | `everwas@example.com` |
