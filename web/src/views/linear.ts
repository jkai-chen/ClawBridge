// Linear (GUI-D-SPEC §5.5): read-only sub-pages. Every write control is
// disabled until P3. Linear text (titles, names, notes) is untrusted and
// reaches the page through textContent only.
import { h, type Child } from "../dom";
import { plural } from "../format";
import { routeArg } from "../router";
import {
  parseBinding, parseCancelKinds, parseConnection, parseIntake, parseIssue, parseProjectStatus, parseQueue, parseTasks, parseViews,
  type BindingPage, type Issue, type LinearProfile,
} from "../pages";
import { emptyBlock, safe, titleRow, type View, type ViewCtx } from "./common";
import { ago, call, dash, dataTable, disabledWrite, linkTabs, loadInto, meter, mono, note, pill, plainText } from "./pagekit";

export const LINEAR_TABS: [string, string][] = [
  ["connection", "Connection"], ["binding", "Binding"], ["tasks", "Tasks"], ["intake", "Intake"],
  ["cancel", "Who may cancel"], ["status", "Project status"], ["views", "Views"], ["queue", "Queued writes"],
];

let chosen = "";

/** The connection the pages read through: the chosen one, else the only one. */
function pickProfile(profiles: LinearProfile[]): LinearProfile | undefined {
  return profiles.find((p) => p.id === chosen) ?? profiles[0];
}

const noConnection = (): HTMLElement => emptyBlock("No Linear connection.", "Connect Linear from your terminal: clawbridge linear connect --name NAME --access read");

export function nameOf(page: BindingPage | null, id: string): string {
  return safe(page?.names.get(id) || id);
}

function connectionPage(ctx: ViewCtx, host: HTMLElement): void {
  loadInto(ctx, host, "the Linear connection", call(ctx, "linear.connection", {}, parseConnection), ({ profiles, bindingError }) => {
    if (profiles.length === 0) return [noConnection(), disabledWrite("Connect Linear")];
    const sel = pickProfile(profiles);
    return h("div", { class: "stack" },
      bindingError ? h("div", { class: "notice plain" }, h("span", { class: "grow", text: `The board couldn't be read: ${safe(bindingError)}` })) : null,
      h("div", { class: "panel" }, dataTable([{ head: "Connection" }, { head: "Connected as" }, { head: "Workspace" }, { head: "Access" }],
        profiles.map((p) => [mono(p.name || p.id), dash(p.account), dash(p.workspace), pill(p.access === "write" ? "Read and write" : p.access === "read" ? "Read only" : p.access || "Unknown", p.access === "write" ? "acc" : "")]))),
      sel ? note(`Pages read through ${safe(sel.name || sel.id)}.`) : null,
      h("div", null, disabledWrite("Disconnect")));
  });
}

function bindingPage(ctx: ViewCtx, host: HTMLElement): void {
  loadInto(ctx, host, "the Linear binding", call(ctx, "linear.binding", {}, parseBinding), (page) => {
    const b = page.binding;
    if (!b || !b.configured) return [emptyBlock("No Linear board is linked to this project.", page.bindingError ? safe(page.bindingError) : undefined), h("div", null, disabledWrite("Link a board"))];
    return h("div", { class: "stack" },
      h("div", { class: "panel pad" }, h("dl", { class: "kv" },
        h("dt", { text: "Mode" }), h("dd", { text: dash(b.mode) }),
        h("dt", { text: "Revision" }), h("dd", { text: String(b.revision) }),
        h("dt", { text: "Teams" }), h("dd", { text: b.teamIds.map((t) => nameOf(page, t) + (t === b.defaultTeam ? " (default)" : "")).join(", ") || "-" }),
        h("dt", { text: "Projects" }), h("dd", { text: b.projectIds.map((t) => nameOf(page, t) + (page.closes.get(t) ? ` (closes: ${safe(page.closes.get(t))})` : "")).join(", ") || "None" }),
        h("dt", { text: "Agents may close" }), h("dd", { text: page.closes.size ? [...page.closes.values()].some((v) => v === "agent") ? "Agents close issues in some projects" : "People close issues" : "Not reported" }),
        h("dt", { text: "Other teams" }), h("dd", { text: b.otherTeams.map(([t, a]) => `${nameOf(page, t)} (${safe(a)})`).join(", ") || "None" }),
        h("dt", { text: "Lanes" }), h("dd", { text: b.lanes.map((l) => `${safe(l.name)} → ${l.teams.map((t) => nameOf(page, t)).join(", ")}`).join("; ") || "None" }),
        h("dt", { text: "Unpinned sessions" }), h("dd", { text: dash(b.unpinned) }),
        h("dt", { text: "Names stored" }), h("dd", { text: page.namesAt ? ago(page.namesAt) : "-" }))),
      page.withoutProject.length ? note(`Teams listed without a project: ${page.withoutProject.map((t) => safe(t.name || t.teamId) + (t.inert ? " (inactive)" : "")).join(", ")}.`) : null,
      page.bindingError ? note(`Part of the board couldn't be read: ${page.bindingError}`) : null,
      h("div", { class: "toolbar" }, disabledWrite("Link"), disabledWrite("Unlink")));
  });
}

