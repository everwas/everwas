import { useCallback, useEffect, useMemo, useState } from "react"
import { Link, useParams } from "react-router-dom"
import { ArrowLeft, Cpu, HardDrive, History, MemoryStick, Monitor, Wifi } from "lucide-react"

import {
  api,
  type DeviceDetail,
  type Fact,
  type FactKind,
  type ScriptRun,
  type ShellSessionRow,
  type SnapshotKind,
  type NetInterfaceSeries,
  type TelemetryPoint,
} from "@/lib/api"
import { SessionPlayer } from "@/components/SessionPlayer"
import { DeviceTerminal } from "@/components/Terminal"
import {
  NetworkPanel,
  NetworkSummaryChart,
  NetworkTile,
} from "@/components/NetworkPanel"
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

const TABS = [
  "software",
  "hardware",
  "network",
  "logins",
  "processes",
  "services",
  "terminal",
  "runs",
  "sessions",
] as const
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
  if (kind === "logins") {
    return <LoginsTable facts={facts} />
  }
  return <HardwareFacts facts={facts} />
}

/** Who is signed in and where they are sitting.
 *
 * Rendered from bitemporal facts like the other tabs, so the time-machine
 * control scrubs this back too: "who was on this box when the change landed"
 * is the version of the question people actually need answered.
 */
