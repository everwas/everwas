---
title: Schedule recurring jobs
description: Cron schedules that fire on the agent's clock, spread themselves across the fleet, and self-heal after downtime.
---

Schedules attach a cron expression to a script and a set of target
devices. The part that distinguishes them from a server-side cron is
*where* they fire: on the agent, from a locally cached copy of its
schedule, on its own clock.

## Why the agent's clock

A server-side scheduler dispatching jobs at fire time has an obvious
failure mode: the machine is offline at that moment, and either the run
silently does not happen or a thundering herd fires on reconnect. Instead,
every agent holds its own schedule and fires locally, offline or not. A
nightly cleanup runs at 02:00 *device local time* whether or not the
device can reach the server, and the results flow up when connectivity
returns.

## Jitter that stays put

A fleet-wide schedule with a jitter window spreads devices across that
window deterministically: each device's offset is derived from its id and
the schedule entry's id, not rolled fresh each night. The fleet spreads
itself the same way every time, and a given machine is predictable, which
matters when you are staring at one box's logs at 02:07 wondering whether
its backup should have started yet.

## Self-healing sync

The server never assumes a schedule push arrived. Every heartbeat carries
the version of the schedule the agent currently holds; the server
compares it with what that device *should* hold and re-syncs on any
mismatch. A device that was offline through three schedule edits fixes
itself within one heartbeat of coming back, about 30 seconds.

An agent can also refuse entries it cannot honor, a cron expression it
cannot parse or a timezone it does not know. Rejected entries are
reported back rather than silently dropped, so the server does not go on
believing they will fire.

## Missed fire times

If a device is powered off across a fire time, the run is skipped and the
agent records a misfire audit event (`sched.misfire_skipped`), visible in
the device's audit trail. Skipping is deliberate: replaying every missed
nightly job on Monday morning boot is rarely what anyone wants.

## Results

A scheduled run produces the same output, result, and audit records as a
manual run, tagged with the schedule entry that produced it, so the run
history of a schedule is queryable per device.
