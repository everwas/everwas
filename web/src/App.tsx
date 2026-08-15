import { useEffect, useState } from "react"
import { NavLink, Route, Routes } from "react-router-dom"
import {
  Activity,
  LayoutGrid,
  LogOut,
  MonitorSmartphone,
  ScrollText,
  ShieldCheck,
  TerminalSquare,
} from "lucide-react"

import { api, type User } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { DeviceDetailPage } from "@/pages/DeviceDetail"
import { DevicesPage } from "@/pages/Devices"
import { LoginPage } from "@/pages/Login"
import { ScriptsPage } from "@/pages/Scripts"

const nav = [
  { label: "Overview", icon: LayoutGrid, to: null },
  { label: "Devices", icon: MonitorSmartphone, to: "/" },
  { label: "Alerts", icon: Activity, to: null },
  { label: "Scripts", icon: TerminalSquare, to: "/scripts" },
  { label: "Patches", icon: ShieldCheck, to: null },
  { label: "Audit", icon: ScrollText, to: null },
]

export default function App() {
  const [user, setUser] = useState<User | null>(null)
  const [checked, setChecked] = useState(false)

  useEffect(() => {
    api
      .me()
      .then(setUser)
      .catch(() => {})
      .finally(() => setChecked(true))
  }, [])

  if (!checked) return null
  if (!user) return <LoginPage onLogin={setUser} />

  return (
    <div className="flex min-h-svh bg-background text-foreground">
      <aside className="hidden w-56 shrink-0 flex-col border-r sm:flex">
        <div className="flex h-14 items-center gap-2 border-b px-4 font-semibold">
          <MonitorSmartphone className="size-5" />
          OpenRMM
        </div>
        <nav className="flex flex-1 flex-col gap-1 p-2">
          {nav.map(({ label, icon: Icon, to }) =>
            to ? (
              <NavLink
                key={label}
                to={to}
                end={to === "/"}
                className={({ isActive }) =>
                  `flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${
                    isActive
                      ? "bg-accent font-medium text-accent-foreground"
                      : "text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                  }`
                }
              >
                <Icon className="size-4" />
                {label}
              </NavLink>
            ) : (
              <span
                key={label}
                className="flex cursor-default items-center gap-2 rounded-md px-3 py-2 text-sm text-muted-foreground/50"
              >
                <Icon className="size-4" />
                {label}
              </span>
            ),
          )}
        </nav>
        <div className="border-t p-2">
          <Button
            variant="ghost"
            size="sm"
            className="w-full justify-start gap-2 text-muted-foreground"
            onClick={() => api.logout().then(() => setUser(null))}
          >
            <LogOut className="size-4" />
            {user.email}
          </Button>
        </div>
      </aside>

      <main className="flex-1 p-6">
        <Routes>
          <Route
            index
            element={
              <>
                <header className="mb-6 flex items-center justify-between">
                  <h1 className="text-xl font-semibold">Devices</h1>
                </header>
                <DevicesPage />
              </>
            }
          />
          <Route path="/devices/:id" element={<DeviceDetailPage />} />
          <Route path="/scripts" element={<ScriptsPage />} />
        </Routes>
      </main>
    </div>
  )
}
