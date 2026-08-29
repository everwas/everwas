---
title: Alerts firing
alt: "The Everwas alerts page with one active alert: a critical cpu-sustained-high rule firing on OPENRMM-WIN11 at 100.0%, with an Ack button. Around it, a notification delivery panel reading everything queued has been delivered, the rule definition (cpu above 80%), an ops-email channel, and a recently-resolved table of past alerts on the same machine."
image: ./alerts-firing.png
frameLabel: everwas · alerts
caption: "This alert is not staged data: we pinned the VM's cores at 100% from the browser terminal and the rule noticed within one ingest cycle. When the load stops, it moves itself to the recently-resolved table below, which is why that table already has history."
source: "Everwas web dashboard · /alerts"
viewport: { width: 1600, height: 1000 }
capturedAt: 2026-08-29
---

## Recapture

1. Dev stack up, `OPENRMM-WIN11` online, and the standing
   `cpu-sustained-high` rule enabled (critical, cpu > 80%, 0s duration,
   ops-email channel). If it is gone, recreate it via **New rule**.
2. Make the alert real instead of faking it: open the device's browser
   terminal and pin every core for ~90 seconds:
   `1..([Environment]::ProcessorCount) | ForEach-Object { Start-Job { $end=(Get-Date).AddSeconds(90); while((Get-Date) -lt $end){} } } | Out-Null`
   The jobs stop themselves; nothing to clean up on the VM.
3. Go to `/alerts` and wait for **Active (1)** with the 100.0% value
   (one telemetry ingest cycle, typically under a minute).
4. Full-viewport screenshot at **1600×1000**, PNG (`raw` on the MCP tool),
   before the load drops and the alert self-resolves.
5. Let it resolve on its own; do not delete the alert row. The
   recently-resolved history is part of what the shot is selling.
