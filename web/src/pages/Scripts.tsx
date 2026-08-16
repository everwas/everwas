import { useCallback, useEffect, useState } from "react"
import { CalendarClock, Play, Plus, Trash2, TriangleAlert } from "lucide-react"

import {
  api,
  type Device,
  type Schedule,
  type SchedulePreview,
  type Script,
  type ScriptRun,
  type ShellKind,
} from "@/lib/api"
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

/** Cron schedules.
 *
 * These do not run from the server. They are pushed to the agents they target,
 * which fire them from a local cache on their own clock, so a nightly job runs
 * on a laptop that is off the network at 02:00 and reports when it is back.
 * The preview exists because a cron expression and a target selector are two
 * things nobody can verify by reading them: getting either wrong means a job
 * that silently runs nowhere, or on everything.
 */
function Schedules({ scripts }: { scripts: Script[] }) {
  const [schedules, setSchedules] = useState<Schedule[]>([])
  const [preview, setPreview] = useState<Record<string, SchedulePreview>>({})
  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [scriptId, setScriptId] = useState("")
  const [cron, setCron] = useState("0 2 * * *")
  const [tz, setTz] = useState(Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC")
  const [jitterS, setJitterS] = useState(300)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(() => {
    api
      .schedules()
      .then((rows) => {
        setSchedules(rows)
        rows.forEach((r) =>
          api
            .previewSchedule(r.id)
            .then((p) => setPreview((prev) => ({ ...prev, [r.id]: p })))
            .catch(() => {}),
        )
      })
      .catch(() => {})
  }, [])

  useEffect(load, [load])

  const scriptName = (id: string) => scripts.find((s) => s.id === id)?.name ?? id.slice(0, 8)

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h2 className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <CalendarClock className="size-4" />
          Schedules ({schedules.length})
        </h2>
        <Button size="sm" variant="outline" className="gap-1.5" onClick={() => setOpen(!open)}>
          <Plus className="size-4" />
          New schedule
        </Button>
      </div>

      {open && (
        <Card>
          <CardContent className="flex flex-col gap-3 py-4">
            <div className="flex flex-wrap gap-3">
              <div className="flex flex-1 flex-col gap-1.5">
                <Label htmlFor="sch-name">Name</Label>
                <Input id="sch-name" value={name} onChange={(e) => setName(e.target.value)} />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="sch-script">Script</Label>
                <select
                  id="sch-script"
                  value={scriptId}
                  onChange={(e) => setScriptId(e.target.value)}
                  className="h-9 rounded-md border bg-transparent px-3 text-sm"
                >
                  <option value="">choose…</option>
                  {scripts.map((sc) => (
                    <option key={sc.id} value={sc.id}>
                      {sc.name}
                    </option>
                  ))}
                </select>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="sch-cron">Cron</Label>
                <Input
                  id="sch-cron"
                  value={cron}
                  onChange={(e) => setCron(e.target.value)}
                  className="w-36 font-mono"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="sch-tz">Timezone</Label>
                <Input id="sch-tz" value={tz} onChange={(e) => setTz(e.target.value)} className="w-48" />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="sch-jitter">Jitter (sec)</Label>
                <Input
                  id="sch-jitter"
                  type="number"
                  value={jitterS}
                  onChange={(e) => setJitterS(Number(e.target.value))}
                  className="w-28"
                />
              </div>
            </div>
            <p className="text-xs text-muted-foreground">
              Every targeted agent spreads its start over the jitter window, the same way each
              night, so a fleet does not stampede at 02:00.
            </p>
            <div className="flex items-center gap-2">
              <Button
                size="sm"
                disabled={!name || !scriptId}
                onClick={async () => {
                  setError(null)
                  try {
                    await api.createSchedule({
                      name,
                      script_id: scriptId,
                      cron,
                      tz,
                      target: { all: true },
                      jitter_s: jitterS,
                      misfire_grace_s: 3600,
                      enabled: true,
                    })
                  } catch (e) {
                    setError(e instanceof Error ? e.message : "could not save the schedule")
                    return
                  }
                  setName("")
                  setOpen(false)
                  load()
                }}
              >
                Save
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setOpen(false)}>
                Cancel
              </Button>
            </div>
            {error && (
              <p className="flex items-start gap-2 text-sm text-destructive">
                <TriangleAlert className="mt-0.5 size-4 shrink-0" />
                {error}
              </p>
            )}
          </CardContent>
        </Card>
      )}

      {schedules.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No schedules. A schedule runs its script on every device it targets, from the agent, so
          it still fires while the machine is off the network.
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {schedules.map((sc) => {
            const p = preview[sc.id]
            return (
              <Card key={sc.id}>
                <CardContent className="flex items-center gap-3 py-3">
                  <div className="flex-1">
                    <p className="flex items-center gap-2 text-sm font-medium">
                      {sc.name}
                      {!sc.enabled && (
                        <Badge variant="outline" className="text-muted-foreground">
                          disabled
                        </Badge>
                      )}
                      {p?.matches === 0 && (
                        /* A cron that is correct and targets nothing looks
                           identical to one that works. */
                        <Badge
                          variant="outline"
                          className="gap-1 border-amber-500/50 text-amber-700 dark:text-amber-400"
                        >
                          <TriangleAlert className="size-3" />
                          matches no devices
                        </Badge>
                      )}
                    </p>
                    <p className="text-xs text-muted-foreground">
                      <span className="font-mono">{sc.cron}</span> {sc.tz} · {scriptName(sc.script_id)}
                      {p ? ` · ${p.matches} device(s)` : ""}
                      {sc.jitter_s ? ` · ±${sc.jitter_s}s jitter` : ""}
                    </p>
                    {p?.next_fires?.[0] && (
                      <p className="text-xs text-muted-foreground">
                        next {new Date(p.next_fires[0]).toLocaleString()}
                      </p>
                    )}
                    <p className="text-xs text-muted-foreground">
                      {sc.last_run_at
                        ? `last ran ${new Date(sc.last_run_at).toLocaleString()}`
                        : "has not run yet"}
                    </p>
                  </div>
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={async () => {
                      await api.deleteSchedule(sc.id)
                      load()
                    }}
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </CardContent>
              </Card>
            )
          })}
        </div>
      )}
    </section>
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

      {scripts.length > 0 && <Schedules scripts={scripts} />}

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
