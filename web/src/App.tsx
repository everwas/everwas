import { useEffect, useState } from "react"
import { Activity, LayoutGrid, MonitorSmartphone, ScrollText, ShieldCheck, TerminalSquare } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"

type Health = { status: string; version: string }

const nav = [
  { label: "Overview", icon: LayoutGrid },
  { label: "Devices", icon: MonitorSmartphone },
  { label: "Alerts", icon: Activity },
  { label: "Scripts", icon: TerminalSquare },
  { label: "Patches", icon: ShieldCheck },
  { label: "Audit", icon: ScrollText },
]

export default function App() {
  const [health, setHealth] = useState<Health | null>(null)
  const [error, setError] = useState(false)

  useEffect(() => {
    fetch("/api/v1/health")
      .then((r) => (r.ok ? r.json() : Promise.reject()))
      .then(setHealth)
      .catch(() => setError(true))
  }, [])

  return (
    <div className="flex min-h-svh bg-background text-foreground">
      <aside className="hidden w-56 shrink-0 flex-col border-r sm:flex">
        <div className="flex h-14 items-center gap-2 border-b px-4 font-semibold">
          <MonitorSmartphone className="size-5" />
          OpenRMM
        </div>
        <nav className="flex flex-col gap-1 p-2">
          {nav.map(({ label, icon: Icon }) => (
            <a
              key={label}
              href="#"
              className="flex items-center gap-2 rounded-md px-3 py-2 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
            >
              <Icon className="size-4" />
              {label}
            </a>
          ))}
        </nav>
      </aside>

      <main className="flex-1 p-6">
        <header className="mb-6 flex items-center justify-between">
          <h1 className="text-xl font-semibold">Overview</h1>
          {health ? (
            <Badge variant="outline" className="gap-1.5">
              <span className="size-2 rounded-full bg-emerald-500" />
              server {health.version}
            </Badge>
          ) : (
            <Badge variant="outline" className="gap-1.5">
              <span className={`size-2 rounded-full ${error ? "bg-red-500" : "bg-muted-foreground"}`} />
              {error ? "server unreachable" : "connecting"}
            </Badge>
          )}
        </header>

        <Card className="max-w-md">
          <CardHeader>
            <CardTitle>Milestone 0</CardTitle>
            <CardDescription>Skeleton stack is up.</CardDescription>
          </CardHeader>
          <CardContent className="text-sm text-muted-foreground">
            Next: agent enrollment and heartbeat (M1). Devices will appear here
            once agents check in.
          </CardContent>
        </Card>
      </main>
    </div>
  )
}
