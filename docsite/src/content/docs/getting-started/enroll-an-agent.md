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
make agent          # → agent/bin/everwas-agent for the local platform
```

Cross-compile with the usual Go environment variables (`GOOS`, `GOARCH`);
the binary is built with `CGO_ENABLED=0`, so there is nothing else to
ship.

## 3. Enroll

On the endpoint, as a user that can write the agent's state directory:

```bash
everwas-agent enroll --server https://rmm.example.com --token <TOKEN>
```

Enrollment happens over HTTPS. The server assigns the device an
`agent_id`, issues its credentials, and tells it which NATS URL to dial
from then on (the `EVERWAS_NATS_PUBLIC_URL` you set during the
[quickstart](/getting-started/quickstart/)). On success:

```text
enrolled; identity saved to /etc/everwas/agent.json
```

The state directory is `/etc/everwas` for a root agent on Linux,
`~/.config/everwas` for one running as a user, `C:\ProgramData\Everwas\Agent`
on Windows and `/Library/Application Support/Everwas` on macOS.
`EVERWAS_STATE_DIR` overrides all of them.

## 4. Install as a service

```bash
everwas-agent install
```

This registers the agent with the platform's service manager: systemd on
Linux, launchd on macOS, the Service Control Manager on Windows. There is
a matching `uninstall`, and `everwas-agent run` runs in the foreground if
you want to watch it work first.

## 5. Confirm it is alive

```bash
everwas-agent status
```

prints the agent id, server, and NATS URL. Heartbeats go out every 30
seconds, so within one of those the device appears in the **Devices** view
with live telemetry following about a minute later. A device that misses
heartbeats for 90 seconds is marked offline and can trigger an alert; see
[alerting](/guides/alerts/).

## Upgrading an agent from before the rename

Machines running an agent from when the project was called OpenRMM do
not need re-enrolling. The state directory moved with the name
(`/etc/openrmm` to `/etc/everwas`, `C:\ProgramData\OpenRMM\Agent` to
`C:\ProgramData\Everwas\Agent`), and the new build migrates it on first
start.

It moves the whole directory rather than the identity file alone,
because the directory also holds the 802.1X key and certificate, the
schedule cache and the script working area. If you ever move state by
hand, move the directory: copying `agent.json` on its own leaves a
machine enrolled and managed while silently dropping its network
identity, and that surfaces weeks later as an authentication failure with
no obvious cause.

Migration is skipped when the new location already holds an identity, so
a machine enrolled fresh under the new name is never rolled back to a
stale one, and skipped when `EVERWAS_STATE_DIR` is set, because an
operator who names the directory is telling the agent where it is.

## What just happened

The credentials issued at enrollment are only half the story. Every time
the agent connects, NATS defers the authorization decision to the server,
which pins that connection to subjects naming this one agent. No agent can
see another agent's traffic, jobs, or replies. The
[security model](/concepts/security-model/) walks through exactly how.
