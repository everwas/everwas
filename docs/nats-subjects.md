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

**Every grant names the agent. There are no shared subjects.** Three details
carry the isolation and are easy to get wrong:

1. **The agent must set a per-agent inbox prefix** (`nats.CustomInboxPrefix`
   with `_INBOX_{agent_id}`). The default `_INBOX` is shared by every client
   in the account, so a grant of `_INBOX.>` lets any one agent receive every
   other agent's request replies and pull-consumer deliveries, which includes
   the full job envelope with the script body that is about to run as root.
2. **The CONSUMER.CREATE grant pins the filter subject.** The JetStream API
   encodes the filter as a trailing token, so a `.>` grant lets an agent
   create its own durable filtering `jobs.*` and drain the whole fleet's work
   while the real devices silently never receive their jobs.
3. **Acks are scoped to the agent's own durable.** `$JS.ACK.>` would let an
   agent forge acks on the server's ingest consumers and drop results and
   audit events that would otherwise be redelivered.

A malformed identifier is as dangerous as a bad grant: `agent_id` is validated
as a UUID before interpolation, and `session_id`, `job_id`, and `entry_id` are
validated by the agent before they reach a subject string. An unvalidated
`session_id` of `>` makes the agent's own subscribe fail in a way that closes
its NATS connection permanently, which is a remote, unrecoverable brick.

The conformance tests assert a foreign-subject publish is refused and that no
grant is shared between two agents.

`agents.{id}.shell.{sid}.ctl` is **bidirectional**: the server publishes
`{"ack": n}` and `{"event":"ping"}`; the agent publishes `{"event":"closed"...}`
and `{"event":"gap"}`. Each side ignores shapes it does not own. Control
messages on `.ctl` are **bare JSON, not enveloped**.

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
| `agents.{id}.jobs.{job_id}.output` | JetStream `JOBOUT` (max-age 24 h) | chunks: `{stream: "stdout"\|"stderr", seq, data: <base64 ≤256KiB>, eof}`; per-job cap 8 MiB then truncate + flag. Scheduled runs also carry `entry_id` (see Scheduled jobs). |
| `agents.{id}.jobs.{job_id}.result` | JetStream `RESULTS` | terminal: `{status, exit_code, duration_ms, truncated}`. Patch jobs additionally carry `installed[]`, `failed{}`, `reboot_required` (omitted by script jobs). The job result is the authoritative record of what happened; the audit event carries the same facts but job state must not depend on a separate best-effort stream. |
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

Schedules fire from the agent's local cache, on the agent's clock, including
while it is offline. Nothing about a scheduled run is dispatched at fire time.

**Job id**: `uuid5(SCHED_NAMESPACE, "{entry_id}:{unix_unjittered_fire_ts}")`,
where `SCHED_NAMESPACE` is `06cadeed-8a30-50ab-87f5-7a27b043ba2d`. Both sides
derive it independently and must agree exactly, so each has a test asserting
the same vector (`TestJobIDMatchesTheServersDerivation` in Go,
`test_matches_the_agents_derivation` in Python). It is a UUID because results
ingest parses every job id as one; the earlier `sched:{entry}:{ts}` form parsed
as nothing and its results were logged as unknown runs and dropped. Deriving it
from the fire time is what makes a doubly-reported run idempotent.

Jitter is deterministic: `hash(agent_id, entry_id) % jitter_s`, so a fleet
spreads itself the same way every night and a given box is predictable.

**`entry_id` travels back on output chunks AND on the result.** The server has
no row for a scheduled run — it never queued one — so the entry is the only
thing that says which schedule a result belongs to. It is on the chunks too
because output arrives before the result: adopting the run only at the result
would keep the exit code and discard the whole of the job's stdout.

**Reconciliation, not push-on-change.** Every heartbeat carries the
`schedule_version` the agent holds. The server computes what that device's
version should be and pushes `cmd.{id}.sched.sync` when they differ. A push on
change alone is lost whenever the agent is offline or restarting, and nothing
notices; comparing every heartbeat means a device that missed an update fixes
itself within one heartbeat of coming back.

The version is derived from the entries themselves (crc32 of the canonical
JSON, masked to 31 bits), NOT a counter: there is no state to drift, a
dispatcher restart recomputes the same answer and resyncs nobody, and editing
one schedule does not change the version of devices it does not target. **An
empty entry list is version 0**, matching what a fresh agent reports, so a
fleet with no schedules is never pushed anything.

The `sched.sync` reply carries `rejected: [{entry_id, reason}]` for entries the
agent will not schedule (a cron it cannot parse, an unknown timezone). The
server must not go on believing those will fire.

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
