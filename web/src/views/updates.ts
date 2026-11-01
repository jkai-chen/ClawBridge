// Settings → Updates, read-only (GUI-DESIGN §8.1, §12.5; GUI-D-SPEC §12). It
// reads versions, and checks for a release only when the person clicks
// Check now. The toggle, Update now and Revert are disabled until writes
// arrive. There is no GUI app row: the UI is inside the client binary.
import { CallError } from "../api";
import { ConnectionError } from "../transport";
import { h, rich, visible } from "../dom";
import { icon } from "../icons";
import { hhmm } from "../format";
import { parseUpdateCheck, parseUpdatesStatus, type UpdateCheck, type UpdatesStatus } from "../needs";
import { codebox, errorBlock, safe, skBar, type ViewCtx } from "./common";

const NOT_YET = "Not in this build yet.";
const AUTO_OFF = "Automatic updates aren't available in this build";

export interface UpdatesModel {
  status: UpdatesStatus;
  /** The last Check now answer, or null before the first click. */
  check: UpdateCheck | null;
  /** The last Check now failure ("" if none): nothing was installed or changed. */
  checkError: string;
  checking: boolean;
  platform: string;
  distro: string;
}

const AGENT: Record<string, string> = { "claude-code": "Claude Code", codex: "Codex" };
const agentName = (a: string) => AGENT[a] ?? safe(a);

function disabled(label: string): HTMLButtonElement {
  return h("button", { class: "btn is-disabled", type: "button", disabled: true, "aria-disabled": "true", "data-write": "", title: NOT_YET, "aria-label": `${label}. ${NOT_YET}`, text: label });
}

function versionsTable(st: UpdatesStatus): HTMLElement {
  const rows: HTMLElement[] = [];
  const row = (part: string, value: Node | string, flag?: string) =>
    rows.push(h("tr", null, h("td", null, part, flag ? h("span", { class: "flagnote", text: flag }) : null), h("td", { class: "n" }, value)));
  const key = (v: string) => h("span", { class: "keyid", text: safe(v) || "-" });
  row("Client (this binary)", key(st.version), st.restartNeeded ? "restart at a clean point" : undefined);
  if (st.skills.length === 0) row("Skill", h("span", { class: "muted", text: st.skillNote ? `Not read: ${safe(st.skillNote)}` : "No agent found" }));
  for (const s of st.skills) {
    row(`Skill (${agentName(s.agent)})`, s.installed ? key(s.version) : h("span", { class: "muted", text: "Not installed" }), s.installed && !s.current ? "run clawbridge skill install" : undefined);
  }
  if (st.mcp.length === 0) row("MCP entries", h("span", { class: "muted", text: st.mcpNote ? `Not read: ${safe(st.mcpNote)}` : "None found to check" }));
  for (const m of st.mcp) {
    // An entry records no version, so the page shows how doctor found it.
    const ok = m.status === "pass";
    row(`MCP entry (${agentName(m.agent)})`, h("span", { class: `pill ${ok ? "pos" : m.status === "fail" ? "neg" : "warn"}`, text: ok ? "OK" : m.status === "fail" ? "Fails" : "Check" }), ok ? undefined : safe(m.message) || undefined);
  }
  return h("div", { class: "table-wrap" }, h("table", null,
    h("thead", null, h("tr", null, h("th", { text: "Part" }), h("th", { class: "n", text: "Version" }))),
    h("tbody", null, rows)));
}

/** The state (a)-(e) card, if any. */
function stateCard(m: UpdatesModel): HTMLElement | null {
  if (m.checkError) {
    return h("div", { class: "upd-card neg", role: "alert", "data-card": "failed" },
      h("h3", { text: "Not checked: the release could not be read. Nothing changed." }),
      h("p", { class: "upd-note", text: safe(m.checkError) }));
  }
  const c = m.check;
  if (!c) return null;
  if (c.available && c.manualRequired) {
    return h("div", { class: "upd-card warn", "data-card": "manual" },
      h("div", { class: "card-head" }, icon("warn"), h("h3", { text: `${safe(c.target)} needs a manual update` })),
      h("p", { text: "This release cannot update itself. Follow the steps from the release." }),
      c.manualText ? h("pre", { text: safe(c.manualText) }) : null,
      c.hold ? h("p", { class: "upd-note", text: "This release is also held: automatic updates skip it." }) : null);
  }
  if (c.available) {
    return h("div", { class: "upd-card", "data-card": "available" },
      h("h3", { text: `${safe(c.target)} is available` }),
      c.hold ? h("p", { class: "upd-note", text: "This release is held: automatic updates skip it." }) : null,
      h("p", null, "To install it, run this in your terminal:"),
      codebox("clawbridge self-update"),
      h("div", null, disabled("Update now"), h("span", { class: "upd-note gap", text: NOT_YET })));
  }
  const word = c.action === "dev_build" ? "This is a development build; name a release with clawbridge self-update --version."
    : c.action === "downgrade_refused" ? `The latest release ${safe(c.target)} is older than the running ${safe(c.current)}.`
      : "Everything is up to date.";
  return h("div", { class: "upd-card", "data-card": "current" }, h("h3", { text: word }));
}

