import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts"

import type { TelemetryPoint } from "@/lib/api"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"

type Props = {
  title: string
  data: TelemetryPoint[]
  dataKey: "cpu_pct" | "mem_pct"
  color: string // CSS var, e.g. "var(--chart-cpu)"
}

function hourLabel(iso: string): string {
  const d = new Date(iso)
  return `${d.getHours().toString().padStart(2, "0")}:${d.getMinutes().toString().padStart(2, "0")}`
}

export function TelemetryChart({ title, data, dataKey, color }: Props) {
  return (
    <Card className="flex-1 min-w-64">
      <CardHeader className="pb-2">
        <CardTitle className="text-sm font-medium text-muted-foreground">{title}</CardTitle>
      </CardHeader>
      <CardContent className="h-44 pt-0">
        {data.length === 0 ? (
          <p className="flex h-full items-center justify-center text-sm text-muted-foreground">
            No telemetry yet
          </p>
        ) : (
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={data} margin={{ top: 4, right: 8, bottom: 0, left: -16 }}>
              <CartesianGrid stroke="var(--viz-grid)" strokeWidth={1} vertical={false} />
              <XAxis
                dataKey="ts"
                tickFormatter={hourLabel}
                stroke="var(--viz-muted)"
                tickLine={false}
                axisLine={{ stroke: "var(--viz-axis)" }}
                fontSize={11}
                minTickGap={48}
              />
              <YAxis
                domain={[0, 100]}
                unit="%"
                stroke="var(--viz-muted)"
                tickLine={false}
                axisLine={false}
                fontSize={11}
                width={54}
              />
              <Tooltip
                labelFormatter={(iso) => new Date(iso as string).toLocaleString()}
                formatter={(value) => [`${(value as number)?.toFixed(1)}%`, title]}
                contentStyle={{
                  background: "var(--popover)",
                  border: "1px solid var(--border)",
                  borderRadius: "0.5rem",
                  color: "var(--popover-foreground)",
                  fontSize: "12px",
                }}
              />
              <Line
                type="monotone"
                dataKey={dataKey}
                stroke={color}
                strokeWidth={2}
                dot={false}
                isAnimationActive={false}
                connectNulls
              />
            </LineChart>
          </ResponsiveContainer>
        )}
      </CardContent>
    </Card>
  )
}
