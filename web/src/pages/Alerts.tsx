import { useCallback, useEffect, useState } from "react"
import { BellRing, Check, MailWarning, Plus, Send, Trash2, TriangleAlert } from "lucide-react"

import {
  api,
  type Alert,
  type AlertMetric,
  type AlertRule,
  type Channel,
  type Device,
  type OutboxHealth,
  type Severity,
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

const REFRESH_MS = 5000
const METRICS: AlertMetric[] = ["cpu", "memory", "disk", "heartbeat_missed"]
const SEVERITIES: Severity[] = ["info", "warning", "critical"]

// severity dots use the validated status palette, never color alone:
// every badge carries its label too.
const SEVERITY_DOT: Record<Severity, string> = {
  info: "bg-[#2a78d6]",
  warning: "bg-[#eda100]",
  critical: "bg-[#e34948]",
}

function SeverityBadge({ severity }: { severity: Severity }) {
  return (
    <Badge variant="outline" className="gap-1.5">
      <span className={`size-2 rounded-full ${SEVERITY_DOT[severity]}`} />
      {severity}
    </Badge>
  )
}

function since(iso: string): string {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return `${Math.floor(s)}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

function NewRuleForm({ channels, onCreated }: { channels: Channel[]; onCreated: () => void }) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [metric, setMetric] = useState<AlertMetric>("cpu")
  const [threshold, setThreshold] = useState(90)
  const [durationS, setDurationS] = useState(300)
  const [severity, setSeverity] = useState<Severity>("warning")
  const [channelIds, setChannelIds] = useState<string[]>([])

  if (!open) {
    return (
      <Button size="sm" className="gap-1.5" onClick={() => setOpen(true)}>
        <Plus className="size-4" />
        New rule
      </Button>
    )
  }

  return (
    <Card className="w-full">
      <CardHeader className="pb-3">
        <CardTitle className="text-base">New alert rule</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <div className="flex flex-wrap gap-3">
          <div className="flex flex-1 flex-col gap-1.5">
            <Label htmlFor="rule-name">Name</Label>
            <Input
              id="rule-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="CPU sustained high"
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="rule-metric">Metric</Label>
            <select
              id="rule-metric"
              value={metric}
              onChange={(e) => setMetric(e.target.value as AlertMetric)}
              className="h-9 rounded-md border bg-transparent px-3 text-sm"
            >
              {METRICS.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </div>
          {metric !== "heartbeat_missed" && (
            <div className="flex w-28 flex-col gap-1.5">
              <Label htmlFor="rule-threshold">Above %</Label>
              <Input
                id="rule-threshold"
                type="number"
                value={threshold}
                onChange={(e) => setThreshold(Number(e.target.value))}
              />
            </div>
          )}
          <div className="flex w-32 flex-col gap-1.5">
            <Label htmlFor="rule-duration">For (sec)</Label>
            <Input
              id="rule-duration"
              type="number"
              value={durationS}
              onChange={(e) => setDurationS(Number(e.target.value))}
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="rule-severity">Severity</Label>
            <select
              id="rule-severity"
              value={severity}
              onChange={(e) => setSeverity(e.target.value as Severity)}
              className="h-9 rounded-md border bg-transparent px-3 text-sm"
            >
              {SEVERITIES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </div>
        </div>

        {channels.length === 0 ? (
          <p className="flex items-start gap-2 text-sm text-amber-700 dark:text-amber-400">
            <TriangleAlert className="mt-0.5 size-4 shrink-0" />
            No notification channels exist yet. This rule will record alerts in this
            page but will not reach anybody.
          </p>
        ) : (
          <div className="flex flex-col gap-1.5">
            <Label>Notify</Label>
            <div className="flex flex-wrap gap-3">
              {channels.map((c) => (
                <label key={c.id} className="flex items-center gap-1.5 text-sm">
                  <input
                    type="checkbox"
                    checked={channelIds.includes(c.id)}
                    onChange={(e) =>
                      setChannelIds((prev) =>
                        e.target.checked ? [...prev, c.id] : prev.filter((id) => id !== c.id),
                      )
                    }
                  />
                  {c.name} <span className="text-muted-foreground">({c.kind})</span>
                </label>
              ))}
            </div>
            {channelIds.length === 0 && (
              /* A rule with no channel is not an error: the alert is still
                 recorded and visible here. It is only dangerous when the
                 operator believes it will page someone, so say so rather than
                 blocking the save. */
              <p className="flex items-start gap-2 text-sm text-amber-700 dark:text-amber-400">
                <TriangleAlert className="mt-0.5 size-4 shrink-0" />
                Nothing selected. This rule will record alerts but notify nobody.
              </p>
            )}
          </div>
        )}

        <div className="flex gap-2">
          <Button
            size="sm"
            disabled={!name}
            onClick={async () => {
              await api.createRule({
                name,
                metric,
                operator: "gt",
                threshold: metric === "heartbeat_missed" ? 0 : threshold,
                duration_s: durationS,
                severity,
                target: { all: true },
                cooldown_s: 900,
                enabled: true,
                channel_ids: channelIds,
              })
              setName("")
              setChannelIds([])
              setOpen(false)
              onCreated()
            }}
          >
            {channelIds.length === 0 ? "Save without notifications" : "Save"}
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setOpen(false)}>
            Cancel
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

function NewChannelForm({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [kind, setKind] = useState<Channel["kind"]>("email")
  const [value, setValue] = useState("")

  if (!open) {
    return (
      <Button size="sm" variant="outline" className="gap-1.5" onClick={() => setOpen(true)}>
        <Plus className="size-4" />
        New channel
      </Button>
    )
  }

  const placeholder =
    kind === "email"
      ? "ops@example.com"
      : kind === "webhook"
        ? "https://hooks.example.com/openrmm"
        : kind === "ntfy"
          ? "my-openrmm-topic"
          : "https://gotify.example.com|token"

  return (
    <Card className="w-full">
      <CardHeader className="pb-3">
        <CardTitle className="text-base">New notification channel</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-wrap items-end gap-3">
        <div className="flex flex-1 flex-col gap-1.5">
          <Label htmlFor="ch-name">Name</Label>
          <Input id="ch-name" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="ch-kind">Kind</Label>
          <select
            id="ch-kind"
            value={kind}
            onChange={(e) => setKind(e.target.value as Channel["kind"])}
            className="h-9 rounded-md border bg-transparent px-3 text-sm"
          >
            {["email", "webhook", "ntfy", "gotify"].map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </select>
        </div>
        <div className="flex flex-1 flex-col gap-1.5">
          <Label htmlFor="ch-value">
            {kind === "email" ? "Recipient" : kind === "ntfy" ? "Topic" : "URL"}
          </Label>
          <Input
            id="ch-value"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder={placeholder}
          />
        </div>
        <Button
          size="sm"
          disabled={!name || !value}
          onClick={async () => {
            const config =
              kind === "email"
                ? { to: [value] }
                : kind === "ntfy"
                  ? { topic: value }
                  : kind === "gotify"
                    ? { url: value.split("|")[0], token: value.split("|")[1] ?? "" }
                    : { url: value }
            await api.createChannel({ name, kind, config, enabled: true })
            setName("")
            setValue("")
            setOpen(false)
            onCreated()
          }}
        >
          Save
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setOpen(false)}>
          Cancel
        </Button>
      </CardContent>
    </Card>
  )
}

/** Notification delivery health.
 *
 * An alert that fires and is never delivered leaves no trace anywhere else in
 * this UI: the alert row looks exactly the same whether it paged someone or
 * sat in a queue behind a channel that was deleted last Tuesday. This is the
 * only place that difference is visible, so it renders even when healthy.
 */
function DeliveryHealth({ health }: { health: OutboxHealth | null }) {
  if (!health) return null
  const broken = health.problems.length > 0

  return (
    <section
      className={
        broken
          ? "rounded-lg border border-amber-500/50 bg-amber-500/5 p-4"
          : "rounded-lg border p-4"
      }
    >
      <h2 className="mb-2 flex items-center gap-2 text-sm font-medium">
        {broken ? (
          <MailWarning className="size-4 text-amber-600 dark:text-amber-500" />
        ) : (
          <Send className="size-4 text-muted-foreground" />
        )}
        Notification delivery
      </h2>

      {broken ? (
        <ul className="mb-3 flex flex-col gap-1 text-sm">
          {health.problems.map((p) => (
            <li key={p} className="text-amber-700 dark:text-amber-400">
              {p}
            </li>
          ))}
        </ul>
      ) : (
        <p className="mb-3 text-sm text-muted-foreground">
          Everything queued has been delivered.
        </p>
      )}

      <dl className="flex flex-wrap gap-x-6 gap-y-1 text-sm">
        <div className="flex gap-1.5">
          <dt className="text-muted-foreground">Queued</dt>
          <dd className="tabular-nums">{health.pending}</dd>
        </div>
        <div className="flex gap-1.5">
          {/* Blocked is not failed. These are waiting on a channel an operator
              has to repair, and they deliver the moment it is fixed. */}
          <dt className="text-muted-foreground">Blocked on config</dt>
          <dd className="tabular-nums">{health.blocked}</dd>
        </div>
        <div className="flex gap-1.5">
          <dt className="text-muted-foreground">
            Failed ({Math.round(health.failed_window_s / 3600)}h)
          </dt>
          <dd className="tabular-nums">{health.failed_recent}</dd>
        </div>
        {health.oldest_pending_age_s !== null && (
          <div className="flex gap-1.5">
            <dt className="text-muted-foreground">Oldest undelivered</dt>
            <dd className="tabular-nums">{health.oldest_pending_age_s}s</dd>
          </div>
        )}
      </dl>
    </section>
  )
}

export function AlertsPage() {
  const [alerts, setAlerts] = useState<Alert[]>([])
  const [rules, setRules] = useState<AlertRule[]>([])
  const [channels, setChannels] = useState<Channel[]>([])
  const [devices, setDevices] = useState<Device[]>([])
  const [outbox, setOutbox] = useState<OutboxHealth | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const load = useCallback(() => {
    api.alerts().then(setAlerts).catch(() => {})
    api.alertRules().then(setRules).catch(() => {})
    api.channels().then(setChannels).catch(() => {})
    api.outboxHealth().then(setOutbox).catch(() => {})
  }, [])

  useEffect(() => {
    load()
    api.devices().then(setDevices).catch(() => {})
    const timer = setInterval(() => api.alerts().then(setAlerts).catch(() => {}), REFRESH_MS)
    return () => clearInterval(timer)
  }, [load])

  const hostname = (id: string) => devices.find((d) => d.id === id)?.hostname ?? id.slice(0, 8)
  const active = alerts.filter((a) => a.state !== "resolved")
  const recent = alerts.filter((a) => a.state === "resolved").slice(0, 20)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">Alerts</h1>
        <div className="flex flex-wrap gap-2">
          <NewChannelForm onCreated={load} />
          <NewRuleForm channels={channels} onCreated={load} />
        </div>
      </div>

      {notice && <p className="text-sm text-muted-foreground">{notice}</p>}

      <DeliveryHealth health={outbox} />

      <section>
        <h2 className="mb-2 flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <BellRing className="size-4" />
          Active ({active.length})
        </h2>
        {active.length === 0 ? (
          <div className="rounded-lg border border-dashed p-8 text-center text-sm text-muted-foreground">
            Nothing firing. Quiet is good.
          </div>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Severity</TableHead>
                <TableHead>Rule</TableHead>
                <TableHead>Device</TableHead>
                <TableHead>Value</TableHead>
                <TableHead>Open</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {active.map((a) => (
                <TableRow key={a.id}>
                  <TableCell>
                    <SeverityBadge severity={a.severity} />
                  </TableCell>
                  <TableCell className="font-medium">{String(a.context.rule ?? "")}</TableCell>
                  <TableCell>{hostname(a.device_id)}</TableCell>
                  <TableCell className="text-muted-foreground">
                    {a.last_value == null ? "-" : `${a.last_value.toFixed(1)}%`}
                  </TableCell>
                  <TableCell className="text-muted-foreground">{since(a.opened_at)}</TableCell>
                  <TableCell className="text-right">
                    {a.state === "firing" && (
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={async () => {
                          await api.ackAlert(a.id)
                          load()
                        }}
                      >
                        Ack
                      </Button>
                    )}
                    {a.state === "acknowledged" && (
                      <Badge variant="secondary" className="mr-2">
                        acked by {a.acked_by}
                      </Badge>
                    )}
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={async () => {
                        await api.resolveAlert(a.id)
                        load()
                      }}
                    >
                      <Check className="size-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </section>

      <section className="flex flex-wrap gap-6">
        <div className="min-w-72 flex-1">
          <h2 className="mb-2 text-sm font-medium text-muted-foreground">Rules ({rules.length})</h2>
          {rules.length === 0 ? (
            <p className="text-sm text-muted-foreground">No rules yet.</p>
          ) : (
            <div className="flex flex-col gap-2">
              {rules.map((r) => (
                <Card key={r.id}>
                  <CardContent className="flex items-center gap-3 py-3">
                    <SeverityBadge severity={r.severity} />
                    <div className="flex-1">
                      <p className="flex items-center gap-2 text-sm font-medium">
                        {r.name}
                        {/* An enabled rule with no channel is the quiet
                            failure: it looks configured, it evaluates, it
                            records, and nobody is told. The list is where an
                            operator would otherwise never notice. */}
                        {r.enabled && r.channel_ids.length === 0 && (
                          <Badge
                            variant="outline"
                            className="gap-1 border-amber-500/50 text-amber-700 dark:text-amber-400"
                          >
                            <TriangleAlert className="size-3" />
                            notifies nobody
                          </Badge>
                        )}
                      </p>
                      <p className="text-xs text-muted-foreground">
                        {r.metric === "heartbeat_missed"
                          ? "device stops checking in"
                          : `${r.metric} ${r.operator === "gt" ? ">" : "<"} ${r.threshold}%`}{" "}
                        for {r.duration_s}s · {r.channel_ids.length} channel(s)
                      </p>
                    </div>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={async () => {
                        await api.deleteRule(r.id)
                        load()
                      }}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </CardContent>
                </Card>
              ))}
            </div>
          )}
        </div>

        <div className="min-w-72 flex-1">
          <h2 className="mb-2 text-sm font-medium text-muted-foreground">
            Channels ({channels.length})
          </h2>
          {channels.length === 0 ? (
            <p className="text-sm text-muted-foreground">No channels yet.</p>
          ) : (
            <div className="flex flex-col gap-2">
              {channels.map((c) => (
                <Card key={c.id}>
                  <CardContent className="flex items-center gap-3 py-3">
                    <div className="flex-1">
                      <p className="text-sm font-medium">{c.name}</p>
                      <p className="text-xs text-muted-foreground">{c.kind}</p>
                    </div>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="gap-1.5"
                      onClick={async () => {
                        await api.testChannel(c.id)
                        setNotice(`Test notification queued for ${c.name}.`)
                      }}
                    >
                      <Send className="size-4" />
                      Test
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={async () => {
                        await api.deleteChannel(c.id)
                        load()
                      }}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </CardContent>
                </Card>
              ))}
            </div>
          )}
        </div>
      </section>

      {recent.length > 0 && (
        <section>
          <h2 className="mb-2 text-sm font-medium text-muted-foreground">Recently resolved</h2>
          <Table>
            <TableBody>
              {recent.map((a) => (
                <TableRow key={a.id}>
                  <TableCell>
                    <SeverityBadge severity={a.severity} />
                  </TableCell>
                  <TableCell>{String(a.context.rule ?? "")}</TableCell>
                  <TableCell>{hostname(a.device_id)}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    resolved {a.resolved_at ? since(a.resolved_at) : ""} ago
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
