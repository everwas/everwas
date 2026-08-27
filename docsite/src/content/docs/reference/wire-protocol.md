---
title: Wire protocol (NATS subjects)
description: The subject namespace, message envelope, and stream layout that connect agents to the server.
---

:::note
The canonical contract is `docs/nats-subjects.md` in the repository; the
server's `natsio/subjects.py` and the agent's `wire/subjects.go` are both
written against that file and hold matching conformance tests. This page
mirrors it for readers; when in doubt, the repo file wins.
:::

## Identity

- `agent_id` is a UUIDv7, assigned by the server at enrollment and used
  verbatim in subjects.
- Agents authenticate to NATS with `user = agent_id` and the secret
  issued at enrollment over HTTPS. The NATS server delegates the
  authorization decision to the **auth-callout responder** running in the
  dispatcher, which verifies the secret against PostgreSQL and returns a
  user JWT pinned to that agent's permissions.

## Agent permissions

```text
publish:   agents.{agent_id}.>
           _INBOX_{agent_id}.>
           $JS.API.CONSUMER.CREATE.JOBS.agent-{agent_id}
           $JS.API.CONSUMER.CREATE.JOBS.agent-{agent_id}.jobs.{agent_id}
           $JS.API.CONSUMER.INFO.JOBS.agent-{agent_id}
           $JS.API.CONSUMER.MSG.NEXT.JOBS.agent-{agent_id}
           $JS.ACK.JOBS.agent-{agent_id}.>
subscribe: cmd.{agent_id}.>   jobs.{agent_id}
           agents.{agent_id}.shell.*.in   agents.{agent_id}.shell.*.rsz
           agents.{agent_id}.shell.*.ctl
           _INBOX_{agent_id}.>
```

**Every grant names the agent. There are no shared subjects.** Three
details carry the isolation and are easy to get wrong:

1. **The agent sets a per-agent inbox prefix** (`_INBOX_{agent_id}`).
   The default `_INBOX` is shared by every client in the account, so a
   grant of `_INBOX.>` would let any one agent receive every other
   agent's request replies and pull-consumer deliveries, including the
   full job envelope with the script body that is about to run as root.
2. **The CONSUMER.CREATE grant pins the filter subject.** The JetStream
   API encodes the filter as a trailing token, so a `.>` grant would let
   an agent create a durable filtering `jobs.*` and drain the whole
   fleet's work while the real devices silently never receive their jobs.
3. **Acks are scoped to the agent's own durable.** `$JS.ACK.>` would let
   an agent forge acks on the server's ingest consumers and drop results
   and audit events that would otherwise be redelivered.

A malformed identifier is as dangerous as a bad grant: `agent_id` is
validated as a UUID before interpolation, and `session_id`, `job_id`, and
`entry_id` are validated by the agent before they reach a subject string.

The conformance tests assert that a foreign-subject publish is refused
and that no grant is shared between two agents.

## Envelope

All JSON messages share:

```json
{
  "v": 1,
  "type": "telemetry",
  "agent_id": "0198f6f2-…",
  "msg_id": "01J9XKZ8…",
  "ts": "2026-08-15T12:00:00Z",
  "data": { }
}
```

- `msg_id` is a ULID, mirrored into the `Nats-Msg-Id` header on JetStream
  publishes for server-side dedup, so spool redelivery is safe.
- Unknown fields are ignored (forward compatibility). `v` bumps only on
  breaking changes; additive changes are always allowed.
- **Raw-bytes exceptions** (no JSON envelope): shell byte streams
  (`…shell.{sid}.in` / `.out`) and chunk payload framing where noted.
  Control messages on `.ctl` are bare JSON, not enveloped.

## Agent to server

