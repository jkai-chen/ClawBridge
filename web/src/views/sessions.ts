import type { SessionRow } from "../api";
import { CallError } from "../api";
import { ConnectionError } from "../transport";
import { h, setChildren } from "../dom";
import { hhmm, plural, since } from "../format";
import { routeArg } from "../router";
import { inTab, isOwn, stateWord, type SessionTab } from "../sessionstate";
import { emptyBlock, errorBlock, safe, skBar, stamp, titleRow, type View, type ViewCtx } from "./common";
import { hostBadge, launchBlock } from "./launch";

export { stateWord };

export function statePill(s: SessionRow): HTMLElement {
  const word = stateWord(s);
  const cls = word === "stale" ? "warn" : word === "retired" || word === "moved" || word === "offline" ? "line" : word === "online" || word === "active" || word === "working" ? "pos" : "";
  return h("span", { class: `pill ${cls}`.trim() }, safe(word));
}

const HEAD = ["Session", "Agent", "Owner", "Host", "State", "Claim"];

function headRow(): HTMLElement {
  return h("tr", null, ...HEAD.map((t) => h("th", { text: t })), h("th", { class: "n", text: "Updated" }));
}

function skeleton(): HTMLElement {
  const rows = [150, 120, 170, 140].map((w) =>
    h("tr", null, h("td", null, skBar(w)), h("td", null, skBar(56)), h("td", null, skBar(110)), h("td", null, skBar(80)), h("td", null, skBar(72)), h("td", null, skBar(90)), h("td", { class: "n" }, skBar(40))));
  return h("div", { class: "table-wrap", "aria-busy": "true" }, h("table", null, h("thead", null, headRow()), h("tbody", null, rows)));
}

export function sessionsTable(rows: SessionRow[], now = new Date(), opts: { selected?: string; onPick?: (s: SessionRow) => void } = {}): HTMLElement {
  const body = rows.map((s) => {
    const tr = h("tr", {
      class: opts.onPick ? "pick" : "",
      tabindex: opts.onPick ? "0" : null,
      "aria-selected": opts.onPick ? String(opts.selected === s.name) : null,
      "data-session": s.id || null,
    },
      h("td", null, h("span", { class: "keyid", text: safe(s.name) })),
      h("td", { text: safe(s.agent) || "-" }),
      h("td", { class: "who" }, h("span", { class: "who-in" }, h("span", { class: "nm", text: safe(s.owner) || "Not recorded" }))),
      h("td", null, h("span", { class: "keyid m", text: safe(s.host) || "-" })),
      h("td", null, statePill(s)),
      h("td", null, s.issue ? h("span", { class: "keyid", text: safe(s.issue) }) : h("span", { class: "muted", text: "No claim" })),
      h("td", { class: "n", text: since(s.updatedAt, now) }));
    if (opts.onPick) {
      const pick = opts.onPick;
      tr.addEventListener("click", () => pick(s));
      tr.addEventListener("keydown", (e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          pick(s);
        }
      });
    }
    return tr;
  });
  return h("div", { class: "table-wrap" }, h("table", null, h("thead", null, headRow()), h("tbody", null, body)));
}

/** A filter box matches the name, owner, host, agent, claim and state. */
export function matches(s: SessionRow, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return [s.name, s.owner, s.host, s.agent, s.issue, stateWord(s)].some((f) => f.toLowerCase().includes(q));
}

export function drawer(ctx: ViewCtx, s: SessionRow, now: Date, close: () => void): HTMLElement {
  const own = isOwn(s, ctx.hello?.principal ?? "");
  const closeBtn = h("button", { class: "btn sm ghost", type: "button", "aria-label": "Close details", text: "Close" });
  closeBtn.addEventListener("click", close);
  const badge = hostBadge(s.host, ctx.hello?.device ?? "");
  const facts = h("dl", { class: "kv" },
    h("dt", { text: "Status" }), h("dd", null, statePill(s)),
    h("dt", { text: "Claim" }), h("dd", null, s.issue ? h("span", { class: "keyid", text: safe(s.issue) }) : h("span", { class: "muted", text: "No claim" })),
    h("dt", { text: "Owner" }), h("dd", { text: safe(s.owner) || "Not recorded" }),
    h("dt", { text: "Host" }), h("dd", null, h("span", { class: "keyid m", text: safe(s.host) || "-" }), badge ? " " : null, badge),
    h("dt", { text: "Agent" }), h("dd", { text: safe(s.agent) || "-" }),
    h("dt", { text: "Updated" }), h("dd", { text: s.updatedAt ? `${since(s.updatedAt, now)} ago` : "-" }),
  );
  const who = safe(s.owner);
  const tail = own
    ? launchBlock(ctx, s)
    : h("p", { class: "readonly-note", "data-block": "readonly" }, who
      ? `This is ${who}'s agent. Only ${who} can start it. Messaging and routing arrive in a later build.`
      : "The owner isn't recorded, so no launch command is offered.");
  return h("aside", { class: "drawer", "aria-label": `Session ${safe(s.name)}` },
    h("div", { class: "hd" }, h("h2", { text: safe(s.name) }), closeBtn),
    facts,
    tail,
  );
}

