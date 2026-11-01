// Agent lanes (GUI-D-SPEC §3): one lane per session over the last 8 hours.
//
// What the data allows. sessions.list says a session's state now and when it
// last changed (updated_at), not its past states. So each lane has at most one
// bar: the session's current state from its last update (or the window's start)
// to now. The prototype also had "waiting on you" bars with the time waited,
// and a mark per past state; those need the message history and the needs
// queue, which come in later builds, so no such mark is drawn here.
//
// Everything is built with createElementNS and setAttribute/textContent. Colours
// come from CSS classes, so the theme and the CSP decide them, not this file.
import type { SessionRow } from "./api";
import { h, visible } from "./dom";
import { hhmm } from "./format";
import { stateWord } from "./sessionstate";

export const WINDOW_MS = 8 * 60 * 60 * 1000;
export const MAX_LANES = 12;
const SVGNS = "http://www.w3.org/2000/svg";

export type LaneKind = "working" | "idle" | "offline";

export interface Lane {
  name: string;
  issue: string;
  kind: LaneKind;
  /** The state word as the TUI says it ("stale", "retired", "active"...). */
  word: string;
  /** 0..1 across the window; null when the session says no update time. */
  from: number | null;
}

/** How a state word maps to a mark: working (pale), idle (grey), or offline (hatch). */
export function laneKind(word: string): LaneKind {
  if (word === "active" || word === "online" || word === "working") return "working";
  if (word === "idle") return "idle";
  return "offline";
}

const RANK: Record<LaneKind, number> = { working: 0, idle: 1, offline: 2 };

export function buildLanes(rows: readonly SessionRow[], now: Date): { lanes: Lane[]; more: number } {
  const start = now.getTime() - WINDOW_MS;
  const all = rows.map((s): Lane => {
    const word = stateWord(s);
    const t = new Date(s.updatedAt).getTime();
    const from = Number.isNaN(t) || t > now.getTime() ? null : Math.max(0, (t - start) / WINDOW_MS);
    return { name: s.name, issue: s.issue, kind: laneKind(word), word, from };
  });
  all.sort((a, b) => RANK[a.kind] - RANK[b.kind] || (b.from ?? -1) - (a.from ?? -1) || a.name.localeCompare(b.name));
  return { lanes: all.slice(0, MAX_LANES), more: Math.max(0, all.length - MAX_LANES) };
}

function el(parent: Element, tag: string, attrs: Record<string, string | number>, text?: string): SVGElement {
  const n = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, String(v));
  if (text !== undefined) n.textContent = text;
  parent.appendChild(n);
  return n as SVGElement;
}

const LABEL_W = 190;
const WIDTH = 960;
const HEAD = 26;
const PITCH = 44;
const BAR_H = 22;

const KIND_LABEL: Record<LaneKind, string> = { working: "Working", idle: "Idle", offline: "Offline or stale" };

