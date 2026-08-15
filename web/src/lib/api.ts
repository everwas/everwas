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

export type ShellKind = "bash" | "sh" | "zsh" | "powershell" | "pwsh" | "cmd" | "python"

export type Script = {
  id: string
  name: string
  description: string
  shell: ShellKind
  body: string
  os_filter: string[]
  timeout_s: number
  sha256: string
  version: number
  updated_at: string
}

export type ScriptRun = {
  id: string
  script_id: string | null
  device_id: string
  run_batch_id: string | null
  trigger: string
  status: "queued" | "delivered" | "running" | "succeeded" | "failed" | "timeout" | "cancelled"
  exit_code: number | null
  stdout: string
  stderr: string
  truncated: boolean
  requested_by: string | null
  queued_at: string
  started_at: string | null
  finished_at: string | null
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

  scripts: () => request<Script[]>("/api/v1/scripts"),
  createScript: (body: Partial<Script>) =>
    request<Script>("/api/v1/scripts", { method: "POST", body: JSON.stringify(body) }),
  deleteScript: (id: string) => request<void>(`/api/v1/scripts/${id}`, { method: "DELETE" }),
  runScript: (id: string, target: { device_ids?: string[]; tags?: string[]; all?: boolean }) =>
    request<{ batch_id: string; queued: number; run_ids: string[] }>(
      `/api/v1/scripts/${id}/run`,
      { method: "POST", body: JSON.stringify(target) },
    ),
  runs: (deviceId?: string) =>
    request<ScriptRun[]>(
      `/api/v1/scripts/runs/recent${deviceId ? `?device_id=${deviceId}` : ""}`,
    ),
  run: (runId: string) => request<ScriptRun>(`/api/v1/scripts/runs/${runId}`),
}
