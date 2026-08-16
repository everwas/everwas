import { useEffect, useState } from "react"
import { Link } from "react-router-dom"
import {
  Activity,
  BellRing,
  MailWarning,
  ScrollText,
  ServerCrash,
  ShieldAlert,
  TriangleAlert,
} from "lucide-react"

import {
  api,
  type Alert,
  type AuditEntry,
  type Device,
  type OutboxHealth,
} from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent } from "@/components/ui/card"

const REFRESH_MS = 10_000

/** One number and what it means, linking to where you act on it. */
function Stat({
  label,
  value,
  detail,
  to,
  icon: Icon,
  alarming,
}: {
  label: string
  value: number | string
  detail: string
  to: string
  icon: typeof Activity
  alarming?: boolean
}) {
  return (
    <Link to={to} className="block">
      <Card
        className={
          alarming
            ? "border-amber-500/50 bg-amber-500/5 transition-colors hover:bg-amber-500/10"
            : "transition-colors hover:bg-accent/40"
        }
      >
        <CardContent className="flex items-start gap-3 py-4">
          <Icon
            className={`mt-0.5 size-5 ${alarming ? "text-amber-600 dark:text-amber-500" : "text-muted-foreground"}`}
          />
          <div className="flex flex-col gap-0.5">
            <span className="text-sm text-muted-foreground">{label}</span>
            <span className="text-2xl font-semibold tabular-nums">{value}</span>
            <span className="text-xs text-muted-foreground">{detail}</span>
          </div>
        </CardContent>
      </Card>
    </Link>
  )
}

export function OverviewPage() {
  const [devices, setDevices] = useState<Device[]>([])
  const [alerts, setAlerts] = useState<Alert[]>([])
  const [outbox, setOutbox] = useState<OutboxHealth | null>(null)
  const [recent, setRecent] = useState<AuditEntry[]>([])

  useEffect(() => {
    const load = () => {
      api.devices().then(setDevices).catch(() => {})
      api.alerts().then(setAlerts).catch(() => {})
      api.outboxHealth().then(setOutbox).catch(() => {})
      // The consequential things only: a wall of every read is not a summary.
      api
        .audit({ limit: 8 })
        .then((p) => setRecent(p.entries))
        .catch(() => {})
    }
    load()
    const timer = setInterval(load, REFRESH_MS)
    return () => clearInterval(timer)
  }, [])

  const offline = devices.filter((d) => d.status === "offline")
  const firing = alerts.filter((a) => a.state === "firing")
  const acked = alerts.filter((a) => a.state === "acknowledged")
  // Blocked is not failed: these are waiting on a channel somebody has to
  // repair, and they deliver the moment it is fixed.
  const undelivered = (outbox?.pending ?? 0) + (outbox?.blocked ?? 0)

  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-xl font-semibold">Overview</h1>

      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <Stat
          label="Devices"
          value={devices.length}
          detail={`${devices.length - offline.length} online`}
          to="/"
          icon={Activity}
        />
        <Stat
          label="Offline"
          value={offline.length}
          detail={
            offline.length
              ? offline
                  .slice(0, 3)
                  .map((d) => d.hostname)
                  .join(", ") + (offline.length > 3 ? "…" : "")
              : "every agent is checking in"
          }
          to="/"
          icon={ServerCrash}
          alarming={offline.length > 0}
        />
        <Stat
          label="Firing alerts"
          value={firing.length}
          detail={acked.length ? `${acked.length} acknowledged` : "nothing unacknowledged"}
          to="/alerts"
          icon={BellRing}
          alarming={firing.length > 0}
        />
        <Stat
          label="Undelivered notifications"
          value={undelivered}
          detail={
            outbox?.problems?.length
              ? outbox.problems[0]
              : "everything queued has been delivered"
          }
          to="/alerts"
          icon={MailWarning}
          alarming={undelivered > 0}
        />
      </div>

      {/* Problems the numbers above cannot express, stated as sentences.
          An alert that fires and is never delivered looks identical to one
          that paged someone, so it has to be said out loud somewhere. */}
      {(outbox?.problems?.length ?? 0) > 0 && (
        <Card className="border-amber-500/50 bg-amber-500/5">
          <CardContent className="flex flex-col gap-1.5 py-4">
            <h2 className="flex items-center gap-2 text-sm font-medium">
              <ShieldAlert className="size-4 text-amber-600 dark:text-amber-500" />
              Needs attention
            </h2>
            {outbox!.problems.map((p) => (
              <p key={p} className="text-sm text-amber-700 dark:text-amber-400">
                {p}
              </p>
            ))}
          </CardContent>
        </Card>
      )}

      <section className="flex flex-col gap-2">
        <h2 className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <BellRing className="size-4" />
          Firing now
        </h2>
        {firing.length === 0 ? (
          <div className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
            Nothing firing. Quiet is good.
          </div>
        ) : (
          <div className="flex flex-col gap-2">
            {firing.slice(0, 5).map((a) => (
              <Link key={a.id} to="/alerts">
                <Card className="transition-colors hover:bg-accent/40">
                  <CardContent className="flex items-center gap-3 py-3">
                    <TriangleAlert className="size-4 text-amber-600 dark:text-amber-500" />
                    <Badge variant="outline" className="capitalize">
                      {a.severity}
                    </Badge>
                    <span className="flex-1 text-sm">
                      {devices.find((d) => d.id === a.device_id)?.hostname ?? a.device_id.slice(-12)}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      since {new Date(a.opened_at).toLocaleTimeString()}
                    </span>
                  </CardContent>
                </Card>
              </Link>
            ))}
          </div>
        )}
      </section>

      <section className="flex flex-col gap-2">
        <h2 className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <ScrollText className="size-4" />
          Latest activity
        </h2>
        <div className="flex flex-col gap-1">
          {recent.map((e) => (
            <div key={e.id} className="flex items-baseline gap-3 text-sm">
              <span className="w-36 shrink-0 text-xs text-muted-foreground">
                {new Date(e.at).toLocaleTimeString()}
              </span>
              <span className="w-52 shrink-0 truncate font-mono text-xs text-muted-foreground">
                {e.action}
              </span>
              <span className="truncate text-xs text-muted-foreground">
                {typeof e.detail?.hostname === "string" ? e.detail.hostname : (e.actor_id ?? "")}
              </span>
            </div>
          ))}
          <Link to="/audit" className="pt-1 text-xs text-muted-foreground underline">
            Full audit log
          </Link>
        </div>
      </section>
    </div>
  )
}
