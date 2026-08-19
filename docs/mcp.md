# MCP server

Everwas ships an MCP server so an AI assistant can query and (with explicit
confirmation) act on your fleet. It is **off by default** and requires a scoped
API key.

## Why this exists

An RMM knows things that are tedious to ask a database directly:

> Which machines had an outdated OpenSSL last Monday, and did we know that at
> the time?

That question needs both time axes of the bitemporal fact store, which is
exactly what `get_device_facts` exposes.

## Enabling it

```bash
# .env
EVERWAS_MCP_ENABLED=true

docker compose --profile mcp up -d everwas-mcp
```

Mint a key with only the scopes you want the assistant to have:

```bash
make api-key NAME=claude SCOPES=devices:read,alerts:read,patches:read
# prints once: ewpk_<id>_<secret>
```

Scopes: `devices:read`, `alerts:read`, `alerts:write`, `scripts:run`,
`patches:read`, `patches:write`. A read-only key cannot run scripts, no matter
what the assistant is asked to do.

## Connecting

```bash
claude mcp add --transport http everwas https://rmm.example.com/mcp \
  --header "Authorization: Bearer ewpk_..."
```

In dev the server listens on `http://127.0.0.1:28001/mcp`.

## Tools

| Tool | Scope | Notes |
|---|---|---|
| `list_devices` | devices:read | filter by status, tag, hostname substring |
| `get_device` | devices:read | detail plus latest CPU/memory/disk |
| `get_device_facts` | devices:read | **the time machine**: `as_of` + `knew_at` |
| `diff_device_facts` | devices:read | what changed between two moments |
| `list_alerts` | alerts:read | firing, acknowledged, or resolved |
| `list_pending_patches` | patches:read | pending updates with approval state |
| `acknowledge_alert` | alerts:write | requires `confirm: true` |
| `run_script` | scripts:run | requires `confirm: true`; by script name |
| `approve_patches` | patches:write | approves only; never installs |

## The two time axes

This is the part worth understanding, because the two questions look alike and
are not:

- **`as_of`** is *valid time*: when was this true on the machine?
  "What was installed on web-01 last Tuesday?"
- **`knew_at`** is *record time*: when did the server believe it?
  "What did we think was installed on web-01, as of last Tuesday's report?"

They differ whenever an agent reports late, or a scan corrects an earlier
belief. After an incident, `knew_at` is what tells you whether your monitoring
knew about a vulnerable package at the time, or only learned later.

## Safety

Every mutating tool takes `confirm: bool = False` and returns a dry-run preview
listing exactly what it *would* do. Nothing changes until the assistant is told
to confirm, which means a user can read the plan first.

Every tool call, successful or refused, writes an `audit_log` row with the key's
name as the actor. Anything an assistant did is visible in the Audit view
alongside human actions.
