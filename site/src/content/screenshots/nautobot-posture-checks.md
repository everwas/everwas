---
title: Nautobot posture checks table
alt: "Nautobot table of RMM posture checks synced from Everwas. Each row pairs an endpoint with one check and its verdict: Pass, Fail, Not applicable, or Not assessed, plus a detail column explaining why."
image: ./nautobot-posture-checks.png
frameLabel: nautobot · rmm posture checks
caption: "Verdicts keep their semantics in transit: only an explicit fail is ever a failure. A check that ran and found nothing to assess says exactly that, instead of quietly counting against the machine."
source: Nautobot plugin · RMM Posture Checks list view
viewport: { width: 1596, height: 680 }
capturedAt: 2026-08-20
---

## Recapture

1. Dev Nautobot with `nautobot-ssot-everwas` installed, after at least one
   sync run so posture rows exist (`Plugins → Single Source of Truth →
   Everwas → Sync Now`).
2. Navigate to `Plugins → RMM Posture Checks`.
3. Viewport ~1600px wide; crop to the table card including the page header
   row, excluding the Nautobot chrome/sidebar.
4. The shot should include at least one row of each verdict: Pass, Fail,
   Not applicable, Not assessed, so the caption's claim is visible.

Captured originally by the Nautobot-sync session, 2026-08-20; recipe
reconstructed from the shot itself. Refine on first recapture.
