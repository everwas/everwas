import { useCallback, useEffect, useMemo, useState } from "react"
import { Link, useParams } from "react-router-dom"
import { ArrowLeft, Cpu, HardDrive, History, MemoryStick } from "lucide-react"

import {
  api,
  type DeviceDetail,
  type Fact,
  type FactKind,
  type ScriptRun,
  type SnapshotKind,
  type TelemetryPoint,
} from "@/lib/api"
import { DeviceTerminal } from "@/components/Terminal"
import { TelemetryChart } from "@/components/TelemetryChart"
import { RunStatusBadge } from "@/pages/Scripts"
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

const TABS = ["software", "hardware", "processes", "services", "terminal", "runs"] as const
type Tab = (typeof TABS)[number]

const TIME_MACHINE_MAX_HOURS = 7 * 24

function StatTile({
  icon: Icon,
  label,
  value,
}: {
  icon: typeof Cpu
  label: string
  value: number | null | undefined
}) {
  return (
    <Card className="flex-1 min-w-36">
      <CardContent className="flex items-center gap-3 py-4">
        <Icon className="size-5 text-muted-foreground" />
        <div>
          <p className="text-xs text-muted-foreground">{label}</p>
          <p className="text-xl font-semibold">
            {value == null ? "n/a" : `${value.toFixed(0)}%`}
          </p>
        </div>
      </CardContent>
    </Card>
  )
}

