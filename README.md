# OpenRMM

Open-source remote monitoring and management for Windows, macOS, and Linux.

OpenRMM gives you fleet monitoring and alerting, a live remote terminal in the
browser, a script library you can run across machines, hardware and software
inventory, and OS patch management — self-hosted, with a single static agent
binary per endpoint.

## Why another RMM?

The commercial options are excellent and expensive. The open-source options are
either monitoring-only or carry licenses that stop you building on them. OpenRMM
is genuinely open source: the server is AGPL-3.0, the agent is Apache-2.0, and
contributions are accepted under the [DCO](DCO).

Two things you won't find elsewhere:

**Bitemporal device history.** Every inventory fact (hardware, installed
software, patch state) is recorded on two time axes: when it was true on the
machine, and when the server learned it. Ask "what did this device look like
last Tuesday?" and get a real answer — including what you *believed* last
Tuesday, which is not always the same thing.

**A first-class MCP server.** AI assistants can query the fleet, inspect device
history, and (with explicit confirmation and full audit logging) run scripts
and approve patches.

## Architecture

```
Endpoint agents (Go, one static binary)
        │  outbound wss:// only — works behind NAT/firewalls
        ▼
NATS + JetStream  ◄──►  Dispatcher (ingest, alerting, scheduling)
        ▲                      │
        │                      ▼
   API (FastAPI) ◄──────► PostgreSQL
        ▲
        ▼
Web dashboard (React + shadcn/ui)
```

- Per-agent NATS credentials: an agent cannot see any other agent's traffic
- Jobs queue durably — a script sent to an offline laptop runs when it returns
- Shell sessions are recorded (asciicast) and fully audited

## Status

Early development. The [protocol contract](docs/nats-subjects.md) and
[architecture docs](docs/) are the best place to start reading.

## Development

Requirements: Docker, `uv`, Go 1.22+, Node 20+.

```bash
cp .env.example .env
make dev        # bring up the dev stack (hot reload)
make migrate    # apply database migrations
make admin      # create the first admin user
```

## License

- `server/`, `web/`, and everything not otherwise marked: [AGPL-3.0](LICENSE)
- `agent/`: [Apache-2.0](agent/LICENSE)
