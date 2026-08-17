import { useCallback, useEffect, useState } from "react"
import { Bot, KeyRound, Server, ScrollText, User } from "lucide-react"

import { api, type ActorType, type AuditEntry } from "@/lib/api"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
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

const PAGE = 50

const ACTOR_ICON: Record<ActorType, typeof User> = {
  user: User,
  api_key: KeyRound,
  agent: Bot,
  system: Server,
}

/** Actions that change a machine or who can reach one, as opposed to reads.
 *  Everything here is worth noticing in a wall of rows. */
const CONSEQUENTIAL = /^(device\.(retired|deleted|credentials_rotated)|agent\.updated|script\.(queued|executed)|patch\.install|policy\.violation)/

/** Device ids are UUIDv7, which is time-ordered: the leading characters are a
 *  timestamp and the entropy is at the END. Truncating to a PREFIX therefore
 *  renders every device enrolled in the same millisecond identically, which is
 *  exactly what a bulk operation produces. Shortening from the right keeps
 *  them distinguishable. */
function shortId(id: string | null): string {
  if (!id) return "n/a"
  return id.length > 12 ? `…${id.slice(-12)}` : id
}

function ActorCell({ entry }: { entry: AuditEntry }) {
  const Icon = ACTOR_ICON[entry.actor_type] ?? Server
  const who = entry.actor_type === "agent" ? shortId(entry.actor_id) : entry.actor_id || "n/a"
  return (
    <span className="flex items-center gap-1.5">
      <Icon className="size-3.5 shrink-0 text-muted-foreground" />
      <span className="truncate">{who}</span>
    </span>
  )
}

/** The detail blob is where the useful specifics live (which updates
 *  installed, which host was retired) and it is different for every action,
 *  so it is rendered as compact key=value rather than pretty-printed JSON. */
function Detail({ detail }: { detail: Record<string, unknown> | null }) {
  if (!detail || Object.keys(detail).length === 0) return <span className="text-muted-foreground">n/a</span>
  const text = Object.entries(detail)
    .map(([k, v]) => `${k}=${typeof v === "object" ? JSON.stringify(v) : String(v)}`)
    .join("  ")
  return (
    <span className="font-mono text-xs text-muted-foreground" title={JSON.stringify(detail, null, 2)}>
      {text.length > 140 ? `${text.slice(0, 140)}…` : text}
    </span>
  )
}

export function AuditPage() {
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [actions, setActions] = useState<string[]>([])
  const [action, setAction] = useState("")
  const [actor, setActor] = useState("")
  const [hours, setHours] = useState<number | "">("")
  const [nextBefore, setNextBefore] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  const load = useCallback(
    async (append = false) => {
      setLoading(true)
      try {
        const page = await api.audit({
          action: action || undefined,
          actor: actor || undefined,
          hours: hours === "" ? undefined : hours,
          before: append ? (nextBefore ?? undefined) : undefined,
          limit: PAGE,
        })
        setEntries((prev) => (append ? [...prev, ...page.entries] : page.entries))
        setNextBefore(page.has_more ? page.next_before : null)
      } finally {
        setLoading(false)
      }
    },
    [action, actor, hours, nextBefore],
  )

  // Filters re-query from the top; paging is explicit.
  useEffect(() => {
    let cancelled = false
    api
      .audit({ action: action || undefined, actor: actor || undefined, hours: hours === "" ? undefined : hours, limit: PAGE })
      .then((page) => {
        if (cancelled) return
        setEntries(page.entries)
        setNextBefore(page.has_more ? page.next_before : null)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [action, actor, hours])

  useEffect(() => {
    api.auditActions().then(setActions).catch(() => {})
  }, [])

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <h1 className="flex items-center gap-2 text-xl font-semibold">
          <ScrollText className="size-5" />
          Audit
        </h1>
        <div className="flex flex-wrap items-end gap-3">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="au-action">Action</Label>
            <select
              id="au-action"
              value={action}
              onChange={(e) => setAction(e.target.value)}
              className="h-9 rounded-md border bg-transparent px-2 text-sm"
            >
              {/* From the data, not typed: a typo returns an empty page that
                  looks exactly like nothing having happened. */}
              <option value="">all actions</option>
              {actions.map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="au-actor">Actor</Label>
            <Input
              id="au-actor"
              placeholder="someone@example.com"
              value={actor}
              onChange={(e) => setActor(e.target.value)}
              className="w-56"
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="au-hours">Window</Label>
            <select
              id="au-hours"
              value={hours}
              onChange={(e) => setHours(e.target.value === "" ? "" : Number(e.target.value))}
              className="h-9 rounded-md border bg-transparent px-2 text-sm"
            >
              <option value="">all time</option>
              <option value={1}>last hour</option>
              <option value={24}>last 24h</option>
              <option value={24 * 7}>last 7 days</option>
              <option value={24 * 30}>last 30 days</option>
            </select>
          </div>
        </div>
      </div>

      {entries.length === 0 ? (
        <div className="rounded-lg border border-dashed p-10 text-center text-sm text-muted-foreground">
          Nothing matches those filters.
        </div>
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-44">When</TableHead>
                <TableHead className="w-44">Actor</TableHead>
                <TableHead className="w-56">Action</TableHead>
                <TableHead className="w-40">Target</TableHead>
                <TableHead>Detail</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((e) => (
                <TableRow key={e.id}>
                  <TableCell className="whitespace-nowrap text-muted-foreground">
                    {new Date(e.at).toLocaleString()}
                  </TableCell>
                  <TableCell className="max-w-44">
                    <ActorCell entry={e} />
                  </TableCell>
                  <TableCell>
                    <Badge
                      variant="outline"
                      className={
                        CONSEQUENTIAL.test(e.action)
                          ? "border-amber-500/50 font-mono text-xs text-amber-700 dark:text-amber-400"
                          : "font-mono text-xs text-muted-foreground"
                      }
                    >
                      {e.action}
                    </Badge>
                  </TableCell>
                  <TableCell
                    className="whitespace-nowrap font-mono text-xs text-muted-foreground"
                    title={e.target_id ?? undefined}
                  >
                    {/* The hostname if the entry recorded one. A uuid tells an
                        operator nothing, and for a deleted device it is the
                        only name that survives. */}
                    {typeof e.detail?.hostname === "string"
                      ? e.detail.hostname
                      : shortId(e.target_id)}
                  </TableCell>
                  <TableCell className="max-w-0">
                    <Detail detail={e.detail} />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>

          {nextBefore && (
            <div className="flex justify-center">
              <Button size="sm" variant="outline" disabled={loading} onClick={() => load(true)}>
                {loading ? "Loading…" : "Load older"}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  )
}