/** The SVG and the legend (only the kinds on screen). onSelect gets the lane's session name. */
export function lanesChart(rows: readonly SessionRow[], now: Date, onSelect?: (name: string) => void): { svg: SVGElement; legend: HTMLElement; empty: boolean } {
  const { lanes, more } = buildLanes(rows, now);
  const height = HEAD + Math.max(1, lanes.length) * PITCH + 4;
  const svg = document.createElementNS(SVGNS, "svg") as SVGElement;
  svg.setAttribute("viewBox", `0 0 ${WIDTH} ${height}`);
  svg.setAttribute("role", "group");
  svg.setAttribute("class", "lanes-svg");
  svg.setAttribute("aria-label", lanes.length
    ? `Agent lanes for the last 8 hours: ${lanes.map((l) => `${visible(l.name)} ${visible(l.word)}`).join(", ")}`
    : "Agent lanes for the last 8 hours: no sessions");
  const defs = el(svg, "defs", {});
  const pat = el(defs, "pattern", { id: "cb-hatch", width: 6, height: 6, patternUnits: "userSpaceOnUse", patternTransform: "rotate(45)" });
  el(pat, "line", { x1: 0, y1: 0, x2: 0, y2: 6, class: "lane-hatch-line" });

  const x0 = LABEL_W;
  const cw = WIDTH - LABEL_W - 8;
  // Hour ticks every two hours, the labels as times of day.
  for (let i = 0; i <= 4; i++) {
    const x = x0 + (cw * i) / 4;
    el(svg, "line", { x1: x, x2: x, y1: HEAD, y2: height, class: "lane-grid" });
    const t = new Date(now.getTime() - WINDOW_MS + (WINDOW_MS * i) / 4);
    const label = i === 4 ? "Now" : t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
    el(svg, "text", { x, y: HEAD - 10, "text-anchor": i === 0 ? "start" : i === 4 ? "end" : "middle", class: i === 4 ? "lane-now-label" : "lane-axis" }, label);
  }
  lanes.forEach((l, i) => {
    const y0 = HEAD + i * PITCH;
    const g = el(svg, "g", onSelect ? { tabindex: 0, role: "button", class: "lane lane-pick", "aria-label": `Open session ${visible(l.name)}` } : { class: "lane" });
    el(g, "rect", { x: 0, y: y0, width: WIDTH, height: PITCH, class: "lane-hit" });
    const name = el(g, "text", { x: 0, y: y0 + (l.issue ? 18 : 27), class: "lane-name" }, clip(visible(l.name), 22));
    el(name, "title", {}, visible(l.name));
    if (l.issue) el(g, "text", { x: 0, y: y0 + 34, class: "lane-issue" }, clip(visible(l.issue), 26));
    if (l.from !== null) {
      const bx = x0 + cw * l.from;
      const bw = Math.max(4, x0 + cw - bx);
      const bar = el(g, "rect", { x: bx, y: y0 + (PITCH - BAR_H) / 2, width: bw, height: BAR_H, rx: 4, class: `lane-bar ${l.kind}` });
      el(bar, "title", {}, `${visible(l.name)}: ${visible(l.word)}`);
      const sinceIso = new Date(now.getTime() - (1 - l.from) * WINDOW_MS).toISOString();
      const text = l.from <= 0.001 ? `${visible(l.word)} · over 8 h` : `${visible(l.word)} · since ${hhmm(sinceIso)}`;
      if (bw > text.length * 7 + 16) el(g, "text", { x: bx + 8, y: y0 + PITCH / 2 + 4, class: `lane-bar-label ${l.kind}` }, text);
    }
    if (onSelect) {
      g.addEventListener("click", () => onSelect(l.name));
      g.addEventListener("keydown", (e) => {
        const k = (e as KeyboardEvent).key;
        if (k === "Enter" || k === " ") {
          e.preventDefault();
          onSelect(l.name);
        }
      });
    }
  });
  // "Now" is a 1px accent line at the right edge.
  el(svg, "line", { x1: x0 + cw, x2: x0 + cw, y1: HEAD, y2: height, class: "lane-now" });
  if (more > 0) el(svg, "text", { x: WIDTH - 8, y: height - 6, "text-anchor": "end", class: "lane-axis" }, `+${more} more in Sessions`);

  const used = new Set(lanes.filter((l) => l.from !== null).map((l) => l.kind));
  const legend = h("span", { class: "legend" }, (["working", "idle", "offline"] as LaneKind[]).filter((k) => used.has(k)).map((k) =>
    h("span", { class: "leg" }, h("i", { class: `sw ${k}` }), KIND_LABEL[k])));
  return { svg, legend, empty: lanes.length === 0 };
}

/** Cuts at n characters (not code units) with an ellipsis. */
export function clip(s: string, n: number): string {
  const chars = Array.from(s);
  return chars.length <= n ? s : chars.slice(0, n - 1).join("") + "…";
}
