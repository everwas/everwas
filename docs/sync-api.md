# Sync API — the read surface for external systems of record

`/api/v1/sync` exists so an external inventory (a Nautobot SSoT job, a CMDB
loader, an asset system) can treat OpenRMM as a source of truth. It is
read-only, bearer-token authenticated, and built around one access pattern:
**sweep the whole fleet in pages, never fetch per device.** A 5,000-device
fleet is a handful of paged sweeps, not five thousand calls.

This file is the canonical contract; the docsite page mirrors it for
readers. Changes here are additive-only for existing fields.

## Authentication

Credentials are OpenRMM API keys exchanged for short-lived tokens.

1. Mint a scoped key (admin UI, or `POST /api/v1/admin/api-keys`). A sync
   consumer wants `devices:read`, plus `patches:read` if it reads patch
   state. The key looks like `orpk_<id>_<secret>` and is shown once.
2. Exchange it — RFC 6749 client credentials, form-encoded:

   ```
   POST /api/v1/auth/token
   grant_type=client_credentials&client_secret=orpk_<id>_<secret>
   ```

   (`client_id=<id>` + `client_secret=<secret>` also works, for OAuth2
   client libraries.) The response is standard:

   ```json
   {"access_token": "orst_...", "token_type": "Bearer",
    "expires_in": 3600, "scope": "devices:read patches:read"}
   ```

3. Call the sync endpoints with `Authorization: Bearer orst_...`. Re-exchange
   when the token expires; `expires_in` is `OPENRMM_SYNC_TOKEN_TTL_S`
   (default 3600).

Semantics worth knowing:

- **Scopes are frozen at issuance.** Narrowing a key's scopes affects new
  tokens; **revoking the key invalidates its outstanding tokens
  immediately** — verification re-checks the key row on every call.
- Session cookies never work on `/sync`, and sync tokens never work on the
  SPA routes. The two authentication roots are disjoint by design.
- Token issuance and refusal are audited under the key's name
  (`sync.token_issued` / `sync.token_refused`), as is nothing else the sync
  surface does — it is read-only.
- Failures are indistinguishable on purpose: unknown key, wrong secret, and
  expired key all read the same.

## Pagination — one contract everywhere

Every endpoint returns the same envelope:

```json
{"items": [...], "has_more": true, "next_cursor": "WyIw..."}
```

- `items` is **always a JSON array**. An empty collection is `[]`, never
  `null` — a reconciling consumer must never read absence as deletion.
