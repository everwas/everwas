---
title: Enable the MCP server
description: Let an AI assistant query the fleet and, with explicit confirmation, act on it. Off by default, scoped by API key, fully audit-logged.
---

OpenRMM ships an MCP server so an AI assistant can query and, with
explicit confirmation, act on your fleet. It is **off by default** and
requires a scoped API key.

## Why this exists

An RMM knows things that are tedious to ask a database directly:

> Which machines had an outdated OpenSSL last Monday, and did we know
> that at the time?

That question needs both time axes of the bitemporal fact store, which is
exactly what `get_device_facts` exposes. See
[query device history](/guides/device-history/) for how to phrase these.

## Enabling it

```bash
# .env
OPENRMM_MCP_ENABLED=true
```

```bash
docker compose --profile mcp up -d openrmm-mcp
```

The MCP endpoint gets its own hostname (`OPENRMM_MCP_DOMAIN`, defaulting
to `mcp-<your-domain>`) rather than a path on the app origin. The reason
is the auth model: browsers talk to the app with a session cookie, MCP
clients present a bearer API key, and keeping the origins separate keeps
the cookie away from the API-key surface entirely.

## Mint a key with only the scopes you mean

```bash
make api-key NAME=claude SCOPES=devices:read,alerts:read,patches:read
# prints once: orpk_<id>_<secret>
```

Available scopes: `devices:read`, `alerts:read`, `alerts:write`,
`scripts:run`, `patches:read`, `patches:write`.

Scope enforcement is server-side. A read-only key cannot run scripts, no
matter what the assistant is asked or how persuasively it asks.

## Connecting

```bash
claude mcp add --transport http openrmm https://mcp-rmm.example.com/mcp \
  --header "Authorization: Bearer orpk_..."
```

In dev the server listens on `http://127.0.0.1:28001/mcp`.

## The safety model

Three properties, and they compose:

1. **Scopes cap capability.** The key decides what is possible;
   the assistant's instructions decide what is attempted.
2. **Mutations are two-step.** Every mutating tool takes
   `confirm: bool = False` and, without it, returns a dry-run preview of
   exactly what it *would* do. Nothing changes until the assistant is
   told to confirm, which means you can read the plan first.
3. **Everything is audit-logged.** Every call, successful or refused,
   writes an audit row naming the key as the actor. Anything an
   assistant did is visible in the Audit view alongside human actions.

The full tool list with scopes and semantics is in the
[MCP tool reference](/reference/mcp-tools/).
