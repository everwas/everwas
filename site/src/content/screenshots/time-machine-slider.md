---
title: Time machine slider
alt: "Device detail for a Windows 11 machine with live CPU, memory, and network charts. The Software tab has the time machine engaged: a slider spanning seven days sits three days back, a badge reads as of 8/26/2026, and the package table shows the Microsoft Edge build that was true on that date, older than the one installed since."
image: ./time-machine-slider.png
frameLabel: everwas · time machine
caption: "Same table, different day. The slider re-asks the bitemporal store for whatever moment you point at, so the software list shows the Edge build that was true that Wednesday, and the update that landed the next morning is simply not there yet."
source: "Everwas web dashboard · /devices/:id · Software tab"
viewport: { width: 1600, height: 1000 }
capturedAt: 2026-08-29
---

## Recapture

1. Dev stack up (`docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d`)
   with a device that has at least a few days of fact history containing a
   known change. The dev fleet's `OPENRMM-WIN11` VM works: its Edge updates
   often enough that a three-day rewind changes the version column.
2. **Capture against a locally run vite, not the containerized one.** The
   compose dev server advertises its HMR socket at the public host, so a
   browser on localhost reconnect-loops and hard-reloads the page, resetting
   the time-machine state between your slider drag and the screenshot:
   `cd web && EVERWAS_API_URL=http://127.0.0.1:28000 npx vite --port 25199`
3. Sign in (throwaway admin from the dashboard-overview recipe), open the
   device, stay on the **Software** tab, click **Time machine**.
4. Drag the slider back past the known change (72h back = slider value 96 of
   168). The badge must read "as of ..." and the table must differ visibly
   from the live view; if it doesn't, pick a bigger rewind.
5. Full-viewport screenshot at **1600×1000**, PNG (pass `raw` to the
   playwright MCP screenshot tool; the default is a lossy JPEG).