const TABS: [SessionTab, string][] = [["current", "Current"], ["archive", "Archive"], ["all", "All"]];

export const sessionsView: View = (ctx: ViewCtx) => {
  const { root, api } = ctx;
  let lastGood = "";
  let all: SessionRow[] = [];
  let tab: SessionTab = "current";
  let query = "";
  let selected = routeArg(location.hash);
  let now = new Date();
  const detail = h("div", { class: "detail" });
  const results = h("div", { class: "results" });
  const tabsEl = h("div", { class: "tabs", role: "tablist" });
  const split = h("div", { class: "split" }, results, detail);

  const close = () => {
    selected = "";
    paint();
  };
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape" && selected) close();
  };

  const paint = () => {
    // Re-open a drawer for a session that is still there, whichever tab it is in.
    const sel = all.find((s) => s.name === selected);
    if (!sel) selected = "";
    tabsEl.replaceChildren(...TABS.map(([id, label]) => {
      const n = all.filter((s) => inTab(s, id, now)).length;
      const b = h("button", { type: "button", role: "tab", "aria-selected": String(id === tab) }, label, h("span", { class: "n", text: String(n) }));
      b.addEventListener("click", () => { tab = id; paint(); });
      return b;
    }));
    const rows = all.filter((s) => inTab(s, tab, now) && matches(s, query));
    const body = all.length === 0
      ? emptyBlock("No agents connected.", "Start one in your terminal with a launch command from any of your sessions.")
      : rows.length === 0
        ? emptyBlock(query ? "No session matches that filter." : tab === "archive" ? "Nothing in the Archive." : "No sessions here.")
        : sessionsTable(rows, now, { selected, onPick: (s) => { selected = s.name; paint(); } });
    results.replaceChildren(body);
    split.classList.toggle("open", Boolean(sel));
    detail.replaceChildren(...(sel ? [drawer(ctx, sel, now, close)] : []));
  };

  const load = () => {
    root.replaceChildren(titleRow("Sessions", "Loading sessions."), skeleton());
    api.sessions().then(
      (result) => {
        if (!ctx.isCurrent()) return;
        all = result.rows;
        now = new Date();
        const offline = result.source === "offline-mirror";
        // An offline copy with no time of its own says so; it never borrows now.
        const at = offline ? hhmm(result.mirrorAt) : hhmm(new Date().toISOString());
        if (at) lastGood = at;
        ctx.setOffline(offline ? at : null);
        const stale = all.filter((s) => stateWord(s) === "stale").length;
        const finding = all.length === 0 ? "No agents connected." : `${plural(all.length, "session")}${stale ? `, ${stale} stale` : ""}.`;
        const stampText = offline ? (at ? `Copy from ${at}` : "Saved copy") : `Updated ${at}`;
        const filter = h("input", { class: "input", type: "search", placeholder: "Filter sessions", "aria-label": "Filter sessions", value: query });
        filter.addEventListener("input", () => { query = filter.value; paint(); });
        setChildren(root,
          titleRow("Sessions", finding, [stamp(stampText), h("button", { class: "btn sm", type: "button", on: { click: load }, text: "Refresh" })]),
          all.length ? tabsEl : null,
          all.length ? h("div", { class: "toolbar" }, filter) : null,
          split,
        );
        paint();
      },
      (e: unknown) => {
        if (!ctx.isCurrent()) return;
        if (e instanceof ConnectionError) return ctx.connection(e.kind);
        const reason = e instanceof CallError ? e.message : String(e);
        root.replaceChildren(titleRow("Sessions"), errorBlock("sessions", lastGood, reason, load));
      },
    );
  };
  root.addEventListener("keydown", onKey);
  load();
};
