import { useEffect, useRef, useState } from "react"
import { FitAddon } from "@xterm/addon-fit"
import { Terminal as XTerm } from "@xterm/xterm"
import "@xterm/xterm/css/xterm.css"

import { Badge } from "@/components/ui/badge"

type Status = "connecting" | "open" | "closed" | "error"

// Matches the neutral shadcn surfaces; the terminal is always dark.
const THEME = {
  background: "#0d0d0d",
  foreground: "#e6e6e6",
  cursor: "#e6e6e6",
  selectionBackground: "#33333a",
}

export function DeviceTerminal({ deviceId }: { deviceId: string }) {
  const hostRef = useRef<HTMLDivElement>(null)
  const [status, setStatus] = useState<Status>("connecting")

  useEffect(() => {
    if (!hostRef.current) return

    const term = new XTerm({
      theme: THEME,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
      fontSize: 13,
      cursorBlink: true,
      scrollback: 5000,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(hostRef.current)
    fit.fit()

    const proto = location.protocol === "https:" ? "wss:" : "ws:"
    const url = `${proto}//${location.host}/api/v1/devices/${deviceId}/shell?cols=${term.cols}&rows=${term.rows}`
    const ws = new WebSocket(url)
    ws.binaryType = "arraybuffer"

    ws.onopen = () => setStatus("open")
    ws.onmessage = (event) => {
      if (event.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(event.data))
      } else {
        term.write(String(event.data))
      }
    }
    ws.onerror = () => setStatus("error")
    ws.onclose = () => setStatus("closed")

    const encoder = new TextEncoder()
    const dataSub = term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
    })

    const onResize = () => {
      fit.fit()
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }))
      }
    }
    window.addEventListener("resize", onResize)

    return () => {
      window.removeEventListener("resize", onResize)
      dataSub.dispose()
      ws.close()
      term.dispose()
    }
  }, [deviceId])

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <Badge variant="outline" className="gap-1.5">
          <span
            className={`size-2 rounded-full ${
              status === "open"
                ? "bg-emerald-500"
                : status === "connecting"
                  ? "bg-amber-500"
                  : "bg-red-500"
            }`}
          />
          {status}
        </Badge>
        <span className="text-xs text-muted-foreground">
          Session is recorded and audit-logged.
        </span>
      </div>
      <div
        ref={hostRef}
        className="h-[28rem] overflow-hidden rounded-lg border bg-[#0d0d0d] p-2"
      />
    </div>
  )
}
