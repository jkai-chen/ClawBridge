// Parsers for the P2-5 read methods (projects, threads, Linear, teams,
// dispatcher, account). Every string is kept as received; the views make it
// safe with safe() and textContent. A field the server did not send is
// empty, never invented.

const rec = (v: unknown): Record<string, unknown> => (v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {});
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const str = (v: unknown): string => (typeof v === "string" ? v : "");
const num = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) ? v : 0);
const strs = (v: unknown): string[] => arr(v).filter((x): x is string => typeof x === "string");

// ---- projects.list

export interface BoardName { id: string; name: string; isDefault: boolean; closes: string }

export interface Overview {
  error: string;
  role: string;
  coordinator: string;
  coordinatorStatus: string;
  active: number;
  retired: number;
  moved: number;
  stale: number;
  sessions: { name: string; owner: string; status: string; agent: string }[];
  linear: { configured: boolean; unreadable: boolean; mode: string; namesAt: string; teams: BoardName[]; projects: BoardName[]; cancelKinds: string[] | null };
}

export interface ProjectRow {
  id: string;
  slug: string;
  name: string;
  role: string;
  gitRemote: string;
  /** null when the overview was not read (past the first 20). */
  overview: Overview | null;
}

export function parseOverview(raw: unknown): Overview {
  const o = rec(raw);
  const counts = rec(o.counts);
  const l = rec(o.linear);
  const policy = rec(l.status_policy);
  const board = (x: unknown): BoardName => {
    const b = rec(x);
    return { id: str(b.id), name: str(b.name), isDefault: b.default === true, closes: str(b.closes) };
  };
  return {
    error: str(o.error),
    role: str(rec(o.project).role),
    coordinator: str(rec(o.coordinator).name),
    coordinatorStatus: str(rec(o.coordinator).status),
    active: num(counts.active), retired: num(counts.retired), moved: num(counts.moved), stale: num(counts.stale),
    sessions: arr(o.sessions).map((s) => {
      const r = rec(s);
      return { name: str(r.name), owner: str(r.owner_principal), status: str(r.status), agent: str(r.agent_kind) };
    }),
    linear: {
      configured: l.configured === true, unreadable: l.unreadable === true, mode: str(l.mode), namesAt: str(l.names_updated_at),
      teams: arr(l.teams).map(board), projects: arr(l.projects).map(board),
      cancelKinds: "agent_may_cancel_kinds" in policy ? strs(policy.agent_may_cancel_kinds) : null,
    },
  };
}

export function parseProjects(raw: unknown): { source: string; rows: ProjectRow[] } {
  const top = rec(raw);
  const overviews = rec(top.overviews);
  return {
    source: str(top.source),
    rows: arr(rec(top.data).projects).map((p) => {
      const r = rec(p);
      const id = str(r.id);
      return { id, slug: str(r.slug), name: str(r.name), role: str(r.role), gitRemote: str(r.git_remote), overview: id in overviews ? parseOverview(overviews[id]) : null };
    }),
  };
}

// ---- project.threads, project.thread

export interface ThreadRow { id: string; kind: string; startedAt: string; lastAt: string; messages: number; participants: string[]; more: number }
export interface ThreadMessage {
  sequence: number; id: string; sender: string; address: string; replyTo: string; kind: string; body: string; coordination: string; sent: string;
  recipients: { name: string; deliveredAt: string; readAt: string }[];
}

export function parseThreads(raw: unknown): { rows: ThreadRow[]; next: string; readableFrom: string } {
  const d = rec(rec(raw).data);
  return {
    next: str(d.next), readableFrom: str(d.readable_from),
    rows: arr(d.threads).map((t) => {
      const r = rec(t);
      return { id: str(r.thread_id), kind: str(r.kind), startedAt: str(r.started_at), lastAt: str(r.last_at), messages: num(r.messages), participants: strs(r.participants), more: num(r.participants_more) };
    }),
  };
}

export function parseThread(raw: unknown): { messages: ThreadMessage[]; next: number; readableFrom: string } {
  const d = rec(rec(raw).data);
  return {
    next: num(d.next), readableFrom: str(d.readable_from),
    messages: arr(d.messages).map((m) => {
      const r = rec(m);
      return {
        sequence: num(r.sequence), id: str(r.id), sender: str(r.sender), address: str(r.address), replyTo: str(r.reply_to), kind: str(r.kind),
        body: str(r.body), coordination: str(r.coordination), sent: str(r.created_at),
        recipients: arr(r.recipients).map((x) => ({ name: str(rec(x).name), deliveredAt: str(rec(x).delivered_at), readAt: str(rec(x).read_at) })),
      };
    }),
  };
}

