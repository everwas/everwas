import { useMemo, useState } from "react"
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts"
import { ArrowDown, ArrowUp, CircleSlash, Network, TriangleAlert } from "lucide-react"

import type { NetInterfaceSeries, NetRatePoint } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"

/** Bytes per second in the largest unit that keeps the number readable. */
function rate(bytesPerSecond: number | null | undefined): string {
  // Spelled out rather than a dash: a screen reader announces "n/a"
  // usefully and a dash as nothing at all.
  if (bytesPerSecond == null) return "n/a"
  const units = ["B/s", "KB/s", "MB/s", "GB/s"]
  let value = bytesPerSecond
  let unit = 0
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000
    unit++
  }
  return `${value.toFixed(value < 10 && unit > 0 ? 1 : 0)} ${units[unit]}`
}

/** Axis-tick form of a rate: "6M", "22K", "0".
 *
 * The full "6.0 MB/s" wraps onto two lines in the narrow summary card and needs
 * a 60px gutter on both sides, which leaves the plot visibly thinner than the
 * CPU and memory charts beside it. The unit is already established by the axis
 * colour and the tooltip, so the tick only has to carry magnitude.
 */
function tickRate(bytesPerSecond: number): string {
  if (!bytesPerSecond) return "0"
  const units = ["", "K", "M", "G"]
  let value = bytesPerSecond
  let unit = 0
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000
    unit++
  }
  return `${value < 10 && unit > 0 ? value.toFixed(1) : Math.round(value)}${units[unit]}`
}

function clockLabel(iso: string): string {
  const d = new Date(iso)
  return `${d.getHours().toString().padStart(2, "0")}:${d.getMinutes().toString().padStart(2, "0")}`
}

/** Most recent non-null value of one field, or null if the tail is all gaps. */
function latest(points: NetRatePoint[], key: keyof NetRatePoint): number | null {
  for (let i = points.length - 1; i >= 0; i--) {
    const v = points[i][key]
    if (typeof v === "number") return v
  }
  return null
}

/** Total of the error and drop rates across the window.
 *
 * Errors are shown as a count rather than charted: they are zero on a healthy
 * machine, so plotting them next to throughput puts a flat line at the bottom
 * of an axis scaled in megabytes and hides the one thing worth noticing.
 */
function faults(points: NetRatePoint[]): number {
  const keys: (keyof NetRatePoint)[] = ["err_in", "err_out", "drop_in", "drop_out"]
  return points.reduce(
    (sum, p) => sum + keys.reduce((s, k) => s + (typeof p[k] === "number" ? (p[k] as number) : 0), 0),
    0,
  )
}

