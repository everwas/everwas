---
title: Query device history
description: Ask what was true on a machine at any moment, and separately what the server believed at any moment.
---

Every inventory fact Everwas stores (hardware, installed software,
processes, services, patch state) is recorded on two time axes:

- <span class="vt">valid time</span>: when it was true *on the machine*
- <span class="rt">record time</span>: when the server *learned it*

Most tools store one timestamp and quietly conflate the two. Everwas
keeps both, which turns device history from a log into something you can
interrogate. The theory lives in the
[bitemporal concept page](/concepts/bitemporal/); this guide is about
asking the questions.

## The two questions

Take "what was installed on web-01 last Tuesday?" There are two honest
readings:

1. **What was actually on the box last Tuesday?** That is a
   <span class="vt">valid time</span> question. Use `as_of`.
2. **What did we believe was on the box, going on the reports we had by
   last Tuesday?** That is a <span class="rt">record time</span>
   question. Use `knew_at`.

The answers differ whenever an agent reported late (the laptop was in a
bag all week) or a later scan corrected an earlier belief. After an
incident, the difference is the whole point: `knew_at` tells you whether
your monitoring knew about the vulnerable package at the time, or only
found out afterwards.

Both parameters take ISO-8601 timestamps and both require an explicit
timezone, because "Tuesday" in a fleet spanning time zones is not a
moment.

## In the app

A device's detail view carries its inventory with a time control: pick a
moment and see the device as it was, or as it was believed to be. The
amber and cyan coloring throughout the UI tracks the two axes, amber for
valid time, cyan for record time.

## Diffing two moments

"What changed on this machine between Friday and Monday?" is
`diff_device_facts`: two moments in, a structured diff out, covering
installed software, services, and the rest of inventory. This is
routinely the fastest way to answer "what changed before it broke".

## From an assistant

The MCP tools `get_device_facts` and `diff_device_facts` expose both
axes with a `devices:read` scoped key, so an assistant can do the
incident archaeology for you:

> Which machines had an outdated OpenSSL last Monday, and did we know
> that at the time?

That question needs both axes in one query, and it is exactly what the
tools were built for. See the [MCP tool reference](/reference/mcp-tools/).

## What makes this trustworthy

History is never updated in place. A correction closes the old belief's
record-time window and opens a new one in the same transaction, so "what
did the server believe at 09:00 that morning?" stays answerable forever,
including the beliefs that turned out to be wrong. The mechanics are on
the [bitemporal concept page](/concepts/bitemporal/).
