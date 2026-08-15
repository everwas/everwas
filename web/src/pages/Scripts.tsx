import { useCallback, useEffect, useState } from "react"
import { Play, Plus, Trash2 } from "lucide-react"

import { api, type Device, type Script, type ScriptRun, type ShellKind } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"

const SHELLS: ShellKind[] = ["bash", "sh", "zsh", "powershell", "pwsh", "cmd", "python"]
const REFRESH_MS = 4000

export function RunStatusBadge({ status }: { status: ScriptRun["status"] }) {
  const tone: Record<ScriptRun["status"], string> = {
    succeeded: "bg-emerald-500",
    failed: "bg-red-500",
    timeout: "bg-red-500",
    cancelled: "bg-muted-foreground",
    running: "bg-blue-500",
    delivered: "bg-amber-500",
    queued: "bg-amber-500",
  }
  return (
    <Badge variant="outline" className="gap-1.5">
      <span className={`size-2 rounded-full ${tone[status]}`} />
      {status}
    </Badge>
  )
}

function NewScriptForm({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [shell, setShell] = useState<ShellKind>("bash")
  const [body, setBody] = useState("")
  const [busy, setBusy] = useState(false)

  if (!open) {
    return (
      <Button size="sm" className="gap-1.5" onClick={() => setOpen(true)}>
        <Plus className="size-4" />
        New script
      </Button>
    )
  }

  return (
    <Card className="w-full">
      <CardHeader className="pb-3">
        <CardTitle className="text-base">New script</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <div className="flex flex-wrap gap-3">
          <div className="flex flex-1 flex-col gap-1.5">
            <Label htmlFor="script-name">Name</Label>
            <Input
              id="script-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="disk-usage-report"
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="script-shell">Shell</Label>
            <select
              id="script-shell"
              value={shell}
              onChange={(e) => setShell(e.target.value as ShellKind)}
              className="h-9 rounded-md border bg-transparent px-3 text-sm"
            >
              {SHELLS.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </div>
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="script-body">Body</Label>
          <textarea
            id="script-body"
            value={body}
            onChange={(e) => setBody(e.target.value)}
            rows={8}
            spellCheck={false}
            className="rounded-md border bg-transparent p-3 font-mono text-xs"
            placeholder={"#!/usr/bin/env bash\ndf -h"}
          />
        </div>
        <div className="flex gap-2">
          <Button
            size="sm"
            disabled={busy || !name || !body}
            onClick={async () => {
              setBusy(true)
              try {
                await api.createScript({ name, shell, body, timeout_s: 300, os_filter: [] })
                setName("")
                setBody("")
                setOpen(false)
                onCreated()
              } finally {
                setBusy(false)
              }
            }}
          >
            Save
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setOpen(false)}>
            Cancel
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

export function ScriptsPage() {
  const [scripts, setScripts] = useState<Script[]>([])
  const [devices, setDevices] = useState<Device[]>([])
  const [runs, setRuns] = useState<ScriptRun[]>([])
  const [selected, setSelected] = useState<string>("")
  const [message, setMessage] = useState<string | null>(null)

  const load = useCallback(() => {
    api.scripts().then(setScripts).catch(() => {})
    api.runs().then(setRuns).catch(() => {})
  }, [])

  useEffect(() => {
    load()
    api.devices().then(setDevices).catch(() => {})
    const timer = setInterval(() => api.runs().then(setRuns).catch(() => {}), REFRESH_MS)
    return () => clearInterval(timer)
  }, [load])

  const deviceName = (id: string) =>
    devices.find((d) => d.id === id)?.hostname ?? id.slice(0, 8)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <h1 className="text-xl font-semibold">Scripts</h1>
        <NewScriptForm onCreated={load} />
      </div>

      {message && <p className="text-sm text-muted-foreground">{message}</p>}

      <div className="flex flex-col gap-3">
        {scripts.length === 0 ? (
          <div className="rounded-lg border border-dashed p-10 text-center text-sm text-muted-foreground">
            No scripts yet. Create one to run it across your fleet.
          </div>
        ) : (
          scripts.map((s) => (
            <Card key={s.id}>
              <CardContent className="flex flex-wrap items-center gap-3 py-4">
                <div className="min-w-48 flex-1">
                  <p className="font-medium">{s.name}</p>
                  <p className="text-xs text-muted-foreground">
                    {s.shell} · timeout {s.timeout_s}s · v{s.version}
                  </p>
                </div>
                <select
                  value={selected}
                  onChange={(e) => setSelected(e.target.value)}
                  className="h-9 max-w-56 rounded-md border bg-transparent px-2 text-sm"
                >
                  <option value="">Select target…</option>
                  {devices.map((d) => (
                    <option key={d.id} value={d.id}>
                      {d.hostname}
                    </option>
                  ))}
                </select>
                <Button
                  size="sm"
                  className="gap-1.5"
                  disabled={!selected}
                  onClick={async () => {
                    const res = await api.runScript(s.id, { device_ids: [selected] })
                    setMessage(`Queued ${res.queued} run(s) for ${s.name}`)
                    api.runs().then(setRuns)
                  }}
                >
                  <Play className="size-4" />
                  Run
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={async () => {
                    await api.deleteScript(s.id)
                    load()
                  }}
                >
                  <Trash2 className="size-4" />
                </Button>
              </CardContent>
            </Card>
          ))
        )}
      </div>

      <div>
        <h2 className="mb-2 text-sm font-medium text-muted-foreground">Recent runs</h2>
        {runs.length === 0 ? (
          <p className="text-sm text-muted-foreground">No runs yet.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Device</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Exit</TableHead>
                <TableHead>Output</TableHead>
                <TableHead>Queued</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.map((r) => (
                <TableRow key={r.id}>
                  <TableCell>{deviceName(r.device_id)}</TableCell>
                  <TableCell>
                    <RunStatusBadge status={r.status} />
                  </TableCell>
                  <TableCell className="text-muted-foreground">{r.exit_code ?? "-"}</TableCell>
                  <TableCell className="max-w-md truncate font-mono text-xs">
                    {(r.stdout || r.stderr || "").slice(0, 160) || "-"}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {new Date(r.queued_at).toLocaleTimeString()}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>
    </div>
  )
}