function InterfaceChart({
  iface,
  compact = false,
}: {
  iface: NetInterfaceSeries
  compact?: boolean
}) {
  const gaps = iface.points.filter((p) => p.bytes_recv == null).length

  if (iface.points.length === 0) {
    return (
      <div className="flex h-full min-h-32 flex-col items-center justify-center gap-2 text-muted-foreground">
        <CircleSlash className="size-5" aria-hidden />
        <p className="text-sm">
          {compact ? "No traffic yet" : "No traffic recorded on this interface"}
        </p>
      </div>
    )
  }

  return (
    <>
      <div className={compact ? "h-full" : "h-56"}>
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={iface.points} margin={{ top: 8, right: 0, bottom: 0, left: -8 }}>
            <defs>
              {/* Only the download series is filled. Two translucent areas
                  overlapping blend into a muddy third colour wherever they
                  cross, which is most of a busy chart and is worst in dark
                  mode. Filling one and stroking the other keeps both legible
                  and makes it obvious which is which. */}
              <linearGradient id="net-in" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor="var(--chart-net-in)" stopOpacity={0.28} />
                <stop offset="100%" stopColor="var(--chart-net-in)" stopOpacity={0.02} />
              </linearGradient>
            </defs>
            <CartesianGrid stroke="var(--viz-grid)" strokeWidth={1} vertical={false} />
            <XAxis
              dataKey="ts"
              tickFormatter={clockLabel}
              stroke="var(--viz-muted)"
              tickLine={false}
              axisLine={{ stroke: "var(--viz-axis)" }}
              fontSize={11}
              minTickGap={48}
            />
            {/* One axis per direction, each tinted to match its line.
                Download typically dwarfs upload by two or three orders of
                magnitude, and on a shared axis the upload series is a flat
                line along zero: present, unreadable, and therefore worse than
                absent because it looks like no traffic. Independent scales
                make both legible. The colour coding is what keeps a dual axis
                honest, since it stops the crossing point reading as equality. */}
            <YAxis
              yAxisId="in"
              tickFormatter={(v) => (compact ? tickRate(v as number) : rate(v as number))}
              stroke="var(--chart-net-in)"
              tickLine={false}
              axisLine={false}
              fontSize={11}
              width={compact ? 40 : 72}
            />
            <YAxis
              yAxisId="out"
              orientation="right"
              tickFormatter={(v) => (compact ? tickRate(v as number) : rate(v as number))}
              stroke="var(--chart-net-out)"
              tickLine={false}
              axisLine={false}
              fontSize={11}
              width={compact ? 40 : 72}
            />
            <Tooltip
              labelFormatter={(iso) => new Date(iso as string).toLocaleString()}
              formatter={(value, name) => [
                rate(value as number),
                name === "bytes_recv" ? "Received" : "Sent",
              ]}
              contentStyle={{
                background: "var(--popover)",
                border: "1px solid var(--border)",
                borderRadius: "0.5rem",
                color: "var(--popover-foreground)",
                fontSize: "12px",
              }}
            />
            {/* connectNulls stays false on purpose. A null here means the rate
                is unknown (counter reset, or the agent was away), and drawing
                through it would invent a smooth line across an outage. The
                break IS the information. */}
            {(
              [
                ["bytes_recv", "in"],
                ["bytes_sent", "out"],
              ] as const
            ).map(([key, dir]) => (
              <Area
                key={key}
                yAxisId={dir}
                type="monotone"
                dataKey={key}
                stroke={`var(--chart-net-${dir})`}
                strokeWidth={2}
                fill={dir === "in" ? "url(#net-in)" : "none"}
                connectNulls={false}
                dot={false}
                activeDot={{ r: 3 }}
                isAnimationActive={false}
              />
            ))}
          </AreaChart>
        </ResponsiveContainer>
      </div>
      {!compact && gaps > 0 && (
        <p className="mt-1 text-xs text-muted-foreground">
          {gaps === 1 ? "1 gap" : `${gaps} gaps`} in this window, where the counter reset or the
          agent was offline. Breaks in the line are missing data, not idle time.
        </p>
      )}
    </>
  )
}

export function NetworkPanel({ interfaces }: { interfaces: NetInterfaceSeries[] }) {
  // Interfaces that carry traffic first, then the rest by name. On a Windows
  // box the list is mostly loopback and virtual adapters, and the one NIC that
  // matters should not be buried among them.
  const ordered = useMemo(
    () =>
      [...interfaces].sort((a, b) => {
        const aTraffic = a.points.length > 0
        const bTraffic = b.points.length > 0
        if (aTraffic !== bTraffic) return aTraffic ? -1 : 1
        return a.name.localeCompare(b.name)
      }),
    [interfaces],
  )
  const [selected, setSelected] = useState<string | null>(null)
  const active = ordered.find((i) => i.name === selected) ?? ordered[0]

  if (ordered.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center gap-2 py-16 text-muted-foreground">
        <Network className="size-6" aria-hidden />
        <p className="text-sm">No interfaces reported yet</p>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      {ordered.length > 1 && (
        <div className="flex flex-wrap gap-1" role="tablist" aria-label="Network interfaces">
          {ordered.map((i) => (
            <Button
              key={i.name}
              role="tab"
              aria-selected={i.name === active.name}
              variant={i.name === active.name ? "secondary" : "ghost"}
              size="sm"
              className="h-7 font-mono text-xs"
              onClick={() => setSelected(i.name)}
            >
              {i.up === false && (
                <span
                  className="mr-1.5 size-1.5 rounded-full bg-muted-foreground"
                  aria-hidden
                />
              )}
              {i.name}
            </Button>
          ))}
        </div>
      )}

      <Card>
        <CardHeader className="flex flex-row flex-wrap items-baseline justify-between gap-2 pb-2">
          <div className="space-y-1">
            <CardTitle className="font-mono text-sm">{active.name}</CardTitle>
            <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
              {active.up === false ? (
                <Badge variant="outline">down</Badge>
              ) : (
                <Badge variant="secondary">up</Badge>
              )}
              {active.mac && <span className="font-mono">{active.mac}</span>}
              {active.addresses.map((a) => (
                <span key={a} className="font-mono">
                  {a}
                </span>
              ))}
            </div>
          </div>
          <div className="flex gap-4 text-sm">
            <span className="flex items-center gap-1.5">
              <ArrowDown className="size-3.5" style={{ color: "var(--chart-net-in)" }} aria-hidden />
              <span className="font-mono tabular-nums">
                {rate(latest(active.points, "bytes_recv"))}
              </span>
            </span>
            <span className="flex items-center gap-1.5">
              <ArrowUp className="size-3.5" style={{ color: "var(--chart-net-out)" }} aria-hidden />
              <span className="font-mono tabular-nums">
                {rate(latest(active.points, "bytes_sent"))}
              </span>
            </span>
          </div>
        </CardHeader>
        <CardContent className="pt-0">
          <InterfaceChart iface={active} />
          {faults(active.points) > 0 && (
            <p className="mt-2 flex items-center gap-1.5 text-xs text-amber-600 dark:text-amber-500">
              <TriangleAlert className="size-3.5" aria-hidden />
              Errors or dropped packets recorded in this window
            </p>
          )}
        </CardContent>
      </Card>
    </div>
  )
}


