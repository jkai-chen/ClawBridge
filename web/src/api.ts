// Typed access to the server's read methods. Every string that came from a
// person or a remote system is kept as received here and made safe in the
// views (visible() + textContent).

import { ConnectionError, RpcError, type Transport } from "./transport";

/** The protocol this page speaks (guibackend.ProtocolVersion). */
export const PROTOCOL = 1;

/** A failed call, for the views: its text, and whether the connection is gone. */
export class CallError extends Error {
  constructor(message: string, readonly connection = false) {
    super(message);
    this.name = "CallError";
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

export interface SessionsResult {
  rows: SessionRow[];
  /** The executor's source: "offline-mirror" means a copy, not a live answer. */
  source: string;
  /** When the offline copy was last updated (ISO), if it says. */
  mirrorAt: string;
}

/** Reads a sessions.list result: `{source, data: {sessions, mirror?}}`. */
export function parseSessions(raw: unknown): SessionsResult {
  const top = rec(raw);
  const data = rec(top.data);
  const list = data.sessions;
  const rows = Array.isArray(list) ? list : [];
  return { source: str(top.source), mirrorAt: str(rec(data.mirror).updated_at), rows: rows.map((r) => {
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
  }) };
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
  const r = rec(raw);
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

// ---- hello

export interface Hello {
  clientVersion: string;
  protocol: number;
  mode: string;
  platform: string;
  workspace: string;
  project: string;
  /** The signed-in person (an email or name), if the profile has one. */
  principal: string;
  /** This device's host name, if the client reported it. */
  device: string;
  /** The WSL distro, when the client runs in WSL and says which. */
  distro: string;
}

export function parseHello(raw: unknown): Hello {
  const r = rec(raw);
  const p = rec(r.profile);
  return {
    clientVersion: str(r.client_version),
    protocol: typeof r.protocol === "number" ? r.protocol : 0,
    mode: str(r.mode),
    platform: str(r.platform),
    workspace: str(p.workspace),
    project: str(p.project),
    principal: str(p.principal),
    device: str(p.device),
    distro: str(r.distro),
  };
}

// ---- the api

export class Api {
  constructor(private readonly t: Transport) {}

  /** One read. A rejected call is a ConnectionError (the shell shows the state) or a CallError. */
  async rpc(method: string, params: Record<string, unknown> = {}): Promise<unknown> {
    try {
      return await this.t.call(method, params);
    } catch (e) {
      if (e instanceof ConnectionError) throw e;
      if (e instanceof RpcError) throw new CallError(e.message);
      throw new CallError(e instanceof Error ? e.message : String(e));
    }
  }
  async hello(): Promise<Hello> {
    return parseHello(await this.rpc("hello"));
  }
  async sessions(): Promise<SessionsResult> {
    return parseSessions(await this.rpc("sessions.list"));
  }
  async doctor(): Promise<DoctorReport> {
    return parseDoctor(await this.rpc("doctor.checks"));
  }
  events(onEvent: Parameters<Transport["events"]>[0], onState: Parameters<Transport["events"]>[1]): () => void {
    return this.t.events(onEvent, onState);
  }
}
