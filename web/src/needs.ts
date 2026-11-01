// Needs you and Updates: the shapes of needs.list, updates.status and
// updates.check, read defensively. Every string stays as received; the views
// make it safe (visible() + textContent).

const rec = (v: unknown): Record<string, unknown> => (v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {});
const str = (v: unknown): string => (typeof v === "string" ? v : "");
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);

export type NeedKind = "escalation" | "claim" | "intake" | "person" | "doctor";
export const NEED_KINDS: readonly NeedKind[] = ["escalation", "claim", "intake", "person", "doctor"];
/** Chip and badge words, in the order the queue shows them. */
export const KIND_LABEL: Record<NeedKind, string> = { escalation: "Escalations", claim: "Claims", intake: "Intake", person: "People", doctor: "Doctor" };
export const KIND_ONE: Record<NeedKind, string> = { escalation: "Escalation", claim: "Claim", intake: "Intake", person: "Person", doctor: "Doctor" };

export interface NeedAction { id: string; label: string; enabled: boolean; reason: string }
export type ActorState = "yes" | "no" | "depends" | "unknown" | "n/a";
export interface NeedItem {
  kind: NeedKind;
  id: string;
  title: string;
  project: string;
  createdAt: string;
  facts: [string, string][];
  preview: string;
  actions: NeedAction[];
  actor: { state: ActorState; text: string };
}
export interface KindStatus { kind: NeedKind; ok: boolean; reason: string; count: number }
export interface Needs { items: NeedItem[]; kinds: KindStatus[]; generatedAt: string; viewerRole: string }

const isKind = (k: string): k is NeedKind => (NEED_KINDS as readonly string[]).includes(k);
const ACTORS: readonly string[] = ["yes", "no", "depends", "unknown", "n/a"];

export function parseNeeds(raw: unknown): Needs {
  const r = rec(raw);
  const items: NeedItem[] = [];
  for (const x of arr(r.items)) {
    const o = rec(x);
    const kind = str(o.kind);
    if (!isKind(kind)) continue; // an unknown kind is never shown as a known one
    const actor = rec(o.who_may_act);
    const state = str(actor.state);
    items.push({
      kind,
      id: str(o.id),
      title: str(o.title),
      project: str(o.project),
      createdAt: str(o.created_at),
      facts: Object.entries(rec(o.facts)).map(([k, v]): [string, string] => [k, str(v)]).filter(([, v]) => v !== ""),
      preview: str(o.preview),
      actions: arr(o.actions).map((a) => {
        const p = rec(a);
        // Every action is read as disabled until a later build enables writes.
        return { id: str(p.id), label: str(p.label), enabled: false, reason: str(p.reason) || "Not in this build yet." };
      }),
      actor: { state: (ACTORS.includes(state) ? state : "unknown") as ActorState, text: str(actor.text) },
    });
  }
  const kinds: KindStatus[] = [];
  for (const x of arr(r.kinds)) {
    const o = rec(x);
    const kind = str(o.kind);
    if (isKind(kind)) kinds.push({ kind, ok: str(o.status) === "ok", reason: str(o.reason), count: typeof o.count === "number" ? o.count : 0 });
  }
  return { items, kinds, generatedAt: str(r.generated_at), viewerRole: str(r.viewer_role) };
}

// ---- updates

export interface SkillRow { agent: string; installed: boolean; version: string; current: boolean }
export interface McpRow { agent: string; name: string; status: string; message: string }
export interface UpdatesStatus {
  version: string;
  onDisk: string;
  restartNeeded: boolean;
  skills: SkillRow[];
  skillNote: string;
  mcp: McpRow[];
  mcpNote: string;
  autoAvailable: boolean;
}

export function parseUpdatesStatus(raw: unknown): UpdatesStatus {
  const r = rec(raw);
  const c = rec(r.client);
  return {
    version: str(c.version),
    onDisk: str(c.on_disk),
    restartNeeded: c.restart_needed === true,
    skills: arr(r.skill).map((s) => {
      const o = rec(s);
      return { agent: str(o.agent), installed: o.installed === true, version: str(o.installed_version), current: o.current === true };
    }),
    skillNote: str(r.skill_note),
    mcp: arr(r.mcp).map((m) => {
      const o = rec(m);
      return { agent: str(o.agent), name: str(o.name), status: str(o.status), message: str(o.message) };
    }),
    mcpNote: str(r.mcp_note),
    autoAvailable: rec(r.auto).available === true,
  };
}

export interface UpdateCheck {
  current: string;
  target: string;
  action: string;
  available: boolean;
  manualRequired: boolean;
  manualText: string;
  hold: boolean;
  checkedAt: string;
  /** True when the server reused an answer from the last minute. */
  cached: boolean;
}

export function parseUpdateCheck(raw: unknown): UpdateCheck {
  const r = rec(raw);
  return {
    current: str(r.current),
    target: str(r.target),
    action: str(r.action),
    available: r.update_available === true,
    manualRequired: r.manual_required === true,
    manualText: str(r.manual_text),
    hold: r.hold === true,
    checkedAt: str(r.checked_at),
    cached: r.cached === true,
  };
}
