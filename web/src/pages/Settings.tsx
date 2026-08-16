import { useCallback, useEffect, useState } from "react"
import { Building2, Copy, KeyRound, Plus, Trash2, TriangleAlert, Users } from "lucide-react"

import { api, type ApiKeyRow, type SiteRow, type UserRow } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
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

const ROLES = ["admin", "operator", "viewer"] as const

/** Shows an error the server sent, verbatim.
 *
 * Every refusal on this surface is a sentence explaining what to do instead
 * ("this is the last active admin; promote somebody else first"). Replacing
 * that with a generic message throws away the only useful part.
 */
function Refusal({ error }: { error: string | null }) {
  if (!error) return null
  return (
    <p className="flex items-start gap-2 text-sm text-destructive">
      <TriangleAlert className="mt-0.5 size-4 shrink-0" />
      {error}
    </p>
  )
}

function useAdminAction(reload: () => void) {
  const [error, setError] = useState<string | null>(null)
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      setError(null)
      try {
        await fn()
        reload()
        return true
      } catch (e) {
        setError(e instanceof Error ? e.message : "that did not work")
        return false
      }
    },
    [reload],
  )
  return { error, setError, run }
}

function UsersSection() {
  const [users, setUsers] = useState<UserRow[]>([])
  const load = useCallback(() => {
    api.users().then(setUsers).catch(() => {})
  }, [])
  useEffect(load, [load])

  const { error, run } = useAdminAction(load)
  const [open, setOpen] = useState(false)
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [role, setRole] = useState<string>("viewer")

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h2 className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <Users className="size-4" />
          Users ({users.length})
        </h2>
        <Button size="sm" variant="outline" className="gap-1.5" onClick={() => setOpen(!open)}>
          <Plus className="size-4" />
          New user
        </Button>
      </div>

      {open && (
        <Card>
          <CardContent className="flex flex-wrap items-end gap-3 py-4">
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="u-email">Email</Label>
              <Input id="u-email" value={email} onChange={(e) => setEmail(e.target.value)} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="u-pass">Password</Label>
              <Input
                id="u-pass"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                className="w-56"
              />
              <span className="text-xs text-muted-foreground">at least 12 characters</span>
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="u-role">Role</Label>
              <select
                id="u-role"
                value={role}
                onChange={(e) => setRole(e.target.value)}
                className="h-9 rounded-md border bg-transparent px-2 text-sm"
              >
                {ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            </div>
            <Button
              size="sm"
              disabled={!email || password.length < 12}
              onClick={async () => {
                if (await run(() => api.createUser({ email, password, role }))) {
                  setEmail("")
                  setPassword("")
                  setOpen(false)
                }
              }}
            >
              Create
            </Button>
          </CardContent>
        </Card>
      )}

      <Refusal error={error} />

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Email</TableHead>
            <TableHead className="w-40">Role</TableHead>
            <TableHead className="w-28">Status</TableHead>
            <TableHead className="w-32 text-right" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {users.map((u) => (
            <TableRow key={u.id}>
              <TableCell className="font-medium">{u.email}</TableCell>
              <TableCell>
                <select
                  value={u.role}
                  onChange={(e) => run(() => api.setUserRole(u.id, e.target.value))}
                  className="h-8 rounded-md border bg-transparent px-2 text-sm"
                >
                  {ROLES.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
              </TableCell>
              <TableCell>
                {u.is_active ? (
                  <Badge variant="outline">active</Badge>
                ) : (
                  <Badge variant="outline" className="text-muted-foreground">
                    disabled
                  </Badge>
                )}
              </TableCell>
              <TableCell className="text-right">
                {/* Disable, never delete: a deleted user's name vanishes from
                    every audit entry they ever produced. */}
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => run(() => api.setUserActive(u.id, !u.is_active))}
                >
                  {u.is_active ? "Disable" : "Enable"}
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </section>
  )
}

function ApiKeysSection() {
  const [keys, setKeys] = useState<ApiKeyRow[]>([])
  const [scopes, setScopes] = useState<string[]>([])
  const load = useCallback(() => {
    api.apiKeys().then(setKeys).catch(() => {})
  }, [])
  useEffect(load, [load])
  useEffect(() => {
    api.apiKeyScopes().then(setScopes).catch(() => {})
  }, [])

  const { error, run } = useAdminAction(load)
  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [chosen, setChosen] = useState<string[]>([])
  const [ttl, setTtl] = useState(365)
  // Shown once and never again: only sha256(secret) is stored.
  const [minted, setMinted] = useState<string | null>(null)

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h2 className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <KeyRound className="size-4" />
          API keys ({keys.length})
        </h2>
        <Button size="sm" variant="outline" className="gap-1.5" onClick={() => setOpen(!open)}>
          <Plus className="size-4" />
          New key
        </Button>
      </div>

      {minted && (
        <Card className="border-amber-500/50 bg-amber-500/5">
          <CardContent className="flex flex-col gap-2 py-4">
            <p className="text-sm font-medium">
              Copy this now. It is not stored and cannot be shown again.
            </p>
            <div className="flex items-center gap-2">
              <code className="flex-1 overflow-x-auto rounded bg-muted p-2 font-mono text-xs">
                {minted}
              </code>
              <Button
                size="sm"
                variant="outline"
                className="gap-1.5"
                onClick={() => navigator.clipboard.writeText(minted)}
              >
                <Copy className="size-4" />
                Copy
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setMinted(null)}>
                Done
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {open && (
        <Card>
          <CardContent className="flex flex-col gap-3 py-4">
            <div className="flex flex-wrap items-end gap-3">
              <div className="flex flex-1 flex-col gap-1.5">
                <Label htmlFor="k-name">Name</Label>
                <Input id="k-name" value={name} onChange={(e) => setName(e.target.value)} />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="k-ttl">Expires in (days)</Label>
                <Input
                  id="k-ttl"
                  type="number"
                  value={ttl}
                  onChange={(e) => setTtl(Number(e.target.value))}
                  className="w-32"
                />
              </div>
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>Scopes</Label>
              {/* Offered from the server's list rather than typed: a typo in a
                  scope mints a key that looks privileged and can do nothing. */}
              <div className="flex flex-wrap gap-3">
                {scopes.map((s) => (
                  <label key={s} className="flex items-center gap-1.5 font-mono text-xs">
                    <input
                      type="checkbox"
                      checked={chosen.includes(s)}
                      onChange={(e) =>
                        setChosen((prev) =>
                          e.target.checked ? [...prev, s] : prev.filter((x) => x !== s),
                        )
                      }
                    />
                    {s}
                  </label>
                ))}
              </div>
            </div>
            <div>
              <Button
                size="sm"
                disabled={!name || chosen.length === 0}
                onClick={async () => {
                  try {
                    const res = await api.createApiKey({
                      name,
                      scopes: chosen,
                      ttl_days: ttl || null,
                    })
                    setMinted(res.secret)
                    setName("")
                    setChosen([])
                    setOpen(false)
                    load()
                  } catch {
                    run(() => Promise.reject(new Error("could not create the key")))
                  }
                }}
              >
                Create
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      <Refusal error={error} />

      {keys.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No API keys. The MCP server and any automation authenticate with one.
        </p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead className="w-56">Key id</TableHead>
              <TableHead>Scopes</TableHead>
              <TableHead className="w-40">Last used</TableHead>
              <TableHead className="w-24 text-right" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.map((k) => (
              <TableRow key={k.id}>
                <TableCell className="font-medium">{k.name}</TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{k.key_id}</TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">
                  {k.scopes.join(" ")}
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">
                  {/* Never-used is worth seeing: it is either a key nobody
                      wired up or one that was replaced and forgotten. */}
                  {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : "never"}
                </TableCell>
                <TableCell className="text-right">
                  <Button size="sm" variant="ghost" onClick={() => run(() => api.revokeApiKey(k.id))}>
                    <Trash2 className="size-4" />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}

function SitesSection() {
  const [sites, setSites] = useState<SiteRow[]>([])
  const load = useCallback(() => {
    api.sites().then(setSites).catch(() => {})
  }, [])
  useEffect(load, [load])

  const { error, run } = useAdminAction(load)
  const [name, setName] = useState("")

  return (
    <section className="flex flex-col gap-3">
      <h2 className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
        <Building2 className="size-4" />
        Sites ({sites.length})
      </h2>
      <p className="text-sm text-muted-foreground">
        A site groups devices, and an enrollment token can pin new agents to one.
      </p>

      <div className="flex items-end gap-2">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="s-name">Name</Label>
          <Input
            id="s-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="w-64"
          />
        </div>
        <Button
          size="sm"
          disabled={!name}
          onClick={async () => {
            if (await run(() => api.createSite(name))) setName("")
          }}
        >
          Add site
        </Button>
      </div>

      <Refusal error={error} />

      {sites.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {sites.map((s) => (
            <Badge key={s.id} variant="outline" className="gap-2 py-1.5 pl-3 pr-1.5">
              {s.name}
              <button
                className="rounded p-0.5 hover:bg-accent"
                onClick={() => run(() => api.deleteSite(s.id))}
                aria-label={`Delete ${s.name}`}
              >
                <Trash2 className="size-3.5" />
              </button>
            </Badge>
          ))}
        </div>
      )}
    </section>
  )
}

export function SettingsPage() {
  return (
    <div className="flex flex-col gap-8">
      <h1 className="text-xl font-semibold">Settings</h1>
      <UsersSection />
      <ApiKeysSection />
      <SitesSection />
    </div>
  )
}