function withProfile(ctx: ViewCtx, host: HTMLElement, what: string, render: (profile: string) => Child): void {
  loadInto(ctx, host, what, call(ctx, "linear.connection", {}, parseConnection), ({ profiles }) => {
    const p = pickProfile(profiles);
    if (!p) return noConnection();
    const out: Child[] = [];
    if (profiles.length > 1) {
      const sel = h("select", { class: "input", "aria-label": "Linear connection" }, profiles.map((x) => h("option", { value: x.id, selected: x.id === p.id, text: safe(x.name || x.id) })));
      sel.addEventListener("change", () => { chosen = sel.value; withProfile(ctx, host, what, render); });
      out.push(h("div", { class: "toolbar" }, sel));
    }
    out.push(h("div", { class: "section-body" }, render(p.id)));
    return out;
  });
}

function issueRow(i: Issue): Child[] {
  return [mono(i.identifier || i.id), h("span", { text: safe(i.title) }), dash(i.state), dash(i.assignee), mono(i.teamId)];
}

function tasksPage(ctx: ViewCtx, host: HTMLElement): void {
  withProfile(ctx, host, "the Linear tasks", (profile) => {
    const out = h("div", { class: "stack" });
    const table = h("div", { class: "panel" });
    const detail = h("div", { class: "panel pad", hidden: true });
    let items: Issue[] = [];
    let next = "";
    let selected = -1;
    const paint = (): void => {
      table.replaceChildren(items.length === 0 ? emptyBlock("No issues on the board.") : dataTable(
        [{ head: "Issue" }, { head: "Title" }, { head: "State" }, { head: "Assignee" }, { head: "Team" }], items.map(issueRow),
        { selected, onPick: (i) => { selected = i; paint(); const it = items[i]; if (it) open(it); } }));
      if (next) {
        const b = h("button", { class: "btn sm", type: "button", text: "Load more" });
        b.addEventListener("click", () => fetchPage(next));
        table.append(h("div", { style: "padding:8px 16px 12px" }, b));
      }
    };
    const open = (it: Issue): void => {
      detail.hidden = false;
      loadInto(ctx, detail, "this issue", call(ctx, "linear.issue", { profile, issue_id: it.id }, parseIssue), (full) =>
        h("div", null, h("div", { class: "sec-head" }, h("h2", { text: safe(full.identifier || it.identifier) }), pill(full.state || it.state || "Unknown")),
          h("p", { text: safe(full.title || it.title) }), full.description ? plainText(full.description) : note("No description.")));
    };
    const fetchPage = (cursor: string): void => {
      loadInto(ctx, table, "the issues", call(ctx, "linear.tasks", { profile, ...(cursor ? { cursor } : {}) }, parseTasks), (r) => {
        items = cursor ? items.concat(r.items) : r.items;
        next = r.next;
        queueMicrotask(paint);
        return h("div");
      });
    };
    out.append(table, detail);
    fetchPage("");
    return out;
  });
}