// ---- Linear

export interface LinearProfile { id: string; name: string; access: string; account: string; workspace: string }

export function parseConnection(raw: unknown): { profiles: LinearProfile[]; bindingError: string } {
  const r = rec(raw);
  return {
    bindingError: str(r.binding_error),
    profiles: arr(r.profiles).map((p) => {
      const x = rec(p);
      return { id: str(x.id), name: str(x.name), access: str(x.access), account: str(x.account_label), workspace: str(x.workspace_label) };
    }),
  };
}

export interface Binding {
  configured: boolean; revision: number; mode: string; provider: string; updatedAt: string;
  teamIds: string[]; defaultTeam: string; projectIds: string[]; otherTeams: [string, string][];
  lanes: { name: string; teams: string[]; defaultTeam: string }[]; unpinned: string; teamsWithoutProject: string[];
}
export interface BindingPage {
  binding: Binding | null;
  bindingError: string;
  withoutProject: { teamId: string; name: string; inert: boolean }[];
  names: Map<string, string>;
  namesAt: string;
  role: string;
  closes: Map<string, string>;
  cancelKinds: string[] | null;
}

export function parseBinding(raw: unknown): BindingPage {
  const r = rec(raw);
  const b = r.binding ? rec(r.binding) : null;
  const stored = rec(r.stored);
  const l = rec(stored.linear);
  const names = new Map<string, string>();
  const closes = new Map<string, string>();
  for (const x of [...arr(l.teams), ...arr(l.projects)]) {
    const y = rec(x);
    if (str(y.name)) names.set(str(y.id), str(y.name));
    if (str(y.closes)) closes.set(str(y.id), str(y.closes));
  }
  const policy = rec(l.status_policy);
  return {
    bindingError: str(r.binding_error),
    names, closes, namesAt: str(l.names_updated_at), role: str(stored.role),
    cancelKinds: "agent_may_cancel_kinds" in policy ? strs(policy.agent_may_cancel_kinds) : null,
    withoutProject: arr(r.without_project).map((x) => ({ teamId: str(rec(x).team_id), name: str(rec(x).name), inert: rec(x).inert === true })),
    binding: b && {
      configured: b.configured === true, revision: num(b.revision), mode: str(b.mode), provider: str(b.provider), updatedAt: str(b.updated_at),
      teamIds: strs(b.team_ids), defaultTeam: str(b.default_team_id), projectIds: strs(b.project_ids),
      otherTeams: Object.entries(rec(b.other_teams)).map(([k, v]) => [k, str(v)] as [string, string]),
      lanes: arr(b.lanes).map((x) => ({ name: str(rec(x).name), teams: strs(rec(x).write_team_ids), defaultTeam: str(rec(x).default_team_id) })),
      unpinned: str(b.unpinned), teamsWithoutProject: strs(b.teams_without_project),
    },
  };
}

export interface Issue { id: string; identifier: string; title: string; description: string; teamId: string; state: string; assignee: string; url: string }

const issueOf = (x: unknown): Issue => {
  const r = rec(x);
  return { id: str(r.id), identifier: str(r.identifier), title: str(r.title), description: str(r.description), teamId: str(r.team_id), state: str(r.state), assignee: str(r.assignee), url: str(r.url) };
};

export function parseTasks(raw: unknown): { items: Issue[]; next: string; complete: boolean } {
  const p = rec(rec(raw).page);
  return { items: arr(p.items).map(issueOf), next: str(p.next_cursor), complete: p.complete === true };
}

export function parseIssue(raw: unknown): Issue {
  return issueOf(rec(raw).issue);
}

export interface Member { id: string; name: string; teamId: string }

const membersOf = (v: unknown): Member[] => arr(v).map((m) => ({ id: str(rec(m).id), name: str(rec(m).name), teamId: str(rec(m).team_id) }));

export interface Intake {
  mode: string; revision: number; signals: string[]; trusted: string[]; queue: Record<string, unknown>[];
  members: Member[]; labels: { teamId: string; found: number; complete: boolean }[]; namesNote: string;
}

