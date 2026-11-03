// Needs you (GUI-D-SPEC §12, GUI-DESIGN §4.5): one queue of things waiting on
// a person. The backend composes it from existing reads. Every action is
// listed but disabled in this build; writes arrive later.
import { CallError } from "../api";
import { ConnectionError } from "../transport";
import { h } from "../dom";
import { plural, since } from "../format";
import { KIND_LABEL, KIND_ONE, NEED_KINDS, parseNeeds, type KindStatus, type NeedAction, type NeedItem, type NeedKind, type Needs } from "../needs";
import { emptyBlock, errorBlock, safe, skBar, stamp, titleRow, type View, type ViewCtx } from "./common";

const NOT_YET = "Not in this build yet.";
const OLDEST_PHRASE: Record<NeedKind, string> = {
  escalation: "an escalation", claim: "a claim awaiting confirmation", intake: "an intake item", person: "an unlinked person", doctor: "a doctor warning",
};

export function kindPill(kind: NeedKind): HTMLElement {
  return h("span", { class: `pill ${kind === "doctor" ? "warn" : "acc"}`, "data-kind": kind, text: KIND_ONE[kind] });
}

/** Age of an item: nothing for a doctor check, which has no waiting time. */
function ageOf(n: NeedItem, now: Date): string {
  if (n.kind === "doctor" || !n.createdAt) return "";
  return since(n.createdAt, now);
}

/** "5 things need you; the oldest, an escalation on atlas/billing-retry, for 42 min." */
export function needsFinding(needs: Needs, now: Date = new Date()): string {
  const all = needs.items;
  if (all.length === 0) return needs.kinds.some((k) => !k.ok) ? "Nothing found in the kinds that could be read." : "Nothing needs you.";
  const dated = all.filter((n) => n.kind !== "doctor" && !Number.isNaN(new Date(n.createdAt).getTime()));
  const head = `${plural(all.length, "thing needs", "things need")} you`;
  if (dated.length === 0) return `${head}.`;
  const oldest = dated.reduce((a, b) => (new Date(b.createdAt).getTime() < new Date(a.createdAt).getTime() ? b : a));
  const where = oldest.project ? ` on ${safe(oldest.project)}` : "";
  return `${head}; the oldest, ${OLDEST_PHRASE[oldest.kind]}${where}, for ${since(oldest.createdAt, now) || "now"}.`;
}

/** A disabled action button: it says why on hover and to a screen reader. */
function actionButton(a: NeedAction, _first: boolean, title: string): HTMLButtonElement {
  // A disabled action never wears the filled primary look, so it cannot pass for a live one.
  return h("button", {
    class: "btn sm is-disabled", type: "button", disabled: true, "aria-disabled": "true", "data-write": "", "data-act": a.id,
    title: a.reason || NOT_YET, "aria-label": `${safe(a.label)}: ${safe(title)}. ${a.reason || NOT_YET}`, text: safe(a.label),
  });
}

/**
 * What the Viewing-as rules show. When the viewer may not approve a claim or
 * an escalation, the approve and release buttons give way to a sentence, and
 * Send to session stays (D-SPEC §12).
 */
export function shownActions(n: NeedItem): NeedAction[] {
  const approvals = n.kind === "claim" || n.kind === "escalation";
  if (approvals && n.actor.state === "no") return n.actions.filter((a) => a.id === "send-to-session");
  return n.actions;
}

function actionRow(n: NeedItem, max: number): HTMLElement {
  return h("div", { class: "acts" }, shownActions(n).slice(0, max).map((a, i) => actionButton(a, i === 0, n.title)));
}

function row(n: NeedItem, selected: boolean, now: Date, pick: () => void): HTMLElement {
  const el = h("div", { class: "nrow", tabindex: "0", "data-id": n.id, "data-kind": n.kind, "data-sel": String(selected), role: "button", "aria-pressed": String(selected) },
    h("div", { class: "l1" }, kindPill(n.kind), h("span", { class: "stamp", text: ageOf(n, now) })),
    h("div", { class: "ttl", text: safe(n.title) }),
    h("div", { class: "l3" }, n.project ? h("span", { class: "keyid m", text: safe(n.project) }) : null),
    actionRow(n, 3),
  );
  el.addEventListener("click", (e) => {
    if ((e.target as Element).closest("button")) return;
    pick();
  });
  el.addEventListener("keydown", (e) => {
    if ((e.key === "Enter" || e.key === " ") && e.target === el) {
      e.preventDefault();
      pick();
    }
  });
  return el;
}

