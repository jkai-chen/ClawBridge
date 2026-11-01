// Projects (GUI-D-SPEC §5.4): the list, and a project page with Overview
// and Threads. Threads is a person-only, owner-only read, as in the TUI.
import { CallError } from "../api";
import { h } from "../dom";
import { plural } from "../format";
import { routeArg } from "../router";
import { parseProjects, parseThread, parseThreads, type Overview, type ProjectRow, type ThreadRow } from "../pages";
import { emptyBlock, safe, titleRow, type View, type ViewCtx } from "./common";
import { ago, buttonTabs, call, dash, dataTable, loadInto, mono, note, pill, plainText } from "./pagekit";

export const THREADS_REFUSED = "Only a project owner can read threads";
export const THREADS_RATE_LIMITED = "Too many thread reads in the last minute; try again shortly.";

/** The server's rate-limit refusal (internal/bus/threads.go: code rate_limited, "limited to 30 a minute"). */
export function isRateLimited(e: unknown): boolean {
  return e instanceof Error && /rate_limited|limited to \d+ a minute/i.test(e.message);
}

// Every project.threads and project.thread call writes an audit row and
// counts toward the server's 30-a-minute limit. They are made only from an
// explicit click (opening the Threads tab or a thread, Older threads, Load
// more, Retry): never from a timer, an event, a re-render or a focus change.

export function boardSummary(o: Overview | null): string {
  if (!o) return "Not read";
  if (o.error) return "Couldn't read";
  if (o.linear.unreadable) return "Unreadable";
  if (!o.linear.configured) return "No Linear board";
  const teams = o.linear.teams.map((t) => t.name || t.id).join(", ");
  return teams ? `Linear: ${teams}` : "Linear board linked";
}

function list(ctx: ViewCtx, rows: ProjectRow[]): HTMLElement {
  if (rows.length === 0) return emptyBlock("No projects yet.", "Projects you are in show up here.");
  return dataTable(
    [{ head: "Project" }, { head: "Role" }, { head: "Board" }, { head: "Coordinator" }, { head: "Sessions", right: true }],
    rows.map((p) => [
      h("div", null, h("div", { text: safe(p.name) || safe(p.slug) }), mono(p.slug || p.id)),
      pill(p.role || "member", p.role === "owner" ? "acc" : ""),
      safe(boardSummary(p.overview)),
      p.overview?.coordinator ? safe(p.overview.coordinator) : "-",
      p.overview && !p.overview.error ? String(p.overview.active) : "-",
    ]),
    { onPick: (i) => { const p = rows[i]; if (p) ctx.go(`projects/${encodeURIComponent(p.slug || p.id)}`); } },
  );
}

function overviewTab(p: ProjectRow): HTMLElement {
  const o = p.overview;
  if (!o) return emptyBlock("The overview wasn't read.", "Only the first 20 projects are read.");
  if (o.error) return h("div", { class: "errblock", role: "alert" }, h("div", { text: "Couldn't read this project's overview." }), note(o.error));
  const l = o.linear;
  return h("div", { class: "stack" },
    h("div", { class: "panel pad" }, h("dl", { class: "kv" },
      h("dt", { text: "Your role" }), h("dd", { text: dash(p.role) }),
      h("dt", { text: "Coordinator" }), h("dd", { text: o.coordinator ? `${safe(o.coordinator)}${o.coordinatorStatus ? ` (${safe(o.coordinatorStatus)})` : ""}` : "None" }),
      h("dt", { text: "Sessions" }), h("dd", { text: `${o.active} active, ${o.retired} retired, ${o.moved} moved, ${o.stale} stale` }),
      h("dt", { text: "Repository" }), h("dd", null, mono(p.gitRemote)),
    )),
    h("div", { class: "panel pad" }, h("h2", { text: "Linear board" }), l.configured
      ? h("dl", { class: "kv" },
        h("dt", { text: "Mode" }), h("dd", { text: dash(l.mode) }),
        h("dt", { text: "Teams" }), h("dd", { text: l.teams.map((t) => safe(t.name || t.id) + (t.isDefault ? " (default)" : "")).join(", ") || "-" }),
        h("dt", { text: "Projects" }), h("dd", { text: l.projects.map((x) => safe(x.name || x.id) + (x.closes ? ` (closes: ${safe(x.closes)})` : "")).join(", ") || "-" }),
        h("dt", { text: "Agents may cancel" }), h("dd", { text: l.cancelKinds === null ? "Not reported" : l.cancelKinds.length ? l.cancelKinds.map(safe).join(", ") : "No kinds" }),
        h("dt", { text: "Names stored" }), h("dd", { text: l.namesAt ? ago(l.namesAt) : "-" }))
      : h("p", { class: "muted", text: l.unreadable ? "The board couldn't be read." : "No Linear board is linked to this project." })),
    o.sessions.length
      ? h("div", { class: "panel" }, dataTable([{ head: "Session" }, { head: "Owner" }, { head: "Agent" }, { head: "Status" }],
        o.sessions.map((s) => [mono(s.name), dash(s.owner), dash(s.agent), dash(s.status)])))
      : null,
  );
}

