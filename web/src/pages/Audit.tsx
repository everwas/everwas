import { Fragment, useCallback, useEffect, useState } from "react"
import { Link } from "react-router-dom"
import { Bot, ChevronRight, KeyRound, Server, ScrollText, User } from "lucide-react"

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
 *  so it is rendered as key=value pairs rather than pretty-printed JSON.
 *  Null-valued keys are noise in a one-line summary (tag=null status=null on
 *  every list call) and are held back for the expanded row, as is anything
 *  past the first handful — the row expands for the rest. */
const COLLAPSED_PAIRS = 5

function fmtValue(v: unknown): string {
  const text = typeof v === "object" ? JSON.stringify(v) : String(v)
  return text.length > 48 ? `${text.slice(0, 48)}…` : text
}

function Detail({ detail }: { detail: Record<string, unknown> | null }) {
  if (!detail || Object.keys(detail).length === 0)
    return <span className="text-muted-foreground">—</span>
  const pairs = Object.entries(detail).filter(([, v]) => v !== null && v !== undefined)
  if (pairs.length === 0) return <span className="text-muted-foreground">—</span>
  const shown = pairs.slice(0, COLLAPSED_PAIRS)
  const hidden = pairs.length - shown.length
  return (
    <span className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 font-mono text-xs">
      {shown.map(([k, v]) => (
        <span key={k} className="whitespace-nowrap">
          <span className="text-muted-foreground">{k}=</span>
          <span className="text-foreground/90">{fmtValue(v)}</span>
        </span>
      ))}
      {hidden > 0 && <span className="text-muted-foreground">+{hidden} more</span>}
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
  // A failed load must not render as "Nothing matches those filters": an
  // empty log and an unreachable server look identical otherwise, and the
  // wrong lesson ("nothing happened") is exactly what an audit page must
  // never teach.
  const [failed, setFailed] = useState(false)
  const [open, setOpen] = useState<string | null>(null)

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
        setFailed(false)
      } catch {
        setFailed(true)
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
        setFailed(false)
      })
      .catch(() => {
        if (!cancelled) setFailed(true)
      })
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

      {failed ? (
        <div className="flex flex-col items-center gap-3 rounded-lg border border-dashed p-10 text-center text-sm text-muted-foreground">
          <span>The audit log could not be loaded. This is a connection problem, not an empty log.</span>
          <Button size="sm" variant="outline" disabled={loading} onClick={() => load(false)}>
            {loading ? "Retrying…" : "Retry"}
          </Button>
        </div>
      ) : entries.length === 0 ? (
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
                <Fragment key={e.id}>
                <TableRow
                  className="cursor-pointer"
                  onClick={() => setOpen(open === e.id ? null : e.id)}
                >
                  <TableCell className="whitespace-nowrap text-muted-foreground">
                    <span className="flex items-center gap-1">
                      <ChevronRight
                        className={`size-3.5 shrink-0 transition-transform ${open === e.id ? "rotate-90" : ""}`}
                      />
                      {new Date(e.at).toLocaleString()}
                    </span>
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
                {open === e.id && (
                  <TableRow className="bg-muted/30 hover:bg-muted/30">
                    <TableCell colSpan={5} className="py-3">
                      <div className="flex flex-col gap-2 pl-5">
                        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
                          <span className="whitespace-nowrap">{new Date(e.at).toISOString()}</span>
                          <span className="font-mono">entry {e.id}</span>
                          {e.target_id && (
                            <span className="font-mono">
                              {e.target_type ?? "target"} {e.target_id}
                            </span>
                          )}
                          {e.target_type === "device" && e.target_id && (
                            <Link
                              to={`/devices/${e.target_id}`}
                              onClick={(ev) => ev.stopPropagation()}
                              className="font-medium text-foreground underline-offset-4 hover:underline"
                            >
                              View device →
                            </Link>
                          )}
                        </div>
                        {e.detail && Object.keys(e.detail).length > 0 && (
                          <pre className="overflow-x-auto rounded-md bg-muted/50 p-3 font-mono text-xs leading-relaxed">
                            {JSON.stringify(e.detail, null, 2)}
                          </pre>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                )}
                </Fragment>
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