export function parseIntake(raw: unknown): Intake {
  const r = rec(raw);
  return {
    mode: str(r.mode), revision: num(r.revision), signals: arr(r.signals).map(String), trusted: strs(r.trusted),
    queue: arr(r.queue).map(rec), members: membersOf(r.members), namesNote: str(r.names_note),
    labels: arr(r.labels).map((x) => ({ teamId: str(rec(x).team_id), found: num(rec(x).found), complete: rec(x).complete === true })),
  };
}

export function parseCancelKinds(raw: unknown): { revision: number; cancelKinds: string[]; kinds: string[]; line: string } {
  const v = rec(rec(raw).view);
  return { revision: num(v.revision), cancelKinds: strs(v.agent_may_cancel_kinds), kinds: strs(v.kinds), line: str(v.line) };
}

export interface ProjectStatus {
  linearProject: string; name: string; type: string; total: number; open: number; complete: boolean; suggestion: string; note: string; refused: string; error: string;
}

export function parseProjectStatus(raw: unknown): ProjectStatus {
  const s = rec(rec(raw).status);
  const i = rec(s.issues);
  return {
    linearProject: str(s.linear_project), name: str(rec(s.status).name), type: str(rec(s.status).type),
    total: num(i.total), open: num(i.open), complete: i.complete === true,
    suggestion: str(s.suggestion), note: str(s.suggestion_note), refused: str(s.refused), error: str(s.error),
  };
}

export interface TeamViews { teamId: string; views: { id: string; name: string; kind: string; teamName: string }[]; otherKinds: number; workspaceViews: number; more: boolean; error: string }

export function parseViews(raw: unknown): TeamViews[] {
  return arr(rec(raw).teams).map((t) => {
    const r = rec(t);
    return {
      teamId: str(r.team_id), otherKinds: num(r.other_kinds), workspaceViews: num(r.workspace_views), more: r.more === true, error: str(r.error),
      views: arr(r.views).map((v) => ({ id: str(rec(v).id), name: str(rec(v).name), kind: str(rec(v).kind), teamName: str(rec(v).team_name) })),
    };
  });
}

export interface Queue {
  available: boolean; reason: string; hasStatus: boolean;
  used: number; limit: number; people: number; agents: number; resetsAt: string;
  queued: number; sending: number; dead: number; expired: number; uncertain: number; oldestS: number;
  writes: Record<string, unknown>[];
}

export function parseQueue(raw: unknown): Queue {
  const r = rec(raw);
  const s = rec(r.status);
  const o = rec(s.outbox);
  const b = rec(s.budget);
  const by = rec(b.by);
  return {
    available: r.available === true, reason: str(r.reason), hasStatus: "outbox" in s && "budget" in s,
    used: num(b.used), limit: num(b.limit), people: num(by.people), agents: num(by.agents), resetsAt: str(b.window_resets_at),
    queued: num(o.queued), sending: num(o.sending), dead: num(o.dead_24h), expired: num(o.expired_24h), uncertain: num(o.uncertain_24h), oldestS: num(o.oldest_queued_age_s),
    writes: arr(r.writes).map(rec),
  };
}

// ---- Teams & People

export interface RolesView {
  revision: number;
  teams: { teamId: string; leads: string[]; bound: boolean }[];
  people: { id: string; role: string; teams: string[]; principal: string }[];
  youLead: string[];
  owner: boolean;
  users: Map<string, string>;
  namesErr: string;
}

export function parseRoles(raw: unknown): RolesView {
  const r = rec(raw);
  return {
    revision: num(r.revision), youLead: strs(r.you_lead), owner: r.owner === true, namesErr: str(r.names_err),
    teams: arr(r.teams).map((t) => ({ teamId: str(rec(t).team_id), leads: strs(rec(t).leads), bound: rec(t).bound === true })),
    people: arr(r.people).map((p) => ({ id: str(rec(p).linear_user_id), role: str(rec(p).role), teams: strs(rec(p).teams), principal: str(rec(p).principal) })),
    users: new Map(arr(r.users).map((u) => [str(rec(u).id), str(rec(u).name)] as [string, string])),
  };
}

export function parseTeamMembers(raw: unknown): { members: Member[]; trusted: string[]; namesNote: string } {
  const r = rec(raw);
  return { members: membersOf(r.members), trusted: strs(r.trusted), namesNote: str(r.names_note) };
}

// ---- Dispatcher

export interface DispatcherStatus {
  denied: string;
  hasDoctor: boolean;
  sealConfigured: boolean; sealPrevious: boolean; modelOn: boolean;
  holders: { principal: string; you: boolean; key: string; configRevision: number; pendingRevision: number; agentStatus: string; testFailure: string; testRevision: number }[];
  enabled: boolean; mode: string; revision: number; updatedAt: string;
  model: { use: string; provider: string; model: string; key: string; agent: string } | null;
  budget: { used: number; limit: number; resetsAt: string } | null;
}

