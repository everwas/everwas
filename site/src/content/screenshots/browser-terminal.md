---
title: Browser terminal
alt: "Device detail for a Windows 11 machine. The Terminal tab is open with a live PowerShell session marked open and Session is recorded and audit-logged. The session shows hostname answering OPENRMM-WIN11 and a Get-Process table with Defender's MsMpEng at the top. Above the terminal, the CPU chart climbs to 100% during a deliberate load test."
image: ./browser-terminal.png
frameLabel: everwas · terminal
caption: "A real ConPTY on a real Windows 11 machine, opened from the dashboard with syntax highlighting courtesy of PowerShell itself. The badge above the prompt is not decoration: this session lands in the audit log as a replayable recording. The CPU chart is mid-spike because we were pinning the cores to make the alerting screenshot at the same time."
source: "Everwas web dashboard · /devices/:id · Terminal tab"
viewport: { width: 1600, height: 1000 }
capturedAt: 2026-08-29
---

## Recapture

1. Dev stack up with an online device. Windows shows off the ConPTY path;
   any online agent works.
2. Use the local-vite workaround from the time-machine recipe: the
   containerized dev server's HMR reconnect loop closes the PTY websocket
   mid-session.
3. Open the device, click the **terminal** tab, wait for the `open` badge.
4. Run a couple of read-only commands that produce a tidy screen:
   `hostname`, then
   `Get-Process | Sort-Object CPU -Descending | Select-Object -First 5 Name,Id,@{n='CPU(s)';e={[math]::Round($_.CPU,1)}}`
   Use `cls` first if you did any exploratory typing; every keystroke is in
   the recorded session either way, but the screenshot should read clean.
5. Full-viewport screenshot at **1600×1000**, PNG (`raw` on the MCP tool).
