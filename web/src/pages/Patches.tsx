import { useCallback, useEffect, useState } from "react"
import { CheckCircle2, Download, RefreshCw, ShieldAlert } from "lucide-react"

import {
  api,
  type Device,
  type DevicePatch,
  type PatchJob,
  type PatchSeverity,
} from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"

const REFRESH_MS = 6000

// status palette: label always accompanies the dot, never color alone
const SEVERITY_DOT: Record<PatchSeverity, string> = {
  critical: "bg-[#d03b3b]",
  important: "bg-[#ec835a]",
  moderate: "bg-[#fab219]",
  low: "bg-[#2a78d6]",
  unknown: "bg-muted-foreground",
}

const SEVERITY_ORDER: PatchSeverity[] = ["critical", "important", "moderate", "low", "unknown"]

function SeverityBadge({ severity }: { severity: PatchSeverity }) {
  return (
    <Badge variant="outline" className="gap-1.5">
      <span className={`size-2 rounded-full ${SEVERITY_DOT[severity]}`} />
      {severity}
    </Badge>
  )
}

function JobStatusBadge({ status }: { status: PatchJob["status"] }) {
  const tone: Record<PatchJob["status"], string> = {
    succeeded: "bg-emerald-500",
    partial: "bg-amber-500",
    failed: "bg-red-500",
    cancelled: "bg-muted-foreground",
    running: "bg-blue-500",
    queued: "bg-amber-500",
  }
  return (
    <Badge variant="outline" className="gap-1.5">
      <span className={`size-2 rounded-full ${tone[status]}`} />
      {status}
    </Badge>
  )
}

function bytes(n: number | null): string {
  if (!n) return "-"
  if (n < 2 ** 20) return `${(n / 2 ** 10).toFixed(0)} KiB`
  return `${(n / 2 ** 20).toFixed(1)} MiB`
}