const SIGNALS: Record<string, string> = { S1: "the needs-agent label", S2: "delegating the issue to the ClawBridge app", S3: "@-mentioning the app in a comment" };

function intakePage(ctx: ViewCtx, host: HTMLElement): void {
  withProfile(ctx, host, "intake", (profile) => {
    const box = h("div", null);
    loadInto(ctx, box, "intake", call(ctx, "linear.intake", { profile }, parseIntake), (i) => {
      const names = new Map(i.members.map((m) => [m.id, m.name] as [string, string]));
      return h("div", { class: "stack" },
        h("div", { class: "panel pad" }, h("dl", { class: "kv" },
          h("dt", { text: "Mode" }), h("dd", { text: dash(i.mode) }),
          h("dt", { text: "Signals" }), h("dd", { text: i.signals.map((s) => SIGNALS[s] ?? s).map(safe).join(", ") || "None" }),
          h("dt", { text: "Revision" }), h("dd", { text: String(i.revision) }),
          h("dt", { text: "Trusted people" }), h("dd", { text: i.trusted.map((t) => safe(names.get(t) || t)).join(", ") || "Nobody yet" }),
          h("dt", { text: "needs-agent label" }), h("dd", { text: i.labels.length ? i.labels.map((l) => `${safe(l.teamId)}: ${l.found === 1 ? "found" : l.found === 0 ? "missing" : `${l.found} found (must be exactly one)`}${l.complete ? "" : ", not fully read"}`).join("; ") : "Not checked" }))),
        i.namesNote ? note(i.namesNote) : null,
        h("div", { class: "panel" }, i.queue.length === 0 ? emptyBlock("The intake queue is empty.") : dataTable([{ head: "Item" }, { head: "State" }, { head: "Created", right: true }],
          i.queue.map((q) => [mono(String(q.issue ?? q.id ?? "")), pill(String(q.state ?? "")), ago(String(q.created_at ?? ""))]))),
        h("div", { class: "toolbar" }, disabledWrite("Change intake"), disabledWrite("Trust a person")));
    });
    return box;
  });
}

function cancelPage(ctx: ViewCtx, host: HTMLElement): void {
  withProfile(ctx, host, "who may cancel", (profile) => {
    const box = h("div", null);
    loadInto(ctx, box, "who may cancel", call(ctx, "linear.cancelKinds", { profile }, parseCancelKinds), (c) =>
      h("div", { class: "stack" },
        h("div", { class: "panel pad" }, h("dl", { class: "kv" },
          h("dt", { text: "Agents may cancel" }), h("dd", { text: c.cancelKinds.map(safe).join(", ") || "In no project kind" }),
          h("dt", { text: "Project kinds" }), h("dd", { text: c.kinds.map(safe).join(", ") || "None set" }),
          h("dt", { text: "Revision" }), h("dd", { text: String(c.revision) }))),
        c.line ? note(c.line) : null,
        h("div", null, disabledWrite("Change who may cancel"))));
    return box;
  });
}

function statusPage(ctx: ViewCtx, host: HTMLElement): void {
  withProfile(ctx, host, "project status", (profile) => {
    const box = h("div", null);
    loadInto(ctx, box, "the bound projects", call(ctx, "linear.binding", {}, parseBinding), (page) => {
      const ids = page.binding?.projectIds ?? [];
      if (ids.length === 0) return emptyBlock("No Linear project is bound.", "A project's status is shown for the projects linked to this board.");
      return h("div", { class: "stack" }, ids.map((id) => {
        const row = h("div", { class: "panel pad" });
        const show = (suggest: boolean): void => {
          loadInto(ctx, row, `the status of ${nameOf(page, id)}`, call(ctx, "linear.projectStatus", { profile, linear_project: id, suggest }, parseProjectStatus), (s) =>
            h("div", null,
              h("div", { class: "sec-head" }, h("h2", { text: nameOf(page, id) }), pill(s.name || "No status")),
              s.error ? note(s.error) : null, s.refused ? note(s.refused) : null,
              h("p", { text: `${plural(s.total, "issue")}, ${s.open} open${s.complete ? "" : " (not every page was read)"}.` }),
              s.suggestion ? h("p", { text: `Suggested status: ${safe(s.suggestion)}${s.note ? `. ${safe(s.note)}` : ""}` }) : null,
              h("div", { class: "toolbar" },
                h("button", { class: "btn sm", type: "button", text: "Suggest a status", on: { click: () => show(true) } }),
                disabledWrite("Set status"))));
        };
        show(false);
        return row;
      }));
    });
    return box;
  });
}