/** The whole page, from what the two reads said. Exported for tests. */
export function updatesPage(m: UpdatesModel, onCheck: () => void): HTMLElement {
  const root = h("div", { "data-view": "updates" });
  if (m.platform === "wsl") root.append(h("p", { class: "upd-note", "data-note": "wsl", text: m.distro ? `Updates run inside ${safe(m.distro)} (WSL)` : "Updates run inside WSL" }));
  if (m.status.restartNeeded) {
    root.append(h("div", { class: "notice", role: "status", "data-note": "restart" }, icon("info"),
      h("span", { class: "grow" }, ...rich(`Updated to ${visible(m.status.onDisk)}. Restart \`clawbridge ui\` to use it.`))));
  }
  const card = stateCard(m);
  if (card) root.append(card);
  root.append(h("section", { class: "section" }, h("div", { class: "sec-head" }, h("h2", { text: "Versions" })), versionsTable(m.status)));
  const when = m.check?.checkedAt ? hhmm(m.check.checkedAt) : "";
  const check = h("button", { class: "btn", type: "button", "data-act": "check", disabled: m.checking, text: m.checking ? "Checking…" : "Check now" });
  check.addEventListener("click", onCheck);
  root.append(h("section", { class: "section" }, h("div", { class: "upd-row" },
    h("div", { class: "stack tight" }, h("b", { text: "Check for updates" }), h("span", { class: "upd-note", text: when ? `Last checked ${when}${m.check?.cached ? " (answer reused from the last minute)" : ""}` : "Not checked yet. This is the only time ClawBridge looks for a release." })),
    check)));
  const manual = m.check?.available === true && m.check.manualRequired;
  const sw = h("input", { class: "switch", type: "checkbox", role: "switch", disabled: true, "aria-label": "Update automatically in the background" });
  root.append(h("section", { class: "section" }, h("div", { class: "panel pad" }, h("div", { class: "switchrow" },
    h("div", { class: "t" }, h("b", { text: "Update automatically in the background" }),
      h("span", { text: manual ? "Automatic updates skip this release" : m.status.autoAvailable ? NOT_YET : AUTO_OFF })), sw))));
  root.append(h("section", { class: "section" }, disabled("Revert to previous version"), h("span", { class: "upd-note gap", text: NOT_YET })));
  return root;
}

/** Loads updates.status, then lets the person check. The check runs only on a click. */
export function updatesPanel(ctx: ViewCtx): HTMLElement {
  const host = h("div", null, h("div", { class: "panel pad", "aria-busy": "true" }, skBar(300)));
  let status: UpdatesStatus | null = null;
  let check: UpdateCheck | null = null;
  let checkError = "";
  let checking = false;
  const paint = () => {
    if (!status || !ctx.isCurrent()) return;
    host.replaceChildren(updatesPage({ status, check, checkError, checking, platform: ctx.hello?.platform ?? "", distro: ctx.hello?.distro ?? "" }, onCheck));
  };
  const onCheck = () => {
    if (checking) return;
    checking = true;
    checkError = "";
    paint();
    ctx.api.rpc("updates.check").then(
      (raw) => { check = parseUpdateCheck(raw); },
      (e: unknown) => {
        if (e instanceof ConnectionError) { ctx.connection(e.kind); return; }
        check = null;
        checkError = e instanceof CallError ? e.message : String(e);
      },
    ).finally(() => { checking = false; paint(); });
  };
  const load = () => {
    ctx.api.rpc("updates.status").then(
      (raw) => { status = parseUpdatesStatus(raw); paint(); },
      (e: unknown) => {
        if (!ctx.isCurrent()) return;
        if (e instanceof ConnectionError) return ctx.connection(e.kind);
        host.replaceChildren(errorBlock("the versions", "", e instanceof CallError ? e.message : String(e), load));
      },
    );
  };
  load();
  return host;
}