const FACT_LABEL: Record<string, string> = {
  session: "Session", issue: "Issue", revision: "Revision", reason: "Reason", options: "Options", state: "State", offers: "Offers",
  signals: "Signals", linear_user: "Linear person", role: "Role", teams: "Teams", check: "Check", status: "Status", path: "Path",
};

function detail(n: NeedItem, now: Date): HTMLElement {
  const kv: (Node | string)[] = [];
  const add = (k: string, v: Node | string) => kv.push(h("dt", { text: k }), h("dd", null, v));
  // The fix text is shown as its own paragraph below; it is not a fact row.
  for (const [k, v] of n.facts) if (k !== "fix") add(FACT_LABEL[k] ?? safe(k), h("span", { class: "keyid", text: safe(v) }));
  if (n.project) add("Project", h("span", { class: "keyid", text: safe(n.project) }));
  const age = ageOf(n, now);
  if (age) add("Waiting", age);
  const fix = n.facts.find(([k]) => k === "fix")?.[1] ?? "";
  return h("div", { class: "ndetail", "data-detail": n.id },
    h("header", null, h("div", { class: "kind-line" }, kindPill(n.kind)), h("h2", { text: safe(n.title) })),
    h("div", { class: "nbody" },
      h("dl", { class: "kv" }, kv),
      n.preview ? h("div", { class: "stack" }, h("div", { class: "lbl stamp", text: "What the server says" }), h("p", { class: "preview", text: safe(n.preview) })) : null,
      fix ? h("div", { class: "stack" }, h("div", { class: "lbl stamp", text: "How to fix" }), h("p", { class: "preview", text: safe(fix) })) : null,
      n.actor.text ? h("p", { class: "who-may", "data-actor": n.actor.state, text: safe(n.actor.text) }) : null,
    ),
    h("div", { class: "nfoot" },
      actionRow(n, 9),
      h("p", { class: "note", text: NOT_YET })),
  );
}

function unavailableNotes(kinds: readonly KindStatus[]): HTMLElement | null {
  const down = kinds.filter((k) => !k.ok);
  if (down.length === 0) return null;
  return h("div", { class: "kind-notes", "data-block": "unavailable" }, down.map((k) =>
    h("div", { class: "notice plain", role: "status", "data-kind": k.kind },
      h("span", { class: "grow", text: `${KIND_LABEL[k.kind]} are unavailable${k.reason ? `: ${safe(k.reason)}` : "."}` }))));
}

/** The Needs you screen's body for a loaded queue. Exported for tests. */
export function needsBody(needs: Needs, state: { filter: NeedKind | "all"; sel: string }, repaint: () => void, now: Date = new Date()): HTMLElement {
  const all = needs.items;
  const root = h("div", null);
  const notes = unavailableNotes(needs.kinds);
  if (notes) root.append(notes);
  if (all.length === 0) {
    if (!needs.kinds.some((k) => !k.ok)) {
      root.append(h("div", { class: "empty", "data-main": "empty" }, h("b", { text: "Nothing needs you." }), h("span", { text: "Agents and Linear items that need a person show up here." })));
    }
    return root;
  }
  const count = (k: NeedKind) => all.filter((n) => n.kind === k).length;
  const kindsShown = NEED_KINDS.filter((k) => count(k) > 0 || needs.kinds.some((s) => s.kind === k && !s.ok));
  if (state.filter !== "all" && !kindsShown.includes(state.filter)) state.filter = "all";
  const chip = (id: NeedKind | "all", label: string, n: string) => {
    const b = h("button", { type: "button", "data-chip": id, "aria-pressed": String(state.filter === id) }, label, h("span", { class: "n", text: n }));
    b.addEventListener("click", () => { state.filter = id; repaint(); });
    return b;
  };
  root.append(h("div", { class: "fchips", role: "group", "aria-label": "Filter by kind" },
    chip("all", "All", String(all.length)),
    kindsShown.map((k) => chip(k, KIND_LABEL[k], needs.kinds.some((s) => s.kind === k && !s.ok) ? "–" : String(count(k))))));
  const items = state.filter === "all" ? all : all.filter((n) => n.kind === state.filter);
  const key = (n: NeedItem) => `${n.kind}/${n.id}`;
  if (!items.some((n) => key(n) === state.sel)) state.sel = items[0] ? key(items[0]) : "";
  const list = h("div", { class: "nlist", "aria-label": "Items that need you" },
    items.length ? items.map((n) => row(n, key(n) === state.sel, now, () => { state.sel = key(n); repaint(); })) : h("div", { class: "pad" }, emptyBlock("Nothing in this filter.")));
  const sel = items.find((n) => key(n) === state.sel);
  root.append(h("div", { class: "split open", "data-main": "main" }, list, sel ? detail(sel, now) : h("div", { class: "muted", text: "Select an item." })));
  return root;
}