function viewsPage(ctx: ViewCtx, host: HTMLElement): void {
  withProfile(ctx, host, "the Linear views", (profile) => {
    const box = h("div", null);
    loadInto(ctx, box, "the Linear views", async () => {
      const [views, binding] = await Promise.all([call(ctx, "linear.views", { profile }, parseViews)(), call(ctx, "linear.binding", {}, parseBinding)().catch(() => null)]);
      return { views, binding };
    }, ({ views, binding }) => {
      if (views.length === 0) return emptyBlock("No bound teams.");
      return h("div", { class: "stack" }, views.map((t) => h("div", { class: "panel pad" },
        h("div", { class: "sec-head" }, h("h2", { text: nameOf(binding, t.teamId) }), h("span", { class: "meta", text: plural(t.views.length, "view") })),
        t.error ? note(`Couldn't read this team's views: ${t.error}`) : t.views.length === 0 ? h("p", { class: "muted", text: "No shared views." }) : h("ul", { class: "passlist" }, t.views.map((v) => h("li", null, h("span", { text: safe(v.name) }), " ", pill(v.kind)))),
        t.otherKinds || t.workspaceViews ? note(`${t.otherKinds} of other kinds and ${t.workspaceViews} workspace-wide views are left out.`) : null,
        t.more ? note("More views exist than were read.") : null)));
    });
    return box;
  });
}

function queuePage(ctx: ViewCtx, host: HTMLElement): void {
  loadInto(ctx, host, "the queued writes", call(ctx, "linear.queue", {}, parseQueue), (q) => {
    if (!q.available) return emptyBlock(safe(q.reason) || "The write queue is for hosted workspaces.");
    return h("div", { class: "stack" },
      q.hasStatus ? h("div", { class: "panel pad" },
        meter("Linear calls this hour", q.used, q.limit),
        note("Shared by every Linear call in this workspace; the dispatcher's issue reads count against it."),
        h("p", { class: "row-note", text: `People ${q.people}, agents ${q.agents}${q.resetsAt ? `. Resets ${ago(q.resetsAt)}` : ""}.` }),
        h("p", { text: `${q.queued} queued${q.queued ? ` (oldest ${Math.floor(q.oldestS / 60)} min)` : ""}, ${q.sending} sending, ${q.dead + q.expired} given up in 24 h, ${q.uncertain} uncertain.` })) : null,
      h("div", { class: "panel" }, q.writes.length === 0 ? emptyBlock("No queued writes.") : dataTable([{ head: "Write" }, { head: "State" }, { head: "Issue" }, { head: "Queued", right: true }],
        q.writes.map((w) => [mono(String(w.operation ?? w.id ?? "")), pill(String(w.state ?? "")), mono(String(w.issue ?? "")), ago(String(w.created_at ?? ""))]))));
  });
}

const PAGES: Record<string, (ctx: ViewCtx, host: HTMLElement) => void> = {
  connection: connectionPage, binding: bindingPage, tasks: tasksPage, intake: intakePage,
  cancel: cancelPage, status: statusPage, views: viewsPage, queue: queuePage,
};

export const linearView: View = (ctx: ViewCtx) => {
  const arg = routeArg(location.hash);
  const tab = arg in PAGES ? arg : "binding";
  const host = h("div", null);
  ctx.root.replaceChildren(
    titleRow("Linear", "Connection, binding and what flows through them. Read only in this build."),
    h("div", { class: "subnav" }, linkTabs(LINEAR_TABS, tab, "linear", "Linear")),
    host);
  (PAGES[tab] ?? bindingPage)(ctx, host);
};
