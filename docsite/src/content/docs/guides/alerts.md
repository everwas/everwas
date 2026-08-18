---
title: Alerting
description: Threshold rules with duration windows and cooldowns, heartbeat-miss detection, and delivery that retries.
---

Alerting covers two kinds of bad news: a metric crossing a line, and a
machine going quiet.

## Threshold rules

A rule names a metric (CPU, memory, disk, and friends), a threshold, and
a **duration window**: the condition must hold for the whole window
before the alert fires. A CPU spike during a build should not page
anyone; thirty minutes above 95% probably should.

Rules also carry a **cooldown**, so a metric oscillating around its
threshold produces one alert, not one per crossing.

## Offline detection

Agents heartbeat every 30 seconds. A device that misses heartbeats for 90
seconds is marked offline, which can itself fire an alert. The threshold
is generous enough to ride out a NATS reconnect and short enough that a
genuinely dead machine is noticed within two minutes.

## Delivery

Firing alerts are delivered by email, webhook,
[ntfy](https://ntfy.sh/), or [gotify](https://gotify.net/), with retries
on delivery failure. In dev mode there is no SMTP setup to do: the stack
runs [Mailpit](https://mailpit.axllent.org/) and alert emails land in its
web UI at `mail-<your-domain>`.

## The alert lifecycle

An alert is **firing** until someone **acknowledges** it, and resolves
when the condition clears. Acknowledgement is the "a human has seen this"
marker, and like every other mutation it lands in the audit log with the
actor's name, whether that actor was a person in the **Alerts** view or
an assistant calling `acknowledge_alert` through the
[MCP server](/guides/enable-mcp/) (which requires `confirm: true` and an
`alerts:write` scoped key).
