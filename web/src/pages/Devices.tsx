import { useEffect, useState } from "react"
import { useNavigate } from "react-router-dom"
import { Apple, AppWindow, Terminal } from "lucide-react"

import { api, type Device } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"

const REFRESH_MS = 10_000

const osIcon = {
  windows: AppWindow,
  macos: Apple,
  linux: Terminal,
} as const

function StatusPill({ status }: { status: Device["status"] }) {
  const styles: Record<Device["status"], string> = {
    active: "bg-emerald-500",
    offline: "bg-red-500",
    enrolled: "bg-amber-500",
    retired: "bg-muted-foreground",
  }
  return (
    <Badge variant="outline" className="gap-1.5 capitalize">
      <span className={`size-2 rounded-full ${styles[status]}`} />
      {status === "active" ? "online" : status}
    </Badge>
  )
}

function lastSeen(iso: string | null): string {
  if (!iso) return "never"
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return `${Math.floor(s)}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export function DevicesPage() {
  const navigate = useNavigate()
  const [devices, setDevices] = useState<Device[] | null>(null)

  useEffect(() => {
    let cancelled = false
    const load = () => api.devices().then((d) => !cancelled && setDevices(d)).catch(() => {})
    load()
    const timer = setInterval(load, REFRESH_MS)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [])

  if (devices === null) {
    return <p className="text-sm text-muted-foreground">Loading devices…</p>
  }
  if (devices.length === 0) {
    return (
      <div className="rounded-lg border border-dashed p-10 text-center text-sm text-muted-foreground">
        No devices yet. Generate an enrollment token and install an agent:
        <pre className="mt-3 rounded bg-muted p-3 text-left font-mono text-xs">
          make enroll-token{"\n"}openrmm-agent enroll --server https://… --token ore_…
        </pre>
      </div>
    )
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Hostname</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>OS</TableHead>
          <TableHead>Arch</TableHead>
          <TableHead>Agent</TableHead>
          <TableHead>Last seen</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {devices.map((d) => {
          const Os = osIcon[d.os_family]
          return (
            <TableRow
              key={d.id}
              className="cursor-pointer"
              onClick={() => navigate(`/devices/${d.id}`)}
            >
              <TableCell className="font-medium">{d.hostname}</TableCell>
              <TableCell>
                <StatusPill status={d.status} />
              </TableCell>
              <TableCell>
                <span className="flex items-center gap-1.5">
                  <Os className="size-4 text-muted-foreground" />
                  {d.os_version || d.os_family}
                </span>
              </TableCell>
              <TableCell className="text-muted-foreground">{d.arch || "-"}</TableCell>
              <TableCell className="text-muted-foreground">{d.agent_version || "-"}</TableCell>
              <TableCell className="text-muted-foreground">{lastSeen(d.last_heartbeat_at)}</TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
