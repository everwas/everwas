---
title: Run scripts across the fleet
description: Keep a script library, run against one device or hundreds, and trust delivery to machines that are currently offline.
---

Scripts live in a library on the server and run as jobs on agents. The
**Scripts** view holds the library; a run targets one device, a set of
devices, or the whole fleet.

## Delivery is durable

A job is not a network call that fails if the machine is asleep. Jobs are
written to a JetStream work stream and each agent pulls from its own
consumer, so a laptop that is off when you start a fleet-wide run picks
the job up when it reconnects. Jobs wait up to seven days for an agent to
collect them.

This is worth internalizing because it changes how you use the feature:
"run this on everything" means everything, eventually, not "everything
that happened to be online at 2pm".

## What a run looks like

Execution is two-phase. The agent immediately acknowledges that it
accepted the job, then streams progress and output separately:

- **Progress** events carry a percentage and a phase note for long jobs.
- **Output** arrives in chunks, stdout and stderr kept apart, capped at
  8 MiB per job. A job that writes more is truncated and flagged as such
  rather than silently trimmed.
- The **result** is terminal: status, exit code, and duration. It is the
  authoritative record of what happened.

Every run also writes a `script.executed` audit event naming who asked
for it, so the Audit view can answer "who ran what, where" later.

## Cancelling

A running job can be cancelled from the device's detail view; the server
sends a cancel command to the agent, which kills the process and reports
a final result.

## From an assistant

With the [MCP server enabled](/guides/enable-mcp/), an assistant holding a
key with the `scripts:run` scope can run library scripts by name. The
call requires `confirm: true`; without it the tool returns a preview of
what would run and where, and nothing executes. See the
[MCP tool reference](/reference/mcp-tools/).

## Running on a schedule

For recurring work, attach a cron schedule instead of firing runs by
hand: [schedules](/guides/schedules/) fire on the agent's own clock and
survive the agent being offline at the appointed minute.
