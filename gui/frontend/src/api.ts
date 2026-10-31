// Typed access to the shell and, through it, to the backend's read methods.
// Every string that came from a person or a remote system is kept as
// received here and made safe in the views (visible() + textContent).

import type { Transport } from "./transport";

export type BackendState = "starting" | "running" | "stopped" | "needs-setup" | "update-needed";

export interface BackendStatus {
  state: BackendState;
  code?: string;
  reason?: string;
  /** "this computer", "Windows" or "Ubuntu (WSL)". */
  target?: string;
  distro?: string;
  os: string;
  client_version?: string;
  hello?: unknown;
}

export const INITIAL_STATUS: BackendStatus = { state: "starting", os: "" };

/** A failed call. `stopped` is true when the backend is gone. */
export class CallError extends Error {
  constructor(message: string, readonly stopped = false) {
    super(message);
    this.name = "CallError";
  }
}

function asError(e: unknown): CallError {
  const message = e instanceof Error ? e.message : typeof e === "string" ? e : JSON.stringify(e);
  return new CallError(message, /backend stopped/i.test(message));
}

function parse(raw: unknown): unknown {
  if (typeof raw !== "string") return raw;
  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}

const rec = (v: unknown): Record<string, unknown> => (v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {});
const str = (v: unknown): string => (typeof v === "string" ? v : "");

// ---- sessions.list

export interface SessionRow {
  id: string;
  name: string;
  owner: string;
  host: string;
  agent: string;
  status: string;
  presence: string;
  stale: boolean;
  /** The claimed issue, e.g. "ATL-142", with its state when not confirmed. */
  issue: string;
  updatedAt: string;
}

/** Reads the `sessions` array of a sessions.list result. */
export function parseSessions(raw: unknown): SessionRow[] {
  const rows = rec(parse(raw)).sessions;
  if (!Array.isArray(rows)) return [];
  return rows.map((r) => {
    const row = rec(r);
    const a = rec(row.assignment);
    const state = str(a.state);
    const key = str(a.issue_identifier) || str(a.issue_uuid);
    return {
      id: str(row.id),
      name: str(row.name),
      owner: str(row.owner_principal),
      host: str(row.host),
      agent: str(row.agent_kind),
      status: str(row.status),
      presence: str(row.presence),
      stale: row.stale === true,
      issue: key && state && state !== "confirmed" ? `${key} (${state})` : key,
      updatedAt: str(row.updated_at),
    };
  });
}

// ---- doctor.checks

export type CheckLevel = "pass" | "warn" | "fail" | "info";

export interface DoctorCheck {
  id: string;
  title: string;
  level: CheckLevel;
  message: string;
  path: string;
  action: string;
}

export interface DoctorReport {
  status: string;
  generatedAt: string;
  checks: DoctorCheck[];
}

const LEVELS: readonly string[] = ["pass", "warn", "fail", "info"];

export function parseDoctor(raw: unknown): DoctorReport {
  const r = rec(parse(raw));
  const list = Array.isArray(r.checks) ? r.checks : [];
  return {
    status: str(r.status),
    generatedAt: str(r.generated_at),
    checks: list.map((c) => {
      const x = rec(c);
      const level = str(x.status);
      return {
        id: str(x.id),
        title: str(x.title),
        // An unknown status is shown as a note, never as a pass.
        level: (LEVELS.includes(level) ? level : "info") as CheckLevel,
        message: str(x.message),
        path: str(x.path),
        action: str(x.action),
      };
    }),
  };
}

// ---- the api

export class Api {
  constructor(private readonly t: Transport) {}

  private async shell<T>(method: string, ...args: unknown[]): Promise<T> {
    try {
      return (await this.t.call(method, ...args)) as T;
    } catch (e) {
      throw asError(e);
    }
  }

  status(): Promise<BackendStatus> {
    return this.shell<BackendStatus>("Status");
  }
  restart(): Promise<void> {
    return this.shell<void>("Restart");
  }
  async logs(): Promise<string[]> {
    return (await this.shell<string[] | null>("Logs")) ?? [];
  }
  async wslDistros(): Promise<string[]> {
    return (await this.shell<string[] | null>("WSLDistros")) ?? [];
  }
  setWslDistro(name: string): Promise<void> {
    return this.shell<void>("SetWSLDistro", name);
  }
  chooseClient(): Promise<string> {
    return this.shell<string>("ChooseClient");
  }

  /** Tells the shell the webview blocked something (CSP, Trusted Types). */
  reportViolation(text: string): Promise<void> {
    return this.shell<void>("ReportViolation", text);
  }
  /** Tells the shell the first render is in the DOM. */
  ready(rendered: boolean): Promise<void> {
    return this.shell<void>("Ready", rendered);
  }

  /** One backend read. The shell refuses anything off its allow-list. */
  async rpc(method: string, params: Record<string, unknown> = {}): Promise<unknown> {
    return parse(await this.shell<unknown>("Call", method, params));
  }
  async sessions(): Promise<SessionRow[]> {
    return parseSessions(await this.rpc("sessions.list"));
  }
  async doctor(): Promise<DoctorReport> {
    return parseDoctor(await this.rpc("doctor.checks"));
  }

  onState(handler: (s: BackendStatus) => void): () => void {
    return this.t.on("backend:state", (d) => handler(d as BackendStatus));
  }
  /** Pushes from the backend arrive as "rpc/<method>". */
  onNotify(method: string, handler: (params: unknown) => void): () => void {
    return this.t.on(`rpc/${method}`, handler);
  }
  onOpenLogs(handler: () => void): () => void {
    return this.t.on("ui/open-logs", () => handler());
  }
}
