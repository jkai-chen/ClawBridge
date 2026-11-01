// The Launch block of a session's drawer (GUI-DESIGN §4.4): the exact line
// the backend built, shown before it is copied. The page never builds it.
import { CallError, type LaunchAgent, type LaunchLine, type SessionRow } from "../api";
import { ConnectionError } from "../transport";
import { copyText, h, setChildren, visible } from "../dom";
import type { ViewCtx } from "./common";
import { safe, skBar } from "./common";

/** Selects the node's text so a person can press the copy shortcut. */
export function selectText(node: Node): void {
  const sel = window.getSelection();
  if (!sel) return;
  const range = document.createRange();
  range.selectNodeContents(node);
  sel.removeAllRanges();
  sel.addRange(range);
}

/** The host badge: "this device" when the host is this device's, else "on HOST". */
export function hostBadge(host: string, device: string): HTMLElement | null {
  if (!host) return null;
  return host === device && device !== ""
    ? h("span", { class: "pill acc", text: "this device" })
    : h("span", { class: "pill line", text: `on ${safe(host)}` });
}

const NOTE = "Paste into your own terminal. No keys or tokens are in this command.";

export function launchBlock(ctx: ViewCtx, s: SessionRow): HTMLElement {
  let agent: LaunchAgent = s.agent === "codex" ? "codex" : "claude-code";
  let token = 0;
  const body = h("div", { class: "launch-body" });
  const seg = h("div", { class: "segmented", role: "group", "aria-label": "Agent" });
  const buttons = new Map<LaunchAgent, HTMLButtonElement>();
  for (const [id, label] of [["claude-code", "Claude Code"], ["codex", "Codex"]] as const) {
    const b = h("button", { type: "button", "aria-pressed": String(id === agent), text: label });
    b.addEventListener("click", () => {
      if (agent === id) return;
      agent = id;
      for (const [k, x] of buttons) x.setAttribute("aria-pressed", String(k === agent));
      load();
    });
    buttons.set(id, b);
    seg.append(b);
  }
  const head = h("div", { class: "tools" }, h("h3", { text: "Launch" }), seg);

  const show = (r: LaunchLine) => {
    const code = h("pre", { class: "launch-line", tabindex: "0", text: visible(r.line) });
    const status = h("span", { class: "note", role: "status" });
    // Only exactly what is shown is ever copied: a line with hidden
    // characters is shown marked and offered no Copy.
    const hidden = visible(r.line) !== r.line;
    const copy = hidden ? null : h("button", { class: "btn sm", type: "button", text: "Copy launch command" });
    if (hidden) status.textContent = "This line holds hidden characters; it was not copied.";
    copy?.addEventListener("click", () => {
      void copyText(r.line).then((ok) => {
        if (ok) {
          status.textContent = "Copied.";
        } else {
          selectText(code);
          status.textContent = "Couldn't reach the clipboard. The line is selected: press Ctrl+C (⌘C on a Mac) to copy it.";
        }
      });
    });
    const badge = hostBadge(r.host || s.host, ctx.hello?.device ?? "");
    setChildren(body,
      code,
      h("div", { class: "tools" }, copy, badge),
      status,
      r.warnings.map((w) => h("div", { class: "notice", role: "alert", "data-warning": "" }, h("div", { class: "grow", text: safe(w) }))),
      h("p", { class: "note", text: NOTE }),
    );
  };

  const load = () => {
    const mine = ++token;
    body.replaceChildren(h("div", { "aria-busy": "true" }, skBar(320)));
    ctx.api.launchCommand(s.name, agent).then(
      (r) => { if (mine === token && ctx.isCurrent()) show(r); },
      (e: unknown) => {
        if (mine !== token || !ctx.isCurrent()) return;
        if (e instanceof ConnectionError) return ctx.connection(e.kind);
        body.replaceChildren(h("div", { class: "errblock", role: "alert" }, h("div", { text: `No launch command. ${safe(e instanceof CallError ? e.message : String(e))}` })));
      },
    );
  };
  load();
  return h("section", { class: "block", "data-block": "launch" }, head, body);
}
