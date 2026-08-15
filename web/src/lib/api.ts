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
}
