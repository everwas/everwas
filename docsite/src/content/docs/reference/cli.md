---
title: CLI and make targets
description: The server management commands, the repo's make targets, and the agent's subcommands.
---

## Server commands

These run inside the `everwas-api` container; the make targets below wrap
the common ones. Direct form: `docker compose exec everwas-api everwas <command>`.

| Command | What it does |
|---|---|
| `everwas version` | Print the server version |
| `everwas create-admin EMAIL` | Create or update an admin user; prompts for a password unless `--password` is given |
| `everwas gen-enrollment-token` | Mint an agent enrollment token; `--max-uses` (default 1), `--ttl-hours` (default 24) |
| `everwas create-api-key NAME --scopes ...` | Mint a scoped API key; the secret is printed once |
| `everwas gen-nats-keys` | Generate the NATS auth-callout keypair as two `.env`-ready lines |

## Make targets

The repo root `Makefile` wraps `docker compose` with the mode overlay
selected by `EVERWAS_MODE` in `.env` (`dev` or `prod`).

| Target | What it does |
|---|---|
| `make up` | Build and start the stack in the current mode |
| `make dev` | Same, but refuses to run unless the mode is `dev` |
| `make down` | Stop the stack |
| `make logs SVC=name` | Tail logs, optionally for one service |
| `make ps` | Service status |
| `make restart SVC=name` | Restart services |
| `make migrate` | Apply Alembic migrations |
| `make revision m="msg"` | Autogenerate a migration |
| `make psql` | PostgreSQL shell |
| `make nats-cli` | A nats-box container inside the stack network for debugging |
| `make admin EMAIL=...` | Create an admin user |
| `make enroll-token` | Mint an agent enrollment token |
| `make api-key NAME=... SCOPES=...` | Mint a scoped API key |
| `make test` | Server tests (pytest) plus agent tests (go test) |
| `make lint` / `make fmt` | ruff and go vet / gofmt |
| `make openapi` | Dump the OpenAPI schema to `web/openapi.json` |
| `make agent` | Build the agent binary for the local platform |
| `make release` | Tag a CalVer release (`YYYY.MM.DD`) |

## Agent subcommands

The agent binary is self-contained; these run on the endpoint.

| Command | What it does |
|---|---|
| `everwas-agent enroll --server URL --token TOKEN` | Enroll with a server over HTTPS and save the identity locally |
| `everwas-agent run` | Run in the foreground |
| `everwas-agent install` | Install as a system service (systemd / launchd / SCM) |
| `everwas-agent uninstall` | Remove the system service |
| `everwas-agent status` | Show enrollment state, agent id, server, and NATS URL |
| `everwas-agent supplicant-profile` | Write an 802.1X client profile for this device: `--ssid NAME` for wireless, omitted for wired, `--out DIR` to place it. Writes a file and nothing else, and refuses if the device is not enrolled or holds no certificate. See [802.1X](/guides/network-authentication/) |
| `everwas-agent version` | Print the agent version |