export function parseDispatcher(raw: unknown): DispatcherStatus {
  const r = rec(raw);
  const d = rec(r.doctor);
  const v = rec(r.view);
  const m = rec(v.model);
  const b = r.budget ? rec(r.budget) : null;
  return {
    denied: str(r.denied), hasDoctor: r.doctor !== null && r.doctor !== undefined,
    sealConfigured: d.seal_configured === true, sealPrevious: d.seal_previous === true, modelOn: d.model_on === true,
    holders: arr(d.holders).map((h) => {
      const x = rec(h);
      return { principal: str(x.principal), you: x.you === true, key: str(x.key), configRevision: num(x.config_revision), pendingRevision: num(x.pending_revision), agentStatus: str(x.agent_status), testFailure: str(x.test_failure), testRevision: num(x.test_revision) };
    }),
    enabled: v.enabled === true, mode: str(v.mode), revision: num(v.revision), updatedAt: str(v.updated_at),
    model: r.view && Object.keys(m).length ? { use: str(m.use), provider: str(m.provider_id), model: str(m.model), key: str(m.key), agent: str(m.agent) } : null,
    budget: b && "limit" in b ? { used: num(b.used), limit: num(b.limit), resetsAt: str(b.window_resets_at) } : null,
  };
}

// ---- Account

export interface Device { id: string; principal: string; label: string; kind: string; status: string; createdAt: string; lastUsedAt: string; expiresAt: string; keyStore: string; dormantSoon: boolean }
const deviceOf = (d: unknown): Device => {
  const x = rec(d);
  return { id: str(x.device_id), principal: str(x.principal), label: str(x.label), kind: str(x.kind), status: str(x.status), createdAt: str(x.created_at), lastUsedAt: str(x.last_used_at), expiresAt: str(x.expires_at), keyStore: str(x.key_store), dormantSoon: x.dormant_soon === true };
};

export interface AccountDevices {
  hosted: boolean; reason: string; principal: string; mainOwner: boolean; devices: Device[];
  transfer: Record<string, unknown> | null;
  events: { id: string; kind: string; label: string; createdAt: string; acked: boolean }[];
}

export function parseAccountDevices(raw: unknown): AccountDevices {
  const r = rec(raw);
  return {
    hosted: r.hosted === true, reason: str(r.reason), principal: str(r.principal), mainOwner: r.main_owner === true,
    devices: arr(r.devices).map(deviceOf),
    transfer: r.transfer && typeof r.transfer === "object" ? rec(r.transfer) : null,
    events: arr(r.events).map((e) => ({ id: str(rec(e).event_id), kind: str(rec(e).kind), label: str(rec(e).label), createdAt: str(rec(e).created_at), acked: Boolean(rec(e).acked_at) })),
  };
}

export interface Invite { id: string; email: string; status: string; expiresAt: string; purpose: string; grants: string[] }

export function parseInvites(raw: unknown): { hosted: boolean; reason: string; invites: Invite[] } {
  const r = rec(raw);
  const slugs = new Map(arr(r.projects).map((p) => [str(rec(p).id), str(rec(p).slug)] as [string, string]));
  return {
    hosted: r.hosted === true, reason: str(r.reason),
    invites: arr(r.invites).map((i) => {
      const x = rec(i);
      return {
        id: str(x.invite_id), email: str(x.email), status: str(x.status), expiresAt: str(x.expires_at), purpose: str(x.purpose),
        grants: Object.entries(rec(x.grants)).map(([p, role]) => `${slugs.get(p) || p}=${str(role)}`).sort(),
      };
    }),
  };
}

export function parseAccountUsers(raw: unknown): { hosted: boolean; reason: string; mainOwner: boolean; users: { principal: string; roles: string[]; devices: number; invitedBy: string }[] } {
  const r = rec(raw);
  return {
    hosted: r.hosted === true, reason: str(r.reason), mainOwner: r.main_owner === true,
    users: arr(r.users).map((u) => {
      const x = rec(u);
      return { principal: str(x.principal), roles: arr(x.roles).map((y) => `${str(rec(y).slug)}=${str(rec(y).role)}`), devices: arr(x.devices).length, invitedBy: str(x.invited_by) };
    }),
  };
}
