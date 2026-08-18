---
title: Enroll your first agent
description: Mint an enrollment token, enroll an endpoint over HTTPS, and install the agent as a system service.
---

The agent is a single static Go binary with no runtime dependencies. It
connects outbound only, over `wss://` on port 443, so it works from behind
NAT and corporate firewalls without any inbound rules.

## 1. Mint an enrollment token

On the server:

```bash
make enroll-token
```

This prints a one-time token. By default it is valid for 24 hours and one
use; the underlying command accepts `--max-uses` and `--ttl-hours` if you
are enrolling a batch.

## 2. Get the binary onto the endpoint

Until published releases exist, build it from the repo:

```bash
make agent          # → agent/bin/openrmm-agent for the local platform
```

Cross-compile with the usual Go environment variables (`GOOS`, `GOARCH`);
the binary is built with `CGO_ENABLED=0`, so there is nothing else to
ship.

## 3. Enroll

On the endpoint, as a user that can write the agent's state directory:

```bash
openrmm-agent enroll --server https://rmm.example.com --token <TOKEN>
```

Enrollment happens over HTTPS. The server assigns the device an
`agent_id`, issues its credentials, and tells it which NATS URL to dial
from then on (the `OPENRMM_NATS_PUBLIC_URL` you set during the
[quickstart](/getting-started/quickstart/)). On success:

```text
enrolled; identity saved to /var/lib/openrmm-agent/state.json
```

## 4. Install as a service

```bash
openrmm-agent install
```

This registers the agent with the platform's service manager: systemd on
Linux, launchd on macOS, the Service Control Manager on Windows. There is
a matching `uninstall`, and `openrmm-agent run` runs in the foreground if
you want to watch it work first.

## 5. Confirm it is alive

```bash
openrmm-agent status
```

prints the agent id, server, and NATS URL. Heartbeats go out every 30
seconds, so within one of those the device appears in the **Devices** view
with live telemetry following about a minute later. A device that misses
heartbeats for 90 seconds is marked offline and can trigger an alert; see
[alerting](/guides/alerts/).

## What just happened

The credentials issued at enrollment are only half the story. Every time
the agent connects, NATS defers the authorization decision to the server,
which pins that connection to subjects naming this one agent. No agent can
see another agent's traffic, jobs, or replies. The
[security model](/concepts/security-model/) walks through exactly how.
