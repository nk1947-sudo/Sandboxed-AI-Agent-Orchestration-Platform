import { getToken } from "./auth";

export interface VM {
  id: string;
  cid: number;
  pid: number;
  vcpus: number;
  mem_mib: number;
  created_at: string;
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

function authHeaders(): Record<string, string> {
  const tok = getToken();
  return tok ? { Authorization: `Bearer ${tok}` } : {};
}

async function request<T>(
  path: string,
  init?: RequestInit
): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...authHeaders(),
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

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
    this.name = "ApiError";
  }
}

// ── VMs ────────────────────────────────────────────────────────────────────

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

export function terminalWsUrl(sandboxId: string): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const tok = getToken();
  const auth = tok ? `&token=${encodeURIComponent(tok)}` : "";
  return `${proto}//${location.host}/terminal?sandbox=${encodeURIComponent(sandboxId)}${auth}`;
}
