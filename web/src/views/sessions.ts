import type { SessionRow } from "../api";
import { CallError } from "../api";
import { ConnectionError } from "../transport";
import { h } from "../dom";
import { hhmm, plural, since } from "../format";
import { emptyBlock, errorBlock, safe, skBar, stamp, titleRow, type View, type ViewCtx } from "./common";

/** The TUI's state word: retired or moved, then stale, then presence. */
export function stateWord(s: SessionRow): string {
  if (s.status === "retired" || s.status === "moved") return s.status;
  if (s.stale) return "stale";
  return s.presence || s.status || "-";
}

function statePill(s: SessionRow): HTMLElement {
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

export function sessionsTable(rows: SessionRow[], now = new Date()): HTMLElement {
  const body = rows.map((s) =>
    h("tr", null,
      h("td", null, h("span", { class: "keyid", text: safe(s.name) })),
      h("td", { text: safe(s.agent) || "-" }),
      h("td", { class: "who" }, h("span", { class: "who-in" }, h("span", { class: "nm", text: safe(s.owner) || "Not recorded" }))),
      h("td", null, h("span", { class: "keyid m", text: safe(s.host) || "-" })),
      h("td", null, statePill(s)),
      h("td", null, s.issue ? h("span", { class: "keyid", text: safe(s.issue) }) : h("span", { class: "muted", text: "No claim" })),
      h("td", { class: "n", text: since(s.updatedAt, now) })));
  return h("div", { class: "table-wrap" }, h("table", null, h("thead", null, headRow()), h("tbody", null, body)));
}

export const sessionsView: View = (ctx: ViewCtx) => {
  const { root, api } = ctx;
  let lastGood = "";

  const load = () => {
    root.replaceChildren(titleRow("Sessions", "Loading sessions."), skeleton());
    api.sessions().then(
      (result) => {
        if (!ctx.isCurrent()) return;
        const rows = result.rows;
        const offline = result.source === "offline-mirror";
        // An offline copy with no time of its own says so; it never borrows now.
        const at = offline ? hhmm(result.mirrorAt) : hhmm(new Date().toISOString());
        if (at) lastGood = at;
        ctx.setOffline(offline ? at : null);
        const waiting = rows.filter((s) => stateWord(s) === "stale").length;
        const finding = rows.length === 0 ? "No agents connected." : `${plural(rows.length, "session")}${waiting ? `, ${waiting} stale` : ""}.`;
        root.replaceChildren(
          titleRow("Sessions", finding, [stamp(offline ? (at ? `Copy from ${at}` : "Saved copy") : `Updated ${at}`), h("button", { class: "btn sm", type: "button", on: { click: load }, text: "Refresh" })]),
          rows.length === 0
            ? emptyBlock("No agents connected.", "Start one in your terminal with a launch command from any of your sessions.")
            : sessionsTable(rows),
        );
      },
      (e: unknown) => {
        if (!ctx.isCurrent()) return;
        if (e instanceof ConnectionError) return ctx.connection(e.kind);
        const reason = e instanceof CallError ? e.message : String(e);
        root.replaceChildren(titleRow("Sessions"), errorBlock("sessions", lastGood, reason, load));
      },
    );
  };
  load();
};