function threadsTab(ctx: ViewCtx, p: ProjectRow): HTMLElement {
  const host = h("div", { class: "stack" });
  const project = p.slug || p.id;
  let rows: ThreadRow[] = [];
  let next = "";
  let selected = -1;

  const refused = (reason: string): HTMLElement =>
    h("div", { class: "empty", "data-state": "refused" }, h("b", { text: THREADS_REFUSED }), reason ? note(reason) : null);

  if (p.role !== "owner") {
    host.append(refused(""));
    return host;
  }

  const thread = h("div", { class: "panel" });
  const listEl = h("div", { class: "panel" });
  const showThread = (t: ThreadRow): void => {
    let after = "";
    let shown: ReturnType<typeof parseThread>["messages"] = [];
    thread.replaceChildren();
    const body = h("div", null);
    const moreBtn = h("button", { class: "btn sm", type: "button", text: "Load more", hidden: true });
    const paint = (): void => {
      body.replaceChildren(...shown.map((m) => h("div", { class: "msg" },
        h("div", { class: "meta" }, mono(m.sender), "to", mono(m.address), pill(m.kind || "message"), m.sent ? h("span", { text: ago(m.sent) }) : null,
          m.recipients.length ? h("span", { text: m.recipients.map((r) => `${safe(r.name)}${r.readAt ? " (read)" : r.deliveredAt ? " (delivered)" : ""}`).join(", ") }) : null),
        plainText(m.body))));
    };
    const fetchPage = (): void => {
      const read = async () => {
        try {
          return await call(ctx, "project.thread", { project, thread_id: t.id, ...(after ? { after } : {}) }, parseThread)();
        } catch (e) {
          throw isRateLimited(e) ? new CallError(THREADS_RATE_LIMITED) : e;
        }
      };
      loadInto(ctx, body, "this thread", read, (r) => {
        shown = shown.concat(r.messages);
        after = r.next > 0 ? String(r.next) : "";
        moreBtn.hidden = !after;
        queueMicrotask(paint);
        return h("div");
      });
    };
    moreBtn.addEventListener("click", fetchPage);
    thread.append(h("div", { class: "sec-head", style: "padding:12px 16px 0" }, h("h2", { text: "Thread" }), mono(t.id)), body, h("div", { style: "padding:8px 16px 12px" }, moreBtn));
    fetchPage();
  };

  const paintList = (): void => {
    listEl.replaceChildren(rows.length === 0
      ? emptyBlock("No threads yet.", "Conversations between this project's sessions show up here.")
      : dataTable([{ head: "Thread" }, { head: "Kind" }, { head: "Participants" }, { head: "Messages", right: true }, { head: "Last", right: true }],
        rows.map((t) => [mono(t.id), pill(t.kind || "thread"), t.participants.map(safe).join(", ") + (t.more ? ` and ${t.more} more` : ""), String(t.messages), ago(t.lastAt)]),
        { selected, onPick: (i) => { selected = i; paintList(); const t = rows[i]; if (t) showThread(t); } }));
    if (next) {
      const b = h("button", { class: "btn sm", type: "button", text: "Older threads" });
      b.addEventListener("click", () => fetchList(next));
      listEl.append(h("div", { style: "padding:8px 16px 12px" }, b));
    }
  };

  const fetchList = (before: string): void => {
    loadInto(ctx, listEl, "the threads", async () => {
      try {
        return parseThreads(await ctx.api.rpc("project.threads", { project, ...(before ? { before } : {}) }));
      } catch (e) {
        // A refusal is the server's: the project's owner reads threads.
        if (e instanceof Error && e.name === "CallError") return isRateLimited(e) ? new CallError(THREADS_RATE_LIMITED) : e;
        throw e;
      }
    }, (r, reload) => {
      if (r instanceof Error) {
        listEl.replaceChildren();
        if (r.message === THREADS_RATE_LIMITED) {
          return h("div", { class: "errblock", role: "alert" }, h("div", { text: THREADS_RATE_LIMITED }),
            h("button", { class: "btn sm", type: "button", style: "margin-left:auto", on: { click: reload }, text: "Retry" }));
        }
        const retry = h("button", { class: "btn sm", type: "button", text: "Retry", on: { click: reload } });
        return h("div", null, refused(r.message), h("div", { style: "padding:8px 16px 12px" }, retry));
      }
      rows = before ? rows.concat(r.rows) : r.rows;
      next = r.next;
      queueMicrotask(paintList);
      return h("div");
    });
  };
  host.append(note("Opening a thread is recorded in the project's history."), listEl, thread);
  fetchList("");
  return host;
}

function projectPage(ctx: ViewCtx, p: ProjectRow): void {
  let tab = "overview";
  const body = h("div", null);
  const tabs = h("div", null);
  const paint = (): void => {
    tabs.replaceChildren(buttonTabs([["overview", "Overview"], ["threads", "Threads"]], tab, (id) => { tab = id; paint(); }, "Project"));
    body.replaceChildren(tab === "overview" ? overviewTab(p) : threadsTab(ctx, p));
  };
  const back = h("a", { class: "btn sm", href: "#/projects", text: "All projects" });
  ctx.root.replaceChildren(
    titleRow(safe(p.name) || safe(p.slug), [mono(p.slug || p.id), " ", pill(p.role || "member", p.role === "owner" ? "acc" : "")], back),
    tabs, body);
  paint();
}

export const projectsView: View = (ctx: ViewCtx) => {
  const { root } = ctx;
  const arg = routeArg(location.hash);
  const host = h("div", null);
  root.replaceChildren(titleRow("Projects", "Loading projects."), host);
  loadInto(ctx, host, "the projects", call(ctx, "projects.list", {}, parseProjects), ({ rows }, reload) => {
    const p = arg ? rows.find((r) => r.slug === arg || r.id === arg) : undefined;
    if (p) {
      queueMicrotask(() => { if (ctx.isCurrent()) projectPage(ctx, p); });
      return h("div");
    }
    const owned = rows.filter((r) => r.role === "owner").length;
    root.firstElementChild?.replaceWith(titleRow("Projects", rows.length ? `${plural(rows.length, "project")}, ${owned} you own.` : "No projects yet.",
      h("button", { class: "btn sm", type: "button", on: { click: reload }, text: "Refresh" })));
    return arg ? [emptyBlock("That project isn't in your list."), list(ctx, rows)] : list(ctx, rows);
  });
};
