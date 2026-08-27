---
title: Dashboard overview
alt: "The Everwas dashboard overview: four stat cards reading two devices online, zero offline, zero firing alerts, zero undelivered notifications; a fleet section with a Windows and a Debian machine showing live CPU, memory, and disk bars; and an activity feed of audit events including agent renewals and MCP calls."
image: ./dashboard-overview.png
frameLabel: everwas · overview
caption: "The overview answers the on-call question in one glance: what is the fleet doing, what is on fire, and who did what last. The activity feed is the audit log itself, so the answer to the third question is always evidence, never memory."
source: Everwas web dashboard · /overview
viewport: { width: 1600, height: 1000 }
capturedAt: 2026-08-27
---

## Recapture

1. Dev stack up with real agents reporting:
   `docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d`
   from the repo root. The shot wants at least two online devices of
   different OS families with telemetry flowing (the dev fleet's
   `OPENRMM-WIN11` VM and a Linux VM/host agent do fine).
2. A throwaway admin for the login:
   `docker compose exec everwas-api everwas create-admin screenshot@example.com --password <pw>`
3. Browser (playwright or human) at viewport **1600×1000**:
   sign in at `http://127.0.0.1:25173/login`, then navigate to
   `http://127.0.0.1:25173/overview`.
4. Wait one refresh cycle (~10s) so the fleet cards carry utilization
   bars and the stat cards are non-zero. The zeros render first;
   patience is part of the recipe.
5. Full-viewport screenshot, no crop: the sidebar is part of the product.

The healthy-fleet state is the point: quiet stat cards, green bars,
"Nothing firing. Quiet is good." Do not stage an alert for this shot;
the alerting story has its own screenshot when it needs one.