export const needsView: View = (ctx: ViewCtx) => {
  const { root, api } = ctx;
  const state: { filter: NeedKind | "all"; sel: string } = { filter: "all", sel: "" };
  let lastGood = "";
  let needs: Needs | null = null;
  const body = h("div", null);
  const head = h("div", null);

  const paintHead = () => {
    if (!needs) return;
    head.replaceChildren(titleRow("Needs you", needsFinding(needs), [stamp(`Updated ${lastGood}`), h("button", { class: "btn sm", type: "button", on: { click: () => load(true) }, text: "Refresh" })]));
  };
  const paint = () => {
    if (!needs) return;
    paintHead();
    body.replaceChildren(needsBody(needs, state, paint));
  };
  const load = (loud: boolean) => {
    if (loud || !needs) {
      root.replaceChildren(titleRow("Needs you", "Loading."), h("div", { class: "panel pad", "aria-busy": "true" }, [320, 260].map((w) => h("div", { style: "margin:12px 0" }, skBar(w)))));
    }
    api.rpc("needs.list").then(
      (raw) => {
        if (!ctx.isCurrent()) return;
        needs = parseNeeds(raw);
        lastGood = new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
        ctx.setBadge("needs-you", needs.items.length);
        root.replaceChildren(head, body);
        paint();
      },
      (e: unknown) => {
        if (!ctx.isCurrent()) return;
        if (e instanceof ConnectionError) return ctx.connection(e.kind);
        root.replaceChildren(titleRow("Needs you"), errorBlock("Needs you", lastGood, e instanceof CallError ? e.message : String(e), () => load(true)));
      },
    );
  };
  // A needs.changed push reads the queue again without clearing the screen.
  const off = ctx.needsChanged?.(() => { if (!ctx.isCurrent()) return off?.(); load(false); });
  load(true);
};

/** The block on Overview: the top three, each with an Open button. */
export function needsOverviewBlock(needs: Needs | null, failed: string, go: () => void): HTMLElement {
  const head = (extra?: Node) => h("div", { class: "sec-head" }, h("h2", { text: "Needs you" }), extra ?? null);
  if (failed) {
    return h("section", { class: "section", "data-block": "needs" }, head(),
      h("div", { class: "panel needs-empty" }, h("b", { text: "Needs you couldn't be loaded." }), h("span", { text: safe(failed) })));
  }
  if (!needs) {
    return h("section", { class: "section", "data-block": "needs" }, head(), h("div", { class: "panel pad", "aria-busy": "true" }, skBar(260)));
  }
  const all = needs.items;
  const see = all.length > 3 ? h("button", { class: "btn sm ghost", type: "button", text: `See all ${all.length}`, on: { click: go } }) : undefined;
  const down = unavailableNotes(needs.kinds);
  if (all.length === 0) {
    return h("section", { class: "section", "data-block": "needs" }, head(),
      needs.kinds.some((k) => !k.ok) ? null : h("div", { class: "panel needs-empty" }, h("b", { text: "Nothing needs you" }), h("span", { text: "Agents and Linear items that need a person show up here." })),
      down);
  }
  const now = new Date();
  const rows = all.slice(0, 3).map((n) => h("div", { class: "row", "data-kind": n.kind },
    kindPill(n.kind),
    h("div", { class: "grow" }, h("div", { class: "name ttl", text: safe(n.title) }), h("div", { class: "line2", text: safe(n.project) })),
    h("span", { class: "stamp", text: ageOf(n, now) }),
    h("div", { class: "acts" }, h("button", { class: "btn sm", type: "button", text: "Open", "aria-label": `Open: ${safe(n.title)}`, on: { click: go } }))));
  return h("section", { class: "section", "data-block": "needs" }, head(see), h("div", { class: "panel rows ovneeds" }, rows), down);
}
