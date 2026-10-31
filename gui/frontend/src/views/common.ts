import type { Api, BackendStatus } from "../api";
import { copyText, h, rich, visible, type Child } from "../dom";
import { icon } from "../icons";

/** What a view gets when it is mounted into the sheet. */
export interface ViewCtx {
  root: HTMLElement;
  api: Api;
  status: BackendStatus;
  /** False once the route or the backend state changed: drop late results. */
  isCurrent(): boolean;
  setBadge(nav: string, n: number): void;
  go(path: string): void;
}

export type View = (ctx: ViewCtx) => void;

export function titleRow(title: string, finding?: Child, right?: Child): HTMLElement {
  return h("div", { class: "title-row" },
    h("div", null, h("h1", { text: title }), finding ? h("p", { class: "finding" }, finding) : null),
    right ? h("div", { class: "right" }, right) : null,
  );
}

export function emptyBlock(headline: string, detail?: string): HTMLElement {
  return h("div", { class: "empty" }, h("b", { text: headline }), detail ? h("span", { text: detail }) : null);
}

/** One sentence plus Retry, as in GUI-D-SPEC §7. */
export function errorBlock(what: string, lastUpdated: string, reason: string, retry: () => void): HTMLElement {
  const last = lastUpdated ? ` Last updated ${lastUpdated}.` : "";
  return h("div", { class: "errblock", role: "alert" },
    icon("warn"),
    h("div", null,
      h("div", { text: `Couldn't load ${what}.${last}` }),
      reason ? h("div", { class: "row-note", text: visible(reason) }) : null,
    ),
    h("button", { class: "btn sm", type: "button", style: "margin-left:auto", on: { click: retry }, text: "Retry" }),
  );
}

/** A grey bar at a real size, no shimmer (GUI-D-SPEC §7). */
export function skBar(width: number): HTMLElement {
  return h("span", { class: "sk-bar", style: `width:${width}px`, "aria-hidden": "true" });
}

export function copyButton(text: string, label = "Copy"): HTMLButtonElement {
  const b = h("button", { class: "btn sm", type: "button", text: label });
  b.addEventListener("click", () => {
    void copyText(text).then((ok) => {
      b.textContent = ok ? "Copied" : "Copy blocked";
      setTimeout(() => { b.textContent = label; }, 1500);
    });
  });
  return b;
}

export function codebox(command: string): HTMLElement {
  return h("div", { class: "codebox" }, h("code", { text: command }), copyButton(command));
}

export function stamp(text: string): HTMLElement {
  return h("span", { class: "stamp", text });
}

/** Text from the backend or a remote system, made safe to read. */
export function safe(text: unknown): string {
  return visible(text);
}

export { rich };