function LoginsTable({ facts }: { facts: Fact[] }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>User</TableHead>
          <TableHead>Seat</TableHead>
          <TableHead>From</TableHead>
          <TableHead>Signed in</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {facts.map((f) => {
          const p = f.payload as Record<string, unknown>
          const remote = p.kind === "remote"
          return (
            <TableRow key={f.fact_key}>
              <TableCell className="font-mono text-xs">{String(p.user ?? "")}</TableCell>
              <TableCell>
                <span className="flex items-center gap-1.5 font-mono text-xs">
                  {remote ? (
                    <Wifi className="size-3.5 text-muted-foreground" aria-label="Remote" />
                  ) : (
                    <Monitor className="size-3.5 text-muted-foreground" aria-label="Console" />
                  )}
                  {String(p.terminal || "n/a")}
                  {/* Windows keeps a disconnected session alive with the
                      user's programs still running, which is a different
                      thing from being signed out and is worth saying. */}
                  {p.state ? <Badge variant="outline">{String(p.state)}</Badge> : null}
                </span>
              </TableCell>
              <TableCell className="font-mono text-xs text-muted-foreground">
                {String(p.host || (remote ? "n/a" : "local"))}
              </TableCell>
              <TableCell className="text-xs text-muted-foreground">
                {p.since ? new Date(String(p.since)).toLocaleString() : "n/a"}
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}

/** Byte counts, as a person reads them.
 *
 * Binary units because that is what the agent measures: gopsutil reports what
 * the kernel reports, and a kernel counts memory in powers of two. Calling
 * 2075656192 "2.1 GB" would be arithmetically defensible and would not match
 * the number on the RAM stick. */
function bytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "n/a"
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"]
  const i = Math.min(Math.floor(Math.log2(n) / 10), units.length - 1)
  const v = n / 1024 ** i
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

function hz(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(2)} GHz` : `${Math.round(n)} MHz`
}

/** snake_case -> Sentence case, with the abbreviations people actually write. */
const LABELS: Record<string, string> = {
  cpu: "Processor",
  memory: "Memory",
  os: "Operating system",
  system: "System",
  bios: "Firmware",
  baseboard: "Motherboard",
  cores: "Cores",
  logical_cores: "Logical cores",
  model: "Model",
  mhz: "Clock",
  total: "Total",
  available: "Available",
  swap_total: "Swap",
  family: "Family",
  kernel: "Kernel",
  version: "Version",
  build: "Build",
  arch: "Architecture",
  hostname: "Hostname",
  virtualization: "Virtualization",
  serial: "Serial number",
  vendor: "Vendor",
  uuid: "Machine UUID",
}

function label(key: string): string {
  return LABELS[key] ?? key.replace(/_/g, " ").replace(/^./, (c) => c.toUpperCase())
}

/** Which keys are byte counts. Chosen by NAME rather than by guessing from
 *  magnitude: a 2000000000 that happens to be a serial number must not be
 *  rendered as "1.9 GiB". */
const BYTE_KEYS = new Set(["total", "available", "used", "free", "swap_total", "size"])

function value(key: string, v: unknown): string {
  if (v === null || v === undefined || v === "") return "n/a"
  if (typeof v === "number") {
    if (BYTE_KEYS.has(key)) return bytes(v)
    if (key === "mhz") return hz(v)
    return v.toLocaleString()
  }
  if (typeof v === "boolean") return v ? "yes" : "no"
  if (Array.isArray(v)) return v.length ? v.map(String).join(", ") : "n/a"
  if (typeof v === "object") return JSON.stringify(v)
  return String(v)
}

//: Stable, most-identifying first. Unknown keys keep their own order after
//: these, so a new fact from a future agent still renders sensibly.
const FACT_ORDER = ["system", "cpu", "memory", "os", "baseboard", "bios"]

function HardwareFacts({ facts }: { facts: Fact[] }) {
  const ordered = [...facts].sort((a, b) => {
    const ia = FACT_ORDER.indexOf(a.fact_key)
    const ib = FACT_ORDER.indexOf(b.fact_key)
    return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib) || a.fact_key.localeCompare(b.fact_key)
  })

  return (
    <div className="grid gap-3 p-4 sm:grid-cols-2 xl:grid-cols-3">
      {ordered.map((f) => {
        const entries = Object.entries((f.payload ?? {}) as Record<string, unknown>)
        return (
          <Card key={f.fact_key}>
            <CardContent className="flex flex-col gap-2 py-4">
              <p className="text-sm font-medium">{label(f.fact_key)}</p>
              <dl className="flex flex-col gap-1">
                {entries.map(([k, v]) => (
                  <div key={k} className="flex items-baseline justify-between gap-4">
                    <dt className="shrink-0 text-xs text-muted-foreground">{label(k)}</dt>
                    <dd
                      className="truncate text-right text-sm tabular-nums"
                      title={typeof v === "string" ? v : undefined}
                    >
                      {value(k, v)}
                    </dd>
                  </div>
                ))}
              </dl>
              {f.valid_from && (
                /* Facts are bitemporal, so "since" is the wire time this
                   became true, not when the row was written. */
                <p className="text-xs text-muted-foreground">
                  since {new Date(f.valid_from).toLocaleString()}
                </p>
              )}
            </CardContent>
          </Card>
        )
      })}
    </div>
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
  const [nets, setNets] = useState<NetInterfaceSeries[]>([])
  const [sessions, setSessions] = useState<ShellSessionRow[]>([])
  const [playing, setPlaying] = useState<string | null>(null)

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
    api.network(id).then(setNets).catch(() => {})
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
    } else if (tab === "sessions") {
      api.shellSessions(id).then(setSessions).catch(() => {})
    } else if (tab !== "terminal" && tab !== "network") {
      // Clear first. Facts from the previous tab are shaped for a different
      // renderer, and if this fetch fails they stay on screen reinterpreted by
      // it: a few hundred Debian packages once rendered as a few hundred blank
      // login rows, which looks like data rather than like an error.
      setFacts([])
      api.facts(id, tab, asOf).then(setFacts).catch(() => setFacts([]))
    }
  }, [id, tab, asOf])

  useEffect(loadTab, [loadTab])

  // Not gated on the tab: the summary chart is visible from every one of them.
  // 30s rather than the 4s runs uses, because the agent samples every 60s and
  // polling faster only re-renders the same points.
  useEffect(() => {
    if (!id) return
    const timer = setInterval(() => api.network(id).then(setNets).catch(() => {}), 30000)
    return () => clearInterval(timer)
  }, [id])

  useEffect(() => {
    if (tab !== "runs" || !id) return
    const timer = setInterval(() => api.runs(id).then(setRuns).catch(() => {}), 4000)
    return () => clearInterval(timer)
  }, [tab, id])

  if (!device) return <p className="text-sm text-muted-foreground">Loading…</p>

  const isFactTab = tab === "software" || tab === "hardware" || tab === "logins"

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
        <NetworkTile interfaces={nets} />
      </div>

      <div className="flex flex-wrap gap-4">
        <TelemetryChart title="CPU %" data={telemetry} dataKey="cpu_pct" color="var(--chart-cpu)" />
        <TelemetryChart
          title="Memory %"
          data={telemetry}
          dataKey="mem_pct"
          color="var(--chart-mem)"
        />
        <NetworkSummaryChart interfaces={nets} />
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

        {tab === "network" ? (
          <div className="p-4">
            <NetworkPanel interfaces={nets} />
          </div>
        ) : tab === "sessions" ? (
          <div className="flex flex-col gap-4 p-4">
            {sessions.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No shell sessions on this device yet.
              </p>
            ) : (
              <>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Started</TableHead>
                      <TableHead>Ended</TableHead>
                      <TableHead>Reason</TableHead>
                      <TableHead>Output</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {sessions.map((s) => (
                      <TableRow key={s.id}>
                        <TableCell className="text-xs">
                          {new Date(s.started_at).toLocaleString()}
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {s.ended_at ? new Date(s.ended_at).toLocaleTimeString() : "open"}
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {s.close_reason ?? "-"}
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {s.bytes_out.toLocaleString()} B
                        </TableCell>
                        <TableCell className="text-right">
                          {s.recording_path ? (
                            <Button
                              size="sm"
                              variant={playing === s.id ? "secondary" : "outline"}
                              onClick={() => setPlaying(playing === s.id ? null : s.id)}
                            >
                              {playing === s.id ? "Close" : "Replay"}
                            </Button>
                          ) : (
                            /* Recording is opt-in per deployment, so say which
                               it is rather than showing a dead button. */
                            <span className="text-xs text-muted-foreground">not recorded</span>
                          )}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
                {playing && <SessionPlayer key={playing} src={api.recordingUrl(playing)} />}
              </>
            )}
          </div>
        ) : tab === "terminal" ? (
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