/** The interface a summary should show: the one carrying the most traffic.
 *
 * "Most traffic" rather than "first" or "not loopback". A machine can have a
 * dozen adapters (Windows virtual switches, tunnels, bridges) and any name-based
 * rule for picking the real one is a guess that breaks on the next platform.
 * Volume is the thing that actually makes an interface the interesting one.
 */
export function primaryInterface(interfaces: NetInterfaceSeries[]): NetInterfaceSeries | null {
  let best: NetInterfaceSeries | null = null
  let bestVolume = -1
  for (const iface of interfaces) {
    const volume = iface.points.reduce(
      (sum, p) => sum + (p.bytes_recv ?? 0) + (p.bytes_sent ?? 0),
      0,
    )
    if (volume > bestVolume) {
      best = iface
      bestVolume = volume
    }
  }
  return best
}

/** Current throughput, sized to sit in the row of CPU / memory / disk tiles. */
export function NetworkTile({ interfaces }: { interfaces: NetInterfaceSeries[] }) {
  const iface = primaryInterface(interfaces)
  const down = iface ? latest(iface.points, "bytes_recv") : null
  const up = iface ? latest(iface.points, "bytes_sent") : null

  return (
    <Card className="flex-1 min-w-36">
      <CardContent className="flex items-center gap-3 py-4">
        <Network className="size-5 text-muted-foreground" aria-hidden />
        <div className="min-w-0">
          <p className="truncate text-xs text-muted-foreground">
            {iface ? `Network · ${iface.name}` : "Network"}
          </p>
          <p className="flex items-center gap-2 font-semibold tabular-nums">
            <span className="flex items-center gap-1">
              <ArrowDown
                className="size-3.5 shrink-0"
                style={{ color: "var(--chart-net-in)" }}
                aria-label="Received"
              />
              <span className="text-base">{rate(down)}</span>
            </span>
            <span className="flex items-center gap-1">
              <ArrowUp
                className="size-3.5 shrink-0"
                style={{ color: "var(--chart-net-out)" }}
                aria-label="Sent"
              />
              <span className="text-base">{rate(up)}</span>
            </span>
          </p>
        </div>
      </CardContent>
    </Card>
  )
}

/** Throughput chart for the summary row, matching the CPU and memory charts.
 *
 * Shows the busiest interface only. Every interface at once belongs in the
 * Network tab, where there is room to pick between them; here the point is a
 * glance, and stacking a dozen adapters into one card answers no question.
 */
export function NetworkSummaryChart({ interfaces }: { interfaces: NetInterfaceSeries[] }) {
  const iface = primaryInterface(interfaces)

  return (
    <Card className="flex-1 min-w-64">
      <CardHeader className="pb-2">
        <CardTitle className="flex items-baseline justify-between gap-2 text-sm font-medium text-muted-foreground">
          <span>Network</span>
          {iface && <span className="truncate font-mono text-xs">{iface.name}</span>}
        </CardTitle>
      </CardHeader>
      <CardContent className="h-44 pt-0">
        {iface === null ? (
          <p className="flex h-full items-center justify-center text-sm text-muted-foreground">
            No interfaces yet
          </p>
        ) : (
          <InterfaceChart iface={iface} compact />
        )}
      </CardContent>
    </Card>
  )
}
