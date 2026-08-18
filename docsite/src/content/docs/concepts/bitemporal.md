---
title: Bitemporal history
description: Why every inventory fact carries two time axes, and what that makes possible.
---

When a system records observations about an external reality, there are
two different time questions about every fact, not one:

1. <span class="vt">valid time</span>: when was this true *on the
   machine*?
2. <span class="rt">record time</span>: when did the server *believe*
   it?

Most monitoring tools store one timestamp and conflate the two. It works
until the first time an agent reports late, and then it quietly lies to
you.

## The laptop in the bag

A laptop goes into a bag on Friday with a vulnerable OpenSSL on it. On
Monday it comes back online and its inventory report lands. In a
single-timestamp system that report simply *is* the state, stamped
Monday. Two lies follow:

- Ask "what was on this machine Saturday?" and the system either has no
  answer or shows you the pre-Friday state as if nothing happened since.
- Worse, ask "did our monitoring know about the vulnerable package on
  Saturday?" and the system cannot even represent the question. It has
  overwritten what it used to believe.

With two axes there is no ambiguity. The fact "OpenSSL 3.0.x installed"
has a <span class="vt">valid window</span> covering the whole time it was
true on the disk, and a <span class="rt">record window</span> that only
opens on Monday when the server learned it. "What was true Saturday?"
and "what did we know Saturday?" are different queries with different,
correct answers.

## Never update, always amend

The store has one mutation pattern: a correction closes the old belief's
record window and inserts the successor in the same transaction. Nothing
is ever updated in place, so past beliefs stay queryable forever,
including the ones that turned out wrong. That is what makes the answer
to "what did the dashboard say at 09:00 that morning?" trustworthy: it
is not a reconstruction, it is the actual superseded row.

In PostgreSQL terms, each fact carries `valid_during` and
`recorded_during` as `tstzrange` columns, with a partial unique index
guaranteeing at most one current belief per fact. This is ordinary
PostgreSQL, no extensions; the partitioning decision for the neighboring
telemetry tables is separate and covered in
[ADR-0002](/decisions/0002-postgres-partitioning/).

## Where you meet it

- The device detail view's time control, and the amber/cyan color
  pairing across the UI: amber always marks
  <span class="vt">valid time</span>, cyan always marks
  <span class="rt">record time</span>.
- The `as_of` / `knew_at` parameters on
  [`get_device_facts`](/reference/mcp-tools/), and
  `diff_device_facts` for what changed between two moments.
- The [device history guide](/guides/device-history/), which works
  through the questions this design exists to answer.
