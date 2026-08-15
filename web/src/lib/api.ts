export type Device = {
  id: string
  hostname: string
  os_family: "windows" | "macos" | "linux"
  os_version: string
  arch: string
  agent_version: string
  status: "enrolled" | "active" | "offline" | "retired"
  tags: string[]
  last_heartbeat_at: string | null
  enrolled_at: string
}

export type DeviceDetail = Device & {
  cpu_pct: number | null
  mem_pct: number | null
  worst_disk_pct: number | null
}

export type TelemetryPoint = {
  ts: string
  cpu_pct: number | null
  mem_pct: number | null
  load1: number | null
}

export type Fact = {
  fact_key: string
  payload: Record<string, unknown>
  valid_from: string | null
  valid_to: string | null
  source: string
}

export type FactKind = "hardware" | "software" | "patchstate"
export type SnapshotKind = "processes" | "services"

export type User = { id: string; email: string; role: string }

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    ...init,
  })
  if (!res.ok) throw new ApiError(res.status)
  return res.status === 204 ? (undefined as T) : res.json()
}

export class ApiError extends Error {
  status: number
  constructor(status: number) {
    super(`API error ${status}`)
    this.status = status
  }
}

export const api = {
  me: () => request<User>("/api/v1/auth/me"),
  login: (email: string, password: string) =>
    request<User>("/api/v1/auth/login", {
      method: "POST",
      body: JSON.stringify({ email, password }),
    }),
  logout: () => request<void>("/api/v1/auth/logout", { method: "POST" }),
  devices: () => request<Device[]>("/api/v1/devices"),
  device: (id: string) => request<DeviceDetail>(`/api/v1/devices/${id}`),
  telemetry: (id: string, hours = 24) =>
    request<TelemetryPoint[]>(`/api/v1/devices/${id}/telemetry?hours=${hours}`),
  facts: (id: string, kind: FactKind, asOf?: Date) => {
    const params = new URLSearchParams({ kind })
    if (asOf) params.set("as_of", asOf.toISOString())
    return request<Fact[]>(`/api/v1/devices/${id}/facts?${params}`)
  },
  snapshot: (id: string, kind: SnapshotKind) =>
    request<{ payload: Record<string, unknown> | null; updated_at: string | null }>(
      `/api/v1/devices/${id}/snapshots/${kind}`,
    ),
}
