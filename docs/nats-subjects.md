# OpenRMM Wire Protocol — NATS Subjects & Envelope (v1)

This document is the contract between the server (`server/src/openrmm/natsio/subjects.py`)
and the agent (`agent/internal/wire/subjects.go`). Both implementations are written
against this file. Changes here require bumping the envelope `v` only if breaking;
additive changes (new subjects, new optional fields) are always allowed.

## Identity

- `agent_id`: UUIDv7, assigned by the server at enrollment. Used verbatim in subjects.
- Agents authenticate to NATS with `user = agent_id`, `pass = agent_secret`
  (issued at enrollment over HTTPS). The NATS server delegates authorization to
  the **auth-callout responder** (runs in the dispatcher), which verifies the
  secret against PostgreSQL and returns a user JWT pinned to the permissions below.

## Agent permissions (issued by auth-callout)

```
publish:   agents.{agent_id}.>   _INBOX.>
subscribe: cmd.{agent_id}.>   jobs.{agent_id}
           agents.{agent_id}.shell.*.in   agents.{agent_id}.shell.*.rsz
           _INBOX.>
```

An agent cannot publish or subscribe outside its own namespace. The M1
conformance test asserts a foreign-subject publish is refused.

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
  publishes for server-side dedup (spool redelivery is safe).
- Unknown fields are ignored (forward compatibility). `v` bumps only on
  breaking changes.
- **Raw-bytes exceptions** (no JSON envelope): shell byte streams
  (`…shell.{sid}.in` / `.out`) and chunk payload framing where noted. All
  semantics live in the subject name and NATS headers.

## Agent → server

| Subject | Transport | Cadence / notes |
|---|---|---|
| `agents.{id}.heartbeat` | core NATS | every 30 s ± jitter; `data`: `{version, uptime_s, schedule_version, seq}`. Offline threshold: 90 s. Never spooled. |
| `agents.{id}.telemetry` | JetStream `TELEMETRY` (max-age 48 h) | every 60 s; CPU/mem/swap/load, per-mount disk, net, service states (delta) |
| `agents.{id}.inventory.{kind}` | JetStream `INVENTORY` (per-subject max-msgs 1) | kind ∈ `hardware` `software` `processes` `services` `patchstate`; full snapshot + `snapshot_hash` |
| `agents.{id}.jobs.{job_id}.progress` | core NATS | `{seq, pct, phase, note}` |
| `agents.{id}.jobs.{job_id}.output` | JetStream `JOBOUT` (max-age 24 h) | chunks: `{stream: "stdout"\|"stderr", seq, data: <base64 ≤256KiB>, eof}`; per-job cap 8 MiB then truncate + flag |
| `agents.{id}.jobs.{job_id}.result` | JetStream `RESULTS` | terminal: `{status, exit_code, duration_ms, truncated}` |
| `agents.{id}.events` | JetStream `EVENTS` | audit: `enrolled`, `shell.opened/closed`, `script.executed`, `patch.installed`, `agent.updated`, `sched.misfire_skipped`, `policy.violation` |
| `agents.{id}.shell.{sid}.out` | core NATS, **raw bytes** | PTY output, frames ≤ 32 KiB |
| `agents.{id}.shell.{sid}.ctl` | core NATS | agent-side control: `{event: "closed"\|"error"\|"gap", …}` |

## Server → agent

| Subject | Mechanism | Notes |
|---|---|---|
| `jobs.{agent_id}` | JetStream `JOBS` (max-age 7 d), per-agent durable pull consumer | **durable job delivery**: `script.run`, `patch.scan`, `patch.install`, `inventory.refresh`. Survives agent downtime; executes on reconnect. `job_id` is server-assigned. |
| `cmd.{id}.ping` | request/reply | liveness probe |
| `cmd.{id}.shell.open` | request/reply | `{session_id, shell, cols, rows, idle_timeout_s, requested_by}` → `{accepted, error?}` |
| `cmd.{id}.shell.close` | request/reply | teardown |
| `cmd.{id}.job.cancel` | request/reply | `{job_id}` |
| `cmd.{id}.sched.sync` | request/reply | full schedule document; agent persists and acks with version |
| `cmd.{id}.agent.update` | request/reply(ack) → job | `{version, url, sha256, minisign_sig}` |
| `cmd.{id}.agent.rotate_creds` | request/reply | issue new secret |
| `agents.{id}.shell.{sid}.in` | core NATS, **raw bytes** | PTY input (server publishes into agent namespace) |
| `agents.{id}.shell.{sid}.rsz` | core NATS | `{cols, rows}` |

Long-running commands are **two-phase**: the agent replies immediately with
`{accepted: true, job_id}` (or `{accepted: false, error}`), then streams on
`agents.{id}.jobs.{job_id}.*`. Requests carry `requested_by` so audit events
name a human.

## Shell flow control

Core NATS does not buffer for slow consumers, so backpressure is explicit:

1. Agent publishes PTY output in frames ≤ 32 KiB and counts un-acked bytes.
2. The server bridge acks via `agents.{id}.shell.{sid}.ctl` → `{"ack": n_bytes}`
   only after the browser WebSocket write completes (TCP backpressure propagates).
3. Agent pauses PTY reads when un-acked > 512 KiB.
4. On NATS disconnect the agent keeps the PTY alive for a 60 s grace period,
   buffering ≤ 256 KiB in a ring (drop-oldest); on resume it sends
   `{event: "gap"}` if anything was dropped. Past grace: teardown.
5. The server console pings via `.ctl` `{event: "ping"}` every 30 s; the agent
   tears down after two missed pings (server died — core NATS won't tell us).

## Scheduled jobs

Schedules fire from the agent's local cache while offline. Scheduled runs use
`job_id = sched:{entry_id}:{fire_ts}` — idempotent server-side if reported twice.
Jitter is deterministic: `hash(agent_id, entry_id) % jitter_s`.

## JetStream streams (created and owned by the server)

| Stream | Subjects | Policy |
|---|---|---|
| `TELEMETRY` | `agents.*.telemetry` | limits, max-age 48 h |
| `INVENTORY` | `agents.*.inventory.>` | limits, max-msgs-per-subject 1 |
| `JOBOUT` | `agents.*.jobs.*.output` | limits, max-age 24 h, per-subject byte cap |
| `RESULTS` | `agents.*.jobs.*.result` | workqueue |
| `EVENTS` | `agents.*.events` | limits, max-age 90 d |
| `JOBS` | `jobs.*` | limits, max-age 7 d; per-agent durable pull consumers |

## Transport

Default: NATS **websocket listener** proxied by Caddy on 443 (`wss://`) —
firewall-friendly and no separate cert management. Plain `tls://:4222` remains
available for LAN deployments.
