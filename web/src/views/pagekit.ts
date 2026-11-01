// Shared pieces of the P2-5 read pages: a block that loads, fails and
// retries (GUI-D-SPEC §7), tables, the disabled write control and tab bars.
import { CallError } from "../api";
import { ConnectionError } from "../transport";
import { h, type Child } from "../dom";
import { errorBlock, safe, skBar, type ViewCtx } from "./common";

export const NOT_YET = "Not in this build yet.";

/** Loading bars at real sizes. */
export function skeleton(): HTMLElement {
  return h("div", { class: "panel pad", "aria-busy": "true" }, [220, 300, 180].map((w) => h("div", { style: "margin:10px 0" }, skBar(w))));
}

/**
 * Fills host: loading bars, then render(value), or one sentence plus Retry.
 * A lost or refused connection goes to the shell's state instead.
 */
export function loadInto<T>(ctx: ViewCtx, host: HTMLElement, what: string, fetch: () => Promise<T>, render: (value: T, reload: () => void) => Child): void {
  const run = (): void => {
    host.replaceChildren(skeleton());
    fetch().then(
      (v) => {
        if (!ctx.isCurrent()) return;
        host.replaceChildren();
        const out = render(v, run);
        if (out instanceof Node) host.append(out);
        else if (Array.isArray(out)) host.append(...out.filter((n): n is Node => n instanceof Node));
      },
      (e: unknown) => {
        if (!ctx.isCurrent()) return;
        if (e instanceof ConnectionError) return ctx.connection(e.kind);
        host.replaceChildren(errorBlock(what, "", e instanceof CallError ? e.message : String(e), run));
      },
    );
  };
  run();
}

export function call<T>(ctx: ViewCtx, method: string, params: Record<string, unknown>, parse: (raw: unknown) => T): () => Promise<T> {
  return async () => parse(await ctx.api.rpc(method, params));
}

export interface Col { head: string; right?: boolean }

export function dataTable(cols: Col[], rows: Child[][], opts: { onPick?: (i: number) => void; selected?: number } = {}): HTMLElement {
  const body = rows.map((cells, i) => {
    const tr = h("tr", { class: opts.onPick ? "pick" : "", tabindex: opts.onPick ? "0" : null, "aria-selected": opts.onPick ? String(opts.selected === i) : null },
      cells.map((c, j) => h("td", { class: cols[j]?.right ? "n" : "" }, c)));
    if (opts.onPick) {
      const pick = opts.onPick;
      tr.addEventListener("click", () => pick(i));
      tr.addEventListener("keydown", (e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          pick(i);
        }
      });
    }
    return tr;
  });
  return h("div", { class: "table-wrap" }, h("table", null,
    h("thead", null, h("tr", null, cols.map((c) => h("th", { class: c.right ? "n" : "", text: c.head })))),
    h("tbody", null, body)));
}

export const mono = (t: string): HTMLElement => h("span", { class: "keyid", text: safe(t) || "-" });
export const dash = (t: string): string => safe(t) || "-";

/** A write control this build does not have: disabled, with the reason beside it. */
export function disabledWrite(label: string): HTMLElement {
  return h("span", { class: "disabled-write" },
    h("button", { class: "btn sm", type: "button", disabled: true, title: NOT_YET, "aria-label": `${label}. ${NOT_YET}`, text: label }),
    h("span", { class: "row-note", text: NOT_YET }));
}

/** "5 min ago" or "in 6 d" for an ISO time, "-" when absent, the text itself when unreadable. */
export function ago(iso: string, now: Date = new Date()): string {
  if (!iso) return "-";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return safe(iso);
  const m = Math.floor(Math.abs(now.getTime() - t) / 60000);
  const span = m < 1 ? "" : m < 60 ? `${m} min` : m < 1440 ? `${Math.floor(m / 60)} h` : `${Math.floor(m / 1440)} d`;
  if (!span) return "just now";
  return t > now.getTime() ? `in ${span}` : `${span} ago`;
}

export function pill(text: string, tone = ""): HTMLElement {
  return h("span", { class: `pill ${tone}`.trim(), text: safe(text) });
}

/** Link tabs: the selected one is aria-current/aria-selected. */
export function linkTabs(items: [string, string][], selected: string, base: string, label: string): HTMLElement {
  return h("div", { class: "tabs", role: "tablist", "aria-label": label },
    items.map(([id, text]) => h("a", { href: `#/${base}/${id}`, role: "tab", "aria-selected": String(id === selected), text })));
}

/** In-page tabs (no route change). */
export function buttonTabs(items: [string, string][], selected: string, onPick: (id: string) => void, label: string): HTMLElement {
  return h("div", { class: "tabs", role: "tablist", "aria-label": label },
    items.map(([id, text]) => h("button", { type: "button", role: "tab", "aria-selected": String(id === selected), on: { click: () => onPick(id) }, text })));
}

/** A bar for a real used/limit pair. */
export function meter(label: string, used: number, limit: number): HTMLElement {
  const pct = limit > 0 ? Math.min(100, Math.round((used / limit) * 100)) : 0;
  const tone = limit > 0 && used >= limit ? "full" : limit > 0 && pct >= 80 ? "high" : "";
  return h("div", { class: "meter-row" },
    h("div", { class: "meter-lab" }, h("span", { text: label }), h("span", { class: "num", text: limit > 0 ? `${used} of ${limit}` : `${used} (no cap)` })),
    limit > 0 ? h("div", { class: `meter ${tone}`.trim(), role: "meter", "aria-valuemin": "0", "aria-valuemax": String(limit), "aria-valuenow": String(used), "aria-label": label }, h("span", { style: `width:${pct}%` })) : null);
}

export function note(text: string): HTMLElement {
  return h("p", { class: "row-note", text: safe(text) });
}

export function plainText(text: string): HTMLElement {
  return h("div", { class: "plain-text", text: safe(text) });
}
