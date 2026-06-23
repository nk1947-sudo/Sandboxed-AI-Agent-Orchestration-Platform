// API client. Authentication is via an HttpOnly session cookie set by the
// server at login — the API token is never stored in the browser. Every request
// includes credentials so the cookie rides along; the WebSocket handshake is
// same-origin and carries the cookie automatically.

export interface User {
  id: string;
  username: string;
  role: string;
}

export interface VM {
  id: string;
  cid: number;
  pid: number;
  vcpus: number;
  mem_mib: number;
  created_at: string;
}

export interface Sandbox {
  id: string;
  owner_id: string;
  name: string;
  status: "running" | "stopped" | "archived";
  vcpus: number;
  mem_mib: number;
  cpu_percent: number;
  pids_max: number;
  cid: number;
  created_at: string;
  stopped_at?: string;
  last_used_at: string;
}

export interface ApprovalRecord {
  id: string;
  sandbox_id: string;
  script: string;
  reason: string;
  state: "pending" | "approved" | "rejected";
  submitted_at: string;
  decided_by?: string;
  decided_at?: string;
}

export interface TranscriptLine {
  seq: number;
  ts: string;
  kind: "input" | "output" | "exit" | "hitl" | "system";
  data?: string;
  exit_code?: number;
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
    this.name = "ApiError";
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: "include", // send the session cookie
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...init?.headers,
    },
  });
  if (res.status === 204) return undefined as T;
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw new ApiError(res.status, body.error ?? res.statusText);
  }
  return res.json() as Promise<T>;
}

// ── Auth ─────────────────────────────────────────────────────────────────────

export async function me(): Promise<User> {
  return request<User>("/api/me");
}

export async function login(username: string, password: string): Promise<User> {
  return request<User>("/api/login", {
    method: "POST",
    body: JSON.stringify({ username, password }),
  });
}

export async function logout(): Promise<void> {
  return request<void>("/api/logout", { method: "POST" });
}

// ── Live VMs ─────────────────────────────────────────────────────────────────

export async function listVMs(): Promise<VM[]> {
  const data = await request<{ vms: VM[] }>("/api/vms");
  return data.vms ?? [];
}

export interface LaunchParams {
  id?: string;
  vcpus?: number;
  mem_mib?: number;
  cpu_percent?: number;
  pids_max?: number;
}

export async function launchVM(params: LaunchParams): Promise<VM> {
  return request<VM>("/api/vms", {
    method: "POST",
    body: JSON.stringify(params),
  });
}

export async function terminateVM(id: string): Promise<void> {
  return request<void>(`/api/vms/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

// ── Sandbox history + resume ─────────────────────────────────────────────────

export async function listSandboxes(): Promise<Sandbox[]> {
  const data = await request<{ sandboxes: Sandbox[] }>("/api/sandboxes");
  return data.sandboxes ?? [];
}

export async function stopSandbox(id: string): Promise<void> {
  await request<unknown>(`/api/sandboxes/${encodeURIComponent(id)}/stop`, {
    method: "POST",
  });
}

export async function resumeSandbox(id: string): Promise<VM> {
  return request<VM>(`/api/sandboxes/${encodeURIComponent(id)}/resume`, {
    method: "POST",
  });
}

export async function renameSandbox(id: string, name: string): Promise<void> {
  await request<void>(`/api/sandboxes/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify({ name }),
  });
}

export async function deleteSandbox(id: string): Promise<void> {
  await request<void>(`/api/sandboxes/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export async function getTranscript(id: string): Promise<TranscriptLine[]> {
  const data = await request<{ lines: TranscriptLine[] }>(
    `/api/sandboxes/${encodeURIComponent(id)}/transcript`
  );
  return data.lines ?? [];
}

// ── Approvals ──────────────────────────────────────────────────────────────

export async function listApprovals(): Promise<ApprovalRecord[]> {
  const data = await request<{ approvals: ApprovalRecord[] }>("/api/approvals");
  return data.approvals ?? [];
}

export async function approveScript(id: string): Promise<void> {
  return request<void>(`/api/approvals/${encodeURIComponent(id)}`, {
    method: "POST",
    body: JSON.stringify({ decision: "approve" }),
  });
}

export async function rejectScript(id: string): Promise<void> {
  return request<void>(`/api/approvals/${encodeURIComponent(id)}`, {
    method: "POST",
    body: JSON.stringify({ decision: "reject" }),
  });
}

// ── WebSocket URL helper ────────────────────────────────────────────────────

// The session cookie authenticates the WS handshake (same-origin), so no token
// appears in the URL.
export function terminalWsUrl(sandboxId: string): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/terminal?sandbox=${encodeURIComponent(sandboxId)}`;
}