| Subject | Transport | Cadence / notes |
|---|---|---|
| `agents.{id}.heartbeat` | core NATS | every 30 s with jitter; carries `version`, `uptime_s`, `schedule_version`, `seq`. Offline threshold 90 s. Never spooled |
| `agents.{id}.telemetry` | JetStream `TELEMETRY` (max-age 48 h) | every 60 s; CPU, memory, swap, load, per-mount disk, network, service-state deltas |
| `agents.{id}.inventory.{kind}` | JetStream `INVENTORY` (per-subject max-msgs 1) | kind is one of `hardware`, `software`, `network`, `logins`, `posture`, `processes`, `services`, `patchstate`; full snapshot plus `snapshot_hash` |
| `agents.{id}.jobs.{job_id}.progress` | core NATS | `{seq, pct, phase, note}` |
| `agents.{id}.jobs.{job_id}.output` | JetStream `JOBOUT` (max-age 24 h) | chunks of stdout/stderr, base64, 256 KiB max each; 8 MiB per-job cap, then truncate and flag. Scheduled runs also carry `entry_id` |
| `agents.{id}.jobs.{job_id}.result` | JetStream `RESULTS` | terminal: `{status, exit_code, duration_ms, truncated}`. Patch jobs add `installed[]`, `failed{}`, `reboot_required` |
| `agents.{id}.events` | JetStream `EVENTS` | audit: `enrolled`, `shell.opened/closed`, `script.executed`, `patch.installed`, `agent.updated`, `sched.misfire_skipped`, `policy.violation` |
| `agents.{id}.shell.{sid}.out` | core NATS, raw bytes | PTY output, frames up to 32 KiB |
| `agents.{id}.shell.{sid}.ctl` | core NATS | agent-side control: `{event: "closed"\|"error"\|"gap", …}` |

## Server to agent

| Subject | Mechanism | Notes |
|---|---|---|
| `jobs.{agent_id}` | JetStream `JOBS` (max-age 7 d), per-agent durable pull consumer | durable job delivery: `script.run`, `patch.scan`, `patch.install`, `inventory.refresh`. Survives agent downtime; executes on reconnect |
| `cmd.{id}.ping` | request/reply | liveness probe |
| `cmd.{id}.shell.open` | request/reply | `{session_id, shell, cols, rows, idle_timeout_s, requested_by}` |
| `cmd.{id}.shell.close` | request/reply | teardown |
| `cmd.{id}.job.cancel` | request/reply | `{job_id}` |
| `cmd.{id}.sched.sync` | request/reply | full schedule document; the agent persists it and acks with the version |
| `cmd.{id}.agent.update` | request/reply, then job | `{version, url, sha256, minisign_sig}` |
| `cmd.{id}.agent.rotate_creds` | request/reply | issue a new secret |
| `agents.{id}.shell.{sid}.in` | core NATS, raw bytes | PTY input |
| `agents.{id}.shell.{sid}.rsz` | core NATS | `{cols, rows}` |

Long-running commands are **two-phase**: the agent replies immediately
with `{accepted: true, job_id}` or `{accepted: false, error}`, then
streams on `agents.{id}.jobs.{job_id}.*`. Requests carry `requested_by`
so audit events name a human.

## Shell flow control

Core NATS does not buffer for slow consumers, so backpressure is
explicit:

1. The agent publishes PTY output in frames up to 32 KiB and counts
   un-acked bytes.
2. The server bridge acks via `.ctl` with `{"ack": n_bytes}` only after
   the browser WebSocket write completes, so TCP backpressure propagates
   end to end.
3. The agent pauses PTY reads when un-acked bytes exceed 512 KiB.
4. On NATS disconnect the agent keeps the PTY alive for a 60 s grace
   period, buffering up to 256 KiB in a drop-oldest ring; on resume it
   sends `{event: "gap"}` if anything was dropped. Past grace, teardown.
5. The server console pings via `.ctl` every 30 s; the agent tears down
   after two missed pings, since core NATS will not announce a dead
   server.

The `.ctl` subject is bidirectional: the server publishes acks and pings,
the agent publishes `closed` and `gap`. Each side ignores shapes it does
not own.

## Scheduled jobs

Schedules fire from the agent's local cache, on the agent's clock,
including while offline. Nothing about a scheduled run is dispatched at
fire time.