function FactsTable({ facts, kind }: { facts: Fact[]; kind: FactKind }) {
  if (facts.length === 0) {
    return <p className="p-6 text-sm text-muted-foreground">No {kind} facts recorded.</p>
  }
  if (kind === "software") {
    return (
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Package</TableHead>
            <TableHead>Version</TableHead>
            <TableHead>Since</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {facts.map((f) => (
            <TableRow key={f.fact_key}>
              <TableCell className="font-mono text-xs">{f.fact_key.replace("pkg:", "")}</TableCell>
              <TableCell className="font-mono text-xs">{String(f.payload.version ?? "")}</TableCell>
              <TableCell className="text-xs text-muted-foreground">
                {f.valid_from ? new Date(f.valid_from).toLocaleString() : ""}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    )
  }
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Fact</TableHead>
          <TableHead>Value</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {facts.map((f) => (
          <TableRow key={f.fact_key}>
            <TableCell className="font-medium">{f.fact_key}</TableCell>
            <TableCell className="font-mono text-xs">
              {JSON.stringify(f.payload)}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

function SnapshotTable({ kind, rows }: { kind: SnapshotKind; rows: Record<string, unknown>[] }) {
  if (rows.length === 0) {
    return <p className="p-6 text-sm text-muted-foreground">No {kind} snapshot yet.</p>
  }
  const columns = kind === "processes" ? ["pid", "name", "mem_rss"] : ["name", "state"]
  return (
    <Table>
      <TableHeader>
        <TableRow>
          {columns.map((c) => (
            <TableHead key={c} className="capitalize">
              {c.replace("mem_rss", "memory")}
            </TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.slice(0, 200).map((r, i) => (
          <TableRow key={i}>
            {columns.map((c) => (
              <TableCell key={c} className="font-mono text-xs">
                {c === "mem_rss" && typeof r[c] === "number"
                  ? `${((r[c] as number) / 2 ** 20).toFixed(0)} MiB`
                  : String(r[c] ?? "")}
              </TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

export function DeviceDetailPage() {
  const { id } = useParams<{ id: string }>()
  const [device, setDevice] = useState<DeviceDetail | null>(null)
  const [telemetry, setTelemetry] = useState<TelemetryPoint[]>([])
  const [tab, setTab] = useState<Tab>("software")
  const [facts, setFacts] = useState<Fact[]>([])
  const [snapshotRows, setSnapshotRows] = useState<Record<string, unknown>[]>([])
  const [runs, setRuns] = useState<ScriptRun[]>([])

  // time machine: hoursBack = 0 means "now"
  const [timeMachine, setTimeMachine] = useState(false)
  const [hoursBack, setHoursBack] = useState(0)
  const asOf = useMemo(
    () => (timeMachine && hoursBack > 0 ? new Date(Date.now() - hoursBack * 3600_000) : undefined),
    [timeMachine, hoursBack],
  )

  useEffect(() => {
    if (!id) return
    api.device(id).then(setDevice).catch(() => {})
    api.telemetry(id).then(setTelemetry).catch(() => {})
  }, [id])

  const loadTab = useCallback(() => {
    if (!id) return
    if (tab === "processes" || tab === "services") {
      api.snapshot(id, tab).then((s) => {
        const rows = (s.payload?.[tab] ?? []) as Record<string, unknown>[]
        setSnapshotRows(rows)
      })
    } else if (tab === "runs") {
      api.runs(id).then(setRuns).catch(() => {})
    } else if (tab !== "terminal") {
      api.facts(id, tab, asOf).then(setFacts).catch(() => {})
    }
  }, [id, tab, asOf])

  useEffect(loadTab, [loadTab])

  useEffect(() => {
    if (tab !== "runs" || !id) return
    const timer = setInterval(() => api.runs(id).then(setRuns).catch(() => {}), 4000)
    return () => clearInterval(timer)
  }, [tab, id])

  if (!device) return <p className="text-sm text-muted-foreground">Loading…</p>

  const isFactTab = tab === "software" || tab === "hardware"

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="ghost" size="sm" asChild>
          <Link to="/">
            <ArrowLeft className="size-4" />
          </Link>
        </Button>
        <h1 className="text-xl font-semibold">{device.hostname}</h1>
        <Badge variant="outline" className="gap-1.5 capitalize">
          <span
            className={`size-2 rounded-full ${
              device.status === "active"
                ? "bg-emerald-500"
                : device.status === "offline"
                  ? "bg-red-500"
                  : "bg-amber-500"
            }`}
          />
          {device.status === "active" ? "online" : device.status}
        </Badge>
        <span className="text-sm text-muted-foreground">
          {device.os_version || device.os_family} · {device.arch} · agent {device.agent_version}
        </span>
      </div>

      <div className="flex flex-wrap gap-4">
        <StatTile icon={Cpu} label="CPU" value={device.cpu_pct} />
        <StatTile icon={MemoryStick} label="Memory" value={device.mem_pct} />
        <StatTile icon={HardDrive} label="Worst disk" value={device.worst_disk_pct} />
      </div>

      <div className="flex flex-wrap gap-4">
        <TelemetryChart title="CPU %" data={telemetry} dataKey="cpu_pct" color="var(--chart-cpu)" />
        <TelemetryChart
          title="Memory %"
          data={telemetry}
          dataKey="mem_pct"
          color="var(--chart-mem)"
        />
      </div>

      <div>
        <div className="flex flex-wrap items-center gap-1 border-b">
          {TABS.map((t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`px-3 py-2 text-sm capitalize transition-colors ${
                tab === t
                  ? "border-b-2 border-foreground font-medium"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              {t}
            </button>
          ))}
          {isFactTab && (
            <Button
              variant={timeMachine ? "secondary" : "ghost"}
              size="sm"
              className="ml-auto gap-1.5"
              onClick={() => {
                setTimeMachine((v) => !v)
                setHoursBack(0)
              }}
            >
              <History className="size-4" />
              Time machine
            </Button>
          )}
        </div>

        {isFactTab && timeMachine && (
          <div className="flex flex-wrap items-center gap-3 border-b bg-muted/40 px-3 py-2">
            <span className="text-xs text-muted-foreground">7 days ago</span>
            <input
              type="range"
              min={0}
              max={TIME_MACHINE_MAX_HOURS}
              step={1}
              value={TIME_MACHINE_MAX_HOURS - hoursBack}
              onChange={(e) => setHoursBack(TIME_MACHINE_MAX_HOURS - Number(e.target.value))}
              className="max-w-md flex-1 accent-foreground"
            />
            <span className="text-xs text-muted-foreground">now</span>
            <Badge variant={hoursBack > 0 ? "default" : "outline"}>
              {hoursBack > 0 ? `as of ${asOf?.toLocaleString()}` : "live"}
            </Badge>
          </div>
        )}

        {tab === "terminal" ? (
          <div className="pt-4">
            <DeviceTerminal deviceId={id!} />
          </div>
        ) : tab === "runs" ? (
          <div className="max-h-[28rem] overflow-y-auto">
            {runs.length === 0 ? (
              <p className="p-6 text-sm text-muted-foreground">
                No script runs for this device yet.
              </p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Status</TableHead>
                    <TableHead>Exit</TableHead>
                    <TableHead>Output</TableHead>
                    <TableHead>By</TableHead>
                    <TableHead>Queued</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {runs.map((r) => (
                    <TableRow key={r.id}>
                      <TableCell>
                        <RunStatusBadge status={r.status} />
                      </TableCell>
                      <TableCell className="text-muted-foreground">{r.exit_code ?? "-"}</TableCell>
                      <TableCell className="max-w-md whitespace-pre-wrap font-mono text-xs">
                        {(r.stdout || r.stderr || "").slice(0, 400) || "-"}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {r.requested_by ?? "-"}
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
        ) : (
          <div className="max-h-[28rem] overflow-y-auto">
            {isFactTab ? (
              <FactsTable facts={facts} kind={tab as FactKind} />
            ) : (
              <SnapshotTable kind={tab as SnapshotKind} rows={snapshotRows} />
            )}
          </div>
        )}
      </div>
    </div>
  )
}
