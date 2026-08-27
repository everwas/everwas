---
title: Architecture
description: One static agent, a NATS backbone, and two server processes over PostgreSQL.
---

```mermaid
flowchart LR
  subgraph endpoints["Endpoints · Apache-2.0"]
    AG["everwas-agent<br/>(Go, one static binary)<br/>Windows · macOS · Linux"]
  end

  subgraph backbone["Backbone"]
    NATS["NATS + JetStream<br/>per-agent subject permissions"]
  end

  subgraph server["Server · AGPL-3.0 · one Python package"]
    API["everwas-api<br/>web app · HTTP API · sync API · device CA"]
    DISP["everwas-dispatcher<br/>ingest · alerts · schedules · auth callout"]
    MCP["everwas-mcp<br/>fleet tools for assistants"]
    PG[("PostgreSQL<br/>partitioned telemetry ·<br/>bitemporal facts")]
  end

  AG -- "outbound wss:// :443" --> NATS
  AG -- "HTTPS: enroll · certificate pull" --> API
  NATS <-- "ingest · job delivery" --> DISP
  API <-- "request/reply: shell · jobs" --> NATS
  DISP --> PG
  API <--> PG
  MCP --> PG
  MCP -- "confirmed actions" --> NATS
  DISP -- "email · webhook · ntfy · gotify" --> OUT["Notification outbox"]

  BROWSER["Browser"] --> API
  ASSISTANT["AI assistant"] --> MCP
  SOR["External systems of record"] -- "read-only sweep + change feed" --> API
```

## The agent

A single static Go binary per endpoint, built with `CGO_ENABLED=0`, so
there is nothing to install alongside it. It runs as a system service
(systemd, launchd, or the Windows SCM), collects telemetry and
inventory, executes jobs, and hosts the PTY behind the browser terminal.

It connects **outbound only**, `wss://` on port 443 by default, which
traverses NAT and corporate egress policies without firewall rules on
the endpoint side. The transport choice is recorded in
[ADR-0001](/decisions/0001-nats-transport/).

Updates are self-applied: the server offers a signed build (sha256 plus a
minisign signature), the agent verifies, swaps itself, and rolls back
automatically if the new binary fails to come up.

## The backbone

NATS with JetStream sits between agents and the server, doing three jobs
that would otherwise each need their own infrastructure:

- **Request/reply** maps onto "send this command to agent X and await a
  response" with no hand-rolled correlation layer.
- **Per-subject authorization** gives each agent a namespace of its own;
  the [security model](/concepts/security-model/) covers how.
- **JetStream** provides durable job delivery to offline agents and
  ingest buffering, without adding Redis or a task queue.

The full subject namespace is in the
[wire protocol reference](/reference/wire-protocol/).

## The server

Two processes from one Python package:

- **`everwas-api`**: the FastAPI application serving the web app and the
  HTTP API from a single origin. Enrollment also lands here, over HTTPS.
- **`everwas-dispatcher`**: an asyncio process consuming the JetStream
  ingest streams, evaluating alert rules, reconciling schedules, and
  answering NATS auth-callout requests.

State lives in PostgreSQL. Telemetry goes into daily range-partitioned
tables (plain PostgreSQL, deliberately not TimescaleDB; see
[ADR-0002](/decisions/0002-postgres-partitioning/)), and inventory facts
are stored [bitemporally](/concepts/bitemporal/).

## The front door

Every HTTP surface (web app, API, the NATS websocket listener, the
optional MCP server) is published through caddy-docker-proxy labels, so
one Caddy instance owns TLS for all of it. The NATS listener gets a
hostname of its own for a concrete reason: the Go NATS client ignores
the path portion of a websocket URL, so it cannot live at a path on the
main domain.

## The bigger picture

Everwas is one member of a family of systems that meet in Nautobot, the
source of truth. Solid lines exist today; dashed lines are designed
(ADR-0003/0004) but not yet wired.

```mermaid
flowchart TB
  subgraph edge["The network edge"]
    EP["Endpoints<br/>Windows · macOS · Linux"]
    SW["Switches & WLCs"]
  end

  subgraph ew["Everwas"]
    EWS["Server<br/>bitemporal inventory · patch state ·<br/>posture checks · device CA"]
  end

  subgraph l2["l2trace"]
    L2S["Bitemporal CAM/MAC store ·<br/>L2 traceroute · RADIUS/802.1X<br/>(monitor-first)"]
  end

  subgraph nb["Nautobot · the source of truth"]
    SSOTE["nautobot-ssot-everwas"]
    RMM["nautobot-app-rmm-models<br/>vendor-neutral RMM schema ·<br/>bitemporal belief log"]
    SSOTL["nautobot-ssot-l2trace<br/>observed L2 vs intent,<br/>review-gated"]
    SCAN["nautobot-app-scanner<br/>nmap discovery"]
    CORE["Nautobot core<br/>DCIM · IPAM · Tenancy"]
  end

  EP -- "everwas-agent, outbound only" --> EWS
  EP -- "802.1X (EAP)" --> SW
  SW -- "RADIUS" --> L2S
  SW -- "gNMI: CAM/MAC · adjacency" --> L2S

  EWS -- "sync API<br/>(ewpk_ key → ewst_ token)" --> SSOTE
  SSOTE --> RMM
  RMM --> CORE
  NINJA["nautobot-ssot-ninjaone<br/>(structural twin)"] -.-> RMM

  L2S -- "observations" --> SSOTL
  SSOTL -- "findings, human-applied" --> CORE
  CORE -- "authors 802.1X policy,<br/>cached locally in l2trace" --> L2S
  SCAN --> CORE

  AI["AI assistants"] -- "MCP" --> EWS
  AI -- "MCP (mcnautobot)" --> CORE

  EWS -. "planned: device certificates (EAP-TLS)<br/>and posture verdicts for remediation" .-> L2S
```

Three properties of this constellation are deliberate:

- **Nautobot is where the systems meet.** Everwas and l2trace do not
  talk to each other directly. Everwas reports what is true *on*
  machines; l2trace observes what is true *between* them; Nautobot
  reconciles both against intent.
- **Bitemporality is the family trait.** Everwas facts, l2trace
  observations, and the rmm-models belief log all carry both time axes,
  so "what did we believe last Tuesday" survives every hop.
- **Discovery never overwrites intent.** The l2trace and scanner apps
  land findings as review-gated proposals. A human applies them; the
  sync from Everwas writes only into its own vendor-neutral models.
