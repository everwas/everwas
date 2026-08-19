# Everwas

[![CI](https://github.com/everwas/everwas/actions/workflows/ci.yml/badge.svg)](https://github.com/everwas/everwas/actions/workflows/ci.yml)
[![Server: AGPL-3.0](https://img.shields.io/badge/server-AGPL--3.0-b97f3a)](LICENSE)
[![Agent: Apache-2.0](https://img.shields.io/badge/agent-Apache--2.0-5fc9e8)](agent/LICENSE)

Open-source remote monitoring and management for Windows, macOS, and Linux.
Website: [everwas.supported.systems](https://everwas.supported.systems) ·
Docs: [docs.everwas.supported.systems](https://docs.everwas.supported.systems) ·
[Contributing](CONTRIBUTING.md)

Monitor a fleet, get alerted when something breaks, open a real terminal on any
machine from your browser, run scripts across hundreds of endpoints, and keep
OS patches under control. Self-hosted, with a single static agent binary per
endpoint.

## Why another RMM?

The commercial options are excellent and expensive. The open-source options are
either monitoring-only or carry licenses that stop you building on them.
Everwas is genuinely open source: the server is AGPL-3.0, the agent is
Apache-2.0, and contributions are accepted under the [DCO](DCO).

Two things you will not find elsewhere:

**Bitemporal device history.** Every inventory fact is recorded on two time
axes: when it was true on the machine, and when the server learned it. Ask
"what was installed on this device last Tuesday?" and get a real answer,
including what you *believed* last Tuesday, which after an incident is a
different and more uncomfortable question. See [docs/bitemporal](docs/).

**A first-class MCP server.** AI assistants can query the fleet, inspect device
history across both time axes, and with explicit confirmation run scripts and
approve patches. Read-only by default, scoped by API key, every call
audit-logged. See [docs/mcp.md](docs/mcp.md).

## What works today

| Area | Capability |
|---|---|
| Monitoring | Agent enrollment, heartbeats, CPU/memory/disk/network telemetry, live charts |
| Alerting | Threshold rules with duration windows and cooldowns, heartbeat-miss detection, email/webhook/ntfy/gotify delivery with retries |
| Remote access | Browser terminal (real PTY) with session recording and playback |
| Automation | Script library, fleet-wide runs, cron schedules, durable delivery to offline machines |
| Inventory | Hardware, installed software, processes, services, with time-machine history |
| Patching | Scan, approve, deploy for apt, dnf, pacman, Windows Update, and macOS softwareupdate |
| Agent ops | Signed self-update with automatic rollback, service install for systemd/launchd/SCM |

## Architecture

```
Endpoint agents (Go, one static binary)
        │  outbound wss:// only, works behind NAT and firewalls
        ▼
NATS + JetStream  ◄──►  Dispatcher (ingest, alerting, scheduling)
        ▲                      │
        │                      ▼
   API (FastAPI) ◄──────► PostgreSQL
        ▲
        ▼
Web dashboard (React + shadcn/ui)      MCP server (opt-in)
```

- Per-agent NATS credentials issued by an auth callout: an agent cannot see any
  other agent's traffic, and revoking one is a database update
- Jobs queue durably, so a script sent to a sleeping laptop runs when it wakes
- Shell sessions are recorded (asciicast) and fully audited

## Quick start

Requirements: Docker, `uv`, Go 1.22+, Node 20+.

```bash
git clone https://github.com/everwas/everwas && cd everwas
cp .env.example .env
cd server && uv run everwas gen-nats-keys   # paste both values into .env
cd .. && make dev                            # bring up the stack (hot reload)
make migrate
make admin EMAIL=you@example.com
```

Then enroll a machine:

```bash
make enroll-token                            # prints ew_...
cd agent && go build -o bin/everwas-agent ./cmd/everwas-agent
sudo ./bin/everwas-agent install --server http://localhost:28000 --token ew_...
```

The dashboard is at http://localhost:25173.

## Documentation

- [docs/nats-subjects.md](docs/nats-subjects.md) — the agent/server wire contract
- [docs/mcp.md](docs/mcp.md) — MCP server, tools, and the two time axes
- [docs/adr/](docs/adr/) — architecture decisions and why

## License

- `server/`, `web/`, and everything not otherwise marked: [AGPL-3.0](LICENSE)
- `agent/`: [Apache-2.0](agent/LICENSE)
