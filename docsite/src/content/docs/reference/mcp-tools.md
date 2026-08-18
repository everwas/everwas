---
title: MCP tools
description: Every tool the OpenRMM MCP server exposes, the scope each requires, and the confirmation contract for mutations.
---

Setup and the safety model are covered in
[enable the MCP server](/guides/enable-mcp/). This page is the tool list.

## Read tools

| Tool | Scope | Notes |
|---|---|---|
| `list_devices` | `devices:read` | Filter by status, tag, or hostname substring |
| `get_device` | `devices:read` | Device detail plus latest CPU, memory, and disk |
| `get_device_facts` | `devices:read` | The time machine: takes `as_of` and `knew_at` |
| `diff_device_facts` | `devices:read` | What changed on a device between two moments |
| `list_alerts` | `alerts:read` | Firing, acknowledged, or resolved |
| `list_pending_patches` | `patches:read` | Pending updates with approval state |

### The two time parameters

`get_device_facts` takes two optional timestamps, and mixing them up
gives a confidently wrong answer:

- `as_of` is <span class="vt">valid time</span>: when something was true
  on the machine. "What was installed on web-01 last Tuesday?"
- `knew_at` is <span class="rt">record time</span>: when the server
  believed it. "What did we think was installed on web-01, going on the
  reports we had by last Tuesday?"

They differ whenever an agent reports late or a later scan corrects an
earlier belief, which is exactly the case that matters after an incident.
Both take ISO-8601 timestamps and both require a timezone.

## Mutating tools

| Tool | Scope | Notes |
|---|---|---|
| `acknowledge_alert` | `alerts:write` | Requires `confirm: true` |
| `run_script` | `scripts:run` | Requires `confirm: true`; runs library scripts by name |
| `approve_patches` | `patches:write` | Approves only; it can never trigger installation |

Every mutating tool takes `confirm: bool = False` and defaults to a dry
run: called without confirmation it changes nothing and returns the plan
of exactly what it would do. Show the plan, get a yes, then confirm.

## Scopes and refusals

Keys are minted with `make api-key NAME=... SCOPES=...` from these
scopes: `devices:read`, `alerts:read`, `alerts:write`, `scripts:run`,
`patches:read`, `patches:write`.

A refusal means the key lacks a scope, not that the request was phrased
wrongly; retrying will not help, and assistants are told as much by the
server's error message.

## Audit

Every call, successful or refused, is written to the audit log under the
API key's name. Assume the operator will read it.