- `cursor` is **opaque**: pass `next_cursor` back verbatim. Cursors are
  keyset-based and strictly advancing, and are **not portable between
  endpoints** (each encodes that endpoint's ordering key).
- Termination is unambiguous: the last page has `has_more: false` **and**
  `next_cursor: null`, together, always.
- `limit` is 1–200, default 100.
- A cursor that does not decode, or that came from a different endpoint, is
  a **422** — not an empty page. Garbage in means the client has a bug, and
  an empty 200 would bury it.
- A sweep is not a snapshot: rows ingested mid-walk may or may not appear.
  Consumers diff against their own state anyway; treat each full sweep as
  eventually consistent.

## Endpoints

All parameters are query parameters. `as_of` / `knew_at` / `since` require
timezone-aware ISO-8601; a naive timestamp is a 422.

| Endpoint | Scope | Extra parameters |
|---|---|---|
| `GET /sync/organizations` | `devices:read` | — |
| `GET /sync/sites` | `devices:read` | — |
| `GET /sync/devices` | `devices:read` | `site_id` |
| `GET /sync/interfaces` | `devices:read` | `device_id`, `site_id`, `as_of`, `knew_at` |
| `GET /sync/software` | `devices:read` | `device_id`, `site_id`, `as_of`, `knew_at` |
| `GET /sync/posture` | `devices:read` | `device_id`, `site_id`, `as_of`, `knew_at` |
| `GET /sync/patches` | `patches:read` | `device_id`, `site_id`, `as_of`, `knew_at` |
| `GET /sync/changes` | `devices:read` | `kind` (required), `since` (required), `device_id`, `site_id` |

Everything is scoped to the API key's organization. A key sees its own
org's sites, devices, and facts, and nothing else exists as far as the
responses are concerned.

### `GET /sync/organizations`

The caller's organization: `{id, name, description}`. One element, but
page-shaped like everything else so consumers walk every endpoint with the
same loop.

### `GET /sync/sites`

`{id, org_id, name, description, address, created_at}`. `description` and
`address` are operator-maintained (nullable) and exist for consumers whose
location models want them.

### `GET /sync/devices`

The devices-detailed sweep. Identity, placement, hardware, and address
rollup ride the **list** payload so no per-device follow-up is ever needed.

| Field | Type | Notes |
|---|---|---|
| `id` | uuid | Stable device id (UUIDv7, assigned at enrollment, survives everything except purge) |
| `org_id`, `site_id` | uuid, uuid? | Placement. `site_id` null until assigned |
| `hostname` | str | |
| `status` | str | `enrolled` `active` `offline` `retired`. Retired devices are included; removing them from the consumer is the consumer's policy |
| `tags` | [str] | |
| `agent_version`, `os_family`, `os_version`, `arch` | str | |
| `enrolled_at`, `last_heartbeat_at` | ts, ts? | |
| `manufacturer`, `model`, `serial_number`, `chassis_type` | str? | SMBIOS/DMI identity. **Null means "no agent has asserted this"** (agent older than the DMI release, or hardware without DMI tables) — never "empty". OEM placeholder junk is blanked agent-side |
| `cpu_model`, `cpu_cores`, `memory_bytes` | str?, int?, int? | From current hardware facts |
| `is_virtual` | bool? | Null when no hardware fact exists yet |
| `device_class` | str? | `vm` when virtual, else the chassis bucket (`desktop`, `laptop`, `server`, `all-in-one`, `tablet`, ...), else null |
| `dns_name` | null | Always null: OpenRMM does not track DNS names. The field exists so its absence is a statement, not a gap |
| `mac_addresses` | [str] | Rollup of current interfaces, loopbacks excluded |
| `ip_addresses` | [str] | Same rollup, **CIDR form preserved** (`192.0.2.10/24`) — the prefix length is what an IPAM consumer needs and cannot reconstruct |

### `GET /sync/interfaces`

One row per (device, interface), current beliefs by default:
`{device_id, key, name, mac, mtu, up, loopback, addresses[], observed_at}`.
`key` is the interface name — the stable per-device natural key. Loopbacks
are included here (filter on `loopback` if unwanted); only the device
rollup excludes them. `observed_at` is when the machine last reported this
state.

### `GET /sync/software`

`{device_id, name, version, observed_at}`. Name+version only — publisher,
install path, and install date are not collected by the agent today; if
that changes the fields will be added, not repurposed.

### `GET /sync/posture`

Security posture, one row per (device, security check) — per-check rather
than a rollup because the check set grows, and a check added since a
machine's last assessment never ran there, which is different from failing
there.

| Field | Type | Notes |
|---|---|---|
| `device_id` | uuid | |
| `check` | str | The check's stable name (`disk-encryption`, `firewall`, `antivirus`, ...) — the per-device natural key. A check **absent** from a device's rows never ran on it; absence is not a failure |
| `status` | str | Agent-defined verdict: `pass`, `fail`, `not_applicable` today, and new values may appear before consumers learn them. **Treat any unknown value as not-assessed, never as failed** — only an explicit `fail` is a failure. `not_applicable` means the check ran and could not assess that platform |
| `detail` | str | Human-oriented explanation. `""` when the agent gave none — the verdict stands on its own |
| `observed_at` | ts | When the machine last reported this verdict |

The three-state rule is load-bearing: a consumer gating network access on
posture (the l2trace remediation/quarantine integration) must read
"not assessed" as *no verdict*, because misreading it as `fail` cuts a
healthy machine off the network.

### `GET /sync/patches`

`{device_id, external_id, title, kind, severity, kb_ids[], cves[],
size_bytes, reboot_likely, status, unsupported, detail, observed_at,
first_seen_at}`.

`status` is the operator's standing decision — `approved`, `declined`, or
`pending` — with device-specific approvals shadowing fleet-wide ones. It is
**not install progress**; that lives in patch jobs, which are not part of
this surface.

### Time travel: `as_of` and `knew_at`

The four fact sweeps accept both bitemporal axes:

- `as_of` — valid time: *what was true on the machines at T?*
- `knew_at` — record time: *what did the server believe at T?*

They differ whenever an agent reports late or a later scan corrects an
earlier belief — exactly the case that matters after an incident. Default
(no parameters) is current belief about the present.

### `GET /sync/changes` — incremental sync

The alternative to re-sweeping: everything the server learned or unlearned
about one fact kind since a watermark **in record time**.

```
GET /sync/changes?kind=software&since=2026-08-18T00:00:00Z
```

Items: `{device_id, kind, fact_key, payload, change, at, valid_from,
valid_to}` where `change` is:

- `recorded` — a belief window opened (new fact, or the new value of an
  amended one, or a tombstone about the past).
- `superseded` — a belief window closed.

Replay rules for a consumer:

- A value change is one `superseded` (old value) + one `recorded` (new).
- A disappearance is one `superseded` + one `recorded` **tombstone with
  `valid_to` set** ("it was there until T"). A fact is current only while
  its latest `recorded` event has `valid_to: null`.
- Keying on record time is deliberate: an agent reporting late about last
  week still lands after your watermark. Use the timestamp you *started*
  the previous run as the next `since`, and expect occasional replays —
  processing must be idempotent.

## Volatile fields — exclude from diffs

A consumer that diffs whole payloads should denylist the fields that change
without meaning anything durable:

- `devices.last_heartbeat_at` — moves every heartbeat.
- `devices.status` — flaps `active` ↔ `offline` on the heartbeat threshold
  (90 s by default). Sync it as reachability telemetry, not as inventory.
- `*.observed_at` — moves with every agent report even when values do not.
  Posture is the sharpest case: every assessment cycle moves it even when
  every verdict is unchanged, so diff posture on `(check, status, detail)`.
- `patches.status` — while an approval workflow is mid-flight.

## Errors

| Code | Meaning |
|---|---|
| 401 | Missing/invalid/expired token, or a raw `orpk_` key where a token belongs (the message says how to exchange it) |
| 403 | Token lacks the required scope; the response names the scopes it holds |
| 404 | Resource outside your organization (indistinguishable from nonexistent, by design) |
| 422 | Naive datetime, undecodable cursor, cursor from another endpoint, unknown `kind` |
| 5xx | Server fault. Retry with backoff; every sync call is a GET, so blind retry is safe |

The server does not currently emit 429. It reserves the right to; a
consumer should already honor `Retry-After` on 429 and back off on 5xx.

## Version dependencies

- `manufacturer` / `model` / `serial_number` / `chassis_type` require
  agents at or above the release that collects DMI identity. Older agents
  simply leave them null — the server records no false "no serial" belief.
- `sites.description` / `sites.address` and `organizations.description`
  exist from schema revision 0019.