export function PatchesPage() {
  const [devices, setDevices] = useState<Device[]>([])
  const [selected, setSelected] = useState<string>("")
  const [patches, setPatches] = useState<DevicePatch[]>([])
  const [jobs, setJobs] = useState<PatchJob[]>([])
  const [checked, setChecked] = useState<Set<string>>(new Set())
  const [notice, setNotice] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    api.devices().then((d) => {
      setDevices(d)
      if (d.length && !selected) setSelected(d[0].id)
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const load = useCallback(() => {
    if (!selected) return
    api.devicePatches(selected).then(setPatches).catch(() => {})
    api.patchJobs(selected).then(setJobs).catch(() => {})
  }, [selected])

  useEffect(() => {
    setChecked(new Set())
    load()
    const timer = setInterval(load, REFRESH_MS)
    return () => clearInterval(timer)
  }, [load])

  const sorted = [...patches].sort(
    (a, b) => SEVERITY_ORDER.indexOf(a.severity) - SEVERITY_ORDER.indexOf(b.severity),
  )
  const approvedCount = patches.filter((p) => p.approved).length
  const installable = patches.filter((p) => p.approved && !p.unsupported)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">Patches</h1>
        <div className="flex flex-wrap items-center gap-2">
          <select
            value={selected}
            onChange={(e) => setSelected(e.target.value)}
            className="h-9 max-w-64 rounded-md border bg-transparent px-2 text-sm"
          >
            {devices.map((d) => (
              <option key={d.id} value={d.id}>
                {d.hostname}
              </option>
            ))}
          </select>
          <Button
            size="sm"
            variant="outline"
            className="gap-1.5"
            disabled={!selected || busy}
            onClick={async () => {
              setBusy(true)
              try {
                await api.scanPatches(selected)
                setNotice("Scan requested. Results appear when the agent reports back.")
              } finally {
                setBusy(false)
              }
            }}
          >
            <RefreshCw className="size-4" />
            Scan now
          </Button>
          <Button
            size="sm"
            className="gap-1.5"
            disabled={!selected || installable.length === 0 || busy}
            onClick={async () => {
              setBusy(true)
              try {
                const job = await api.deployPatches(selected)
                setNotice(`Deploying ${job.external_ids.length} approved patch(es).`)
                load()
              } catch {
                setNotice("Nothing approved to deploy.")
              } finally {
                setBusy(false)
              }
            }}
          >
            <Download className="size-4" />
            Deploy approved ({installable.length})
          </Button>
        </div>
      </div>

      {notice && <p className="text-sm text-muted-foreground">{notice}</p>}

      <div className="flex flex-wrap gap-4">
        <Card className="min-w-40 flex-1">
          <CardContent className="flex items-center gap-3 py-4">
            <ShieldAlert className="size-5 text-muted-foreground" />
            <div>
              <p className="text-xs text-muted-foreground">Pending</p>
              <p className="text-xl font-semibold">{patches.length}</p>
            </div>
          </CardContent>
        </Card>
        <Card className="min-w-40 flex-1">
          <CardContent className="flex items-center gap-3 py-4">
            <CheckCircle2 className="size-5 text-muted-foreground" />
            <div>
              <p className="text-xs text-muted-foreground">Approved</p>
              <p className="text-xl font-semibold">{approvedCount}</p>
            </div>
          </CardContent>
        </Card>
      </div>

      {checked.size > 0 && (
        <div className="flex items-center gap-2 rounded-md border bg-muted/40 px-3 py-2">
          <span className="text-sm">{checked.size} selected</span>
          <Button
            size="sm"
            onClick={async () => {
              await api.approvePatches([...checked], selected)
              setChecked(new Set())
              load()
            }}
          >
            Approve
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={async () => {
              await api.approvePatches([...checked], selected, "declined")
              setChecked(new Set())
              load()
            }}
          >
            Decline
          </Button>
        </div>
      )}

      {patches.length === 0 ? (
        <div className="rounded-lg border border-dashed p-10 text-center text-sm text-muted-foreground">
          No pending patches reported for this device. Use Scan now to ask the agent to check.
        </div>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-8" />
              <TableHead>Severity</TableHead>
              <TableHead>Update</TableHead>
              <TableHead>Size</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sorted.map((p) => (
              <TableRow key={p.external_id}>
                <TableCell>
                  <input
                    type="checkbox"
                    checked={checked.has(p.id)}
                    disabled={p.unsupported}
                    onChange={(e) =>
                      setChecked((prev) => {
                        const next = new Set(prev)
                        if (e.target.checked) next.add(p.id)
                        else next.delete(p.id)
                        return next
                      })
                    }
                  />
                </TableCell>
                <TableCell>
                  <SeverityBadge severity={p.severity} />
                </TableCell>
                <TableCell>
                  <p className="text-sm">{p.title}</p>
                  <p className="font-mono text-xs text-muted-foreground">{p.external_id}</p>
                  {p.detail && <p className="text-xs text-amber-600">{p.detail}</p>}
                </TableCell>
                <TableCell className="text-muted-foreground">{bytes(p.size_bytes)}</TableCell>
                <TableCell>
                  {p.unsupported ? (
                    <Badge variant="secondary">needs MDM</Badge>
                  ) : p.approved ? (
                    <Badge variant="outline" className="gap-1.5">
                      <span className="size-2 rounded-full bg-emerald-500" />
                      approved
                    </Badge>
                  ) : (
                    <span className="text-xs text-muted-foreground">pending review</span>
                  )}
                  {p.reboot_likely && (
                    <Badge variant="secondary" className="ml-1">
                      reboot
                    </Badge>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      {jobs.length > 0 && (
        <section>
          <h2 className="mb-2 text-sm font-medium text-muted-foreground">Deployments</h2>
          <Table>
            <TableBody>
              {jobs.map((j) => (
                <TableRow key={j.id}>
                  <TableCell>
                    <JobStatusBadge status={j.status} />
                  </TableCell>
                  <TableCell className="text-sm">
                    {j.installed.length}/{j.external_ids.length} installed
                    {Object.keys(j.failed).length > 0 &&
                      ` · ${Object.keys(j.failed).length} failed`}
                  </TableCell>
                  <TableCell>
                    {j.reboot_required && <Badge variant="secondary">reboot required</Badge>}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {j.requested_by} · {new Date(j.queued_at).toLocaleTimeString()}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </section>
      )}
    </div>
  )
}