- **Job id** is `uuid5(SCHED_NAMESPACE, "{entry_id}:{unix_unjittered_fire_ts}")`
  with `SCHED_NAMESPACE = 06cadeed-8a30-50ab-87f5-7a27b043ba2d`. Both
  sides derive it independently and each carries a test asserting the
  same vector. Deriving it from the fire time makes a doubly-reported
  run idempotent.
- **Jitter is deterministic**: `hash(agent_id, entry_id) % jitter_s`, so
  a fleet spreads itself the same way every night and a given box is
  predictable.
- **`entry_id` travels back on output chunks and on the result.** The
  server never queued a scheduled run, so the entry id is the only thing
  that says which schedule a result belongs to, and output arrives
  before the result.
- **Reconciliation, not push-on-change.** Every heartbeat carries the
  agent's `schedule_version`; the server pushes `sched.sync` on any
  mismatch. The version is a crc32 of the canonical entries (an empty
  list is version 0), so there is no counter to drift and a dispatcher
  restart resyncs nobody.
- The `sched.sync` reply carries `rejected: [{entry_id, reason}]` for
  entries the agent will not schedule, so the server does not go on
  believing they will fire.

## Server to verifier (posture egress)

Opt-in, off by default. When `EVERWAS_POSTURE_EGRESS_SUBJECT` is set, the
dispatcher pushes **one envelope per device per posture collection** to
that subject (core NATS, on the server's existing connection) immediately
after the collection's facts commit. l2trace's ingress consumes
`l2trace.posture`. An empty setting means no publisher, which is the safe
state: a verifier reads absence as not-assessed, and not-assessed never
gates.

```json
{
  "device_id": "01a00b45-0e50-78c8-b572-8b8fbc272ad1",
  "hostname": "deb01",
  "agent_version": "2026.08.20",
  "macs": ["52:54:00:12:34:56"],
  "collected_at": "2026-08-27T12:00:00Z",
  "ingested_at": "2026-08-27T12:00:02Z",
  "checks": [
    {"check": "disk-encryption", "category": "encryption", "status": "fail",
     "detail": "the root filesystem is on unencrypted storage", "took_ms": 1}
  ]
}
```

- `checks` is the agent's serialised posture verbatim: three statuses
  (`pass` / `fail` / `not_assessed`) with `not_assessed_reason`
  distinguishing `not_applicable` from `undetermined`, plus the forensic
  extras (`detail`, `evidence`, `took_ms`). The server passes it through
  untouched.
- `macs` is the device's current MAC set from its network facts,
  loopbacks excluded, deduplicated, sorted. It is the only join material
  for devices that authenticate by MAB.
- `ingested_at` is the server's clock and is what the verifier's
  freshness window (2 h against the 30 min collection cadence) gates on;
  `collected_at` is the endpoint's clock, forensic only.
- Delivery is fire-and-forget: a publish failure is logged and ingest
  proceeds. The next collection is the retry, and a stale entry on the
  verifier degrades to not-assessed.

## JetStream streams

Created and owned by the server:

| Stream | Subjects | Policy |
|---|---|---|
| `TELEMETRY` | `agents.*.telemetry` | limits, max-age 48 h |
| `INVENTORY` | `agents.*.inventory.>` | limits, max-msgs-per-subject 1 |
| `JOBOUT` | `agents.*.jobs.*.output` | limits, max-age 24 h, per-subject byte cap |
| `RESULTS` | `agents.*.jobs.*.result` | workqueue |
| `EVENTS` | `agents.*.events` | limits, max-age 90 d |
| `JOBS` | `jobs.*` | limits, max-age 7 d; per-agent durable pull consumers |

## Transport

Default: the NATS websocket listener proxied by Caddy on 443 (`wss://`),
firewall-friendly with no separate cert management. Plain `tls://:4222`
remains available for LAN deployments. The reasoning is recorded in
[ADR-0001](/decisions/0001-nats-transport/).
