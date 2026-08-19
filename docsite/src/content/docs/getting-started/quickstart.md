---
title: Get the server running
description: Bring up the Everwas server stack with Docker Compose, apply migrations, and create the first admin login.
---

This tutorial takes you from a checkout of the repository to a running
server with an admin login. It takes about ten minutes, most of it waiting
on image builds.

## What you need

- Docker with the Compose plugin (`docker compose`, not `docker-compose`)
- [uv](https://docs.astral.sh/uv/) on the host, used once to generate NATS
  auth keys
- A hostname pointed at the machine. The stack publishes its services
  through [caddy-docker-proxy](https://github.com/lucaslorentz/caddy-docker-proxy)
  labels on an external Docker network named `caddy`, so a
  caddy-docker-proxy container should already be running on that network.
  It handles TLS for every hostname automatically.

The server stack is four services: PostgreSQL, NATS with JetStream, the
FastAPI application (`everwas-api`), and the dispatcher that handles
ingest, alerting, and scheduling.

## 1. Configure the environment

Copy the template and open it:

```bash
cp .env.example .env
```

Set the hostname your fleet and your browser will use:

```bash
EVERWAS_DOMAIN=rmm.example.com
```

The web app and the API share this one origin on purpose, and agents dial
`wss://nats-rmm.example.com` (its own hostname, derived from yours by
default). The comments in `.env.example` explain both choices.

Generate the secrets:

```bash
# session cookie signing
openssl rand -hex 32   # → EVERWAS_SECRET_KEY

# internal NATS password for the api/dispatcher
openssl rand -hex 24   # → EVERWAS_NATS_SERVER_PASSWORD

# and a strong POSTGRES_PASSWORD of your choosing
```

Then generate the NATS auth-callout keypair. This is what lets the server
vouch for agents when they connect:

```bash
cd server && uv run everwas gen-nats-keys
```

It prints two lines, `EVERWAS_NATS_AUTH_SEED=...` and
`EVERWAS_NATS_AUTH_ISSUER=...`, ready to paste into `.env`.

Finally, make sure `EVERWAS_NATS_PUBLIC_URL` matches your domain:

```bash
EVERWAS_NATS_PUBLIC_URL=wss://nats-rmm.example.com
```

This URL is baked into each agent's config at enrollment, so it is worth
getting right before the first agent enrolls. Changing it later does not
move agents that are already in the field.

## 2. Start the stack

```bash
make up
```

This builds and starts everything in the mode set by `EVERWAS_MODE` in
`.env`. The default is `dev`, which gives you hot reload and a
[Mailpit](https://mailpit.axllent.org/) instance at `mail-rmm.example.com`
so alert emails land somewhere you can see them. Switch to `prod` when you
mean it.

Apply the database migrations:

```bash
make migrate
```

## 3. Create your login

```bash
make admin EMAIL=you@example.com
```

The command prompts for a password and creates (or updates) the admin
user. Now open `https://rmm.example.com` and sign in.

You have a server and an empty fleet. The next step is
[enrolling your first agent](/getting-started/enroll-an-agent/).

## If something is off

`make ps` shows service health, and `make logs SVC=everwas-api` tails one
service. The compose file gives PostgreSQL and NATS healthchecks, so a
service stuck in `starting` usually means a missing `.env` value; the
compose file fails fast with a message naming the variable.
