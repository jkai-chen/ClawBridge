import type { SessionRow } from "../api";
import { CallError } from "../api";
import { ConnectionError } from "../transport";
import { h, rich } from "../dom";
import { hhmm, plural } from "../format";
import { lanesChart, laneKind } from "../lanes";
import { isArchived, stateWord } from "../sessionstate";
import { errorBlock, skBar, stamp, titleRow, type View, type ViewCtx } from "./common";

/** The counts the Today strip can compute from the session list; a cell the data cannot back is left out. */
export interface TodayCell {
  id: string;
  label: string;
  value: number;
}

export function todayCells(rows: readonly SessionRow[], queued: number | null): TodayCell[] {
  const cells: TodayCell[] = [
    { id: "working", label: "Sessions working", value: rows.filter((s) => laneKind(stateWord(s)) === "working").length },
    { id: "stale", label: "Stale", value: rows.filter((s) => stateWord(s) === "stale").length },
    { id: "claims", label: "Claims held", value: rows.filter((s) => s.issue !== "").length },
  ];
  if (queued !== null) cells.push({ id: "queued", label: "Writes queued", value: queued });
  return cells;
}

function stripEl(cells: readonly TodayCell[]): HTMLElement {
  return h("div", { class: "strip", "data-block": "today" }, cells.map((c) =>
    h("div", { "data-cell": c.id }, h("div", { class: "lab", text: c.label }), h("div", { class: "val", text: String(c.value) }))));
}

function secHead(title: string, meta?: string): HTMLElement {
  return h("div", { class: "sec-head" }, h("h2", { text: title }), meta ? h("span", { class: "meta", text: meta }) : null);
}

/**
 * The Needs-you queue (needs.list) is a later build, so this block must not say
 * nothing needs the person. It names the commands that list these today.
 */
function needsBlock(): HTMLElement {
  return h("section", { class: "section", "data-block": "needs" },
    secHead("Needs you"),
    h("div", { class: "panel needs-empty" },
      h("b", { text: "Needs you arrives in a later build." }),
      h("span", null, rich("Until then, check `clawbridge dispatcher escalations`, `clawbridge sessions --assignment-state proposed`, `clawbridge intake list` and `clawbridge doctor`.")),
    ),
  );
}

function skeleton(): HTMLElement {
  return h("div", { class: "panel pad", "aria-busy": "true" }, [320, 260, 200].map((w) => h("div", { style: "margin:12px 0" }, skBar(w))));
}

export const overviewView: View = (ctx: ViewCtx) => {
  const { root, api } = ctx;
  let lastGood = "";

  const load = () => {
    root.replaceChildren(titleRow("Overview", "Loading sessions."), skeleton());
    api.sessions().then(
      (result) => {
        if (!ctx.isCurrent()) return;
        const now = new Date();
        const offline = result.source === "offline-mirror";
        const at = offline ? hhmm(result.mirrorAt) : hhmm(now.toISOString());
        if (at) lastGood = at;
        ctx.setOffline(offline ? at : null);
        // Archived sessions are old news; they have no lane.
        const rows = result.rows.filter((s) => !isArchived(s, now));
        const working = rows.filter((s) => laneKind(stateWord(s)) === "working").length;
        const stale = rows.filter((s) => stateWord(s) === "stale").length;
        const finding = rows.length === 0
          ? "No agents connected."
          : `${plural(rows.length, "session")}: ${working} working${stale ? `, ${stale} stale` : ""}.`;
        const chart = lanesChart(rows, now, (name) => ctx.go(`sessions/${encodeURIComponent(name)}`));
        const strip = h("div", null, stripEl(todayCells(rows, ctx.outbox.get())));
        const off = ctx.outbox.subscribe((n) => {
          if (!ctx.isCurrent()) return off();
          strip.replaceChildren(stripEl(todayCells(rows, n)));
        });
        root.replaceChildren(
          titleRow("Overview", finding, [stamp(offline ? (at ? `Copy from ${at}` : "Saved copy") : `Updated ${at}`), h("button", { class: "btn sm", type: "button", on: { click: load }, text: "Refresh" })]),
          h("section", { class: "section", "data-block": "lanes" },
            secHead("Agent lanes", "Last 8 hours"),
            h("div", { class: "panel lanes" },
              h("div", { class: "chart" }, chart.svg),
              h("div", { class: "foot" }, chart.legend, h("span", { text: chart.empty ? "No sessions in this window" : "Select a lane to open the session" })),
              h("p", { class: "lanes-note", text: "Each bar is the session's state since its last update. Past states arrive in a later build." }),
            ),
          ),
          needsBlock(),
          h("section", { class: "section" }, secHead("Today"), strip),
        );
      },
      (e: unknown) => {
        if (!ctx.isCurrent()) return;
        if (e instanceof ConnectionError) return ctx.connection(e.kind);
        root.replaceChildren(titleRow("Overview"), errorBlock("the overview", lastGood, e instanceof CallError ? e.message : String(e), load));
      },
    );
  };
  load();
};
