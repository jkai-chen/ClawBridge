// Dispatcher (GUI-D-SPEC §5.6): the mode, the model setup as the server
// reports it, and real budget numbers. Nothing here is a made-up figure:
// what the server does not say is shown as "Not available yet".
import { h } from "../dom";
import { parseDispatcher, type DispatcherStatus } from "../pages";
import { safe, titleRow, type View, type ViewCtx } from "./common";
import { ago, call, dash, disabledWrite, loadInto, meter, mono, note, pill } from "./pagekit";

export function modeWords(s: DispatcherStatus): string {
  if (!s.enabled) return "Off: the dispatcher is not enabled for this project.";
  return s.mode === "model" ? "Model mode: a model decides, within the rules." : s.mode === "rules" ? "Rules mode: the rules decide." : `Mode ${safe(s.mode) || "unknown"}.`;
}

/** The last setup test, from the owner's own holder row; nothing is invented. */
export function lastTest(s: DispatcherStatus): string {
  const me = s.holders.find((x) => x.you);
  if (!me) return s.hasDoctor ? "No model key is set up by you." : "Only a project owner sees the model setup test.";
  if (me.testFailure) return `The setup test of revision ${me.testRevision} failed: ${safe(me.testFailure)}`;
  if (me.key === "active") return `The key of revision ${me.configRevision} is active; its setup test passed when it was set.${me.agentStatus ? ` Agent ${safe(me.agentStatus)}.` : ""}`;
  return `Setup test pending for revision ${me.pendingRevision}.`;
}

export const dispatcherView: View = (ctx: ViewCtx) => {
  const host = h("div", null);
  ctx.root.replaceChildren(titleRow("Dispatcher", "Loading the dispatcher."), host);
  loadInto(ctx, host, "the dispatcher", call(ctx, "dispatcher.status", {}, parseDispatcher), (s, reload) => {
    ctx.root.firstElementChild?.replaceWith(titleRow("Dispatcher", modeWords(s), h("button", { class: "btn sm", type: "button", on: { click: reload }, text: "Refresh" })));
    const m = s.model;
    return h("div", { class: "stack" },
      s.denied ? h("div", { class: "notice plain" }, h("span", { class: "grow", text: s.denied })) : null,
      h("div", { class: "panel pad" }, h("h2", { text: "Mode" }), h("dl", { class: "kv" },
        h("dt", { text: "Routing" }), h("dd", null, pill(s.enabled ? (s.mode === "model" ? "Model" : "Rules") : "Off", s.enabled ? "pos" : "line")),
        h("dt", { text: "Policy revision" }), h("dd", { text: String(s.revision) }),
        h("dt", { text: "Updated" }), h("dd", { text: ago(s.updatedAt) }),
        h("dt", { text: "Model use" }), h("dd", { text: m ? dash(m.use) : "Not reported" }),
        h("dt", { text: "Provider and model" }), h("dd", { text: m && (m.provider || m.model) ? `${dash(m.provider)} / ${dash(m.model)}` : "-" }),
        h("dt", { text: "Key" }), h("dd", { text: m ? `${dash(m.key)}${m.key === "active" ? " (stored on the server, never shown)" : ""}` : "-" }),
        h("dt", { text: "Agent" }), h("dd", { text: m ? dash(m.agent) : "-" }))),
      h("div", { class: "panel pad" }, h("h2", { text: "Last test" }), h("p", { text: lastTest(s) }),
        s.hasDoctor ? note(s.sealConfigured ? "The server can open stored model keys." : "The server has no model key seal key, so no stored key opens and the rules decide.") : null,
        s.holders.length > 1 ? h("p", { class: "row-note", text: `${s.holders.length} owners have a model key.` }) : null),
      h("div", { class: "panel pad" }, h("h2", { text: "Today's cases" }), h("p", { class: "muted", text: "Not available yet. The server has no read for routed and escalated cases." })),
      h("div", { class: "panel pad" }, h("h2", { text: "Budget" }),
        s.budget ? [meter("Linear calls this hour (all writers)", s.budget.used, s.budget.limit), s.budget.resetsAt ? note(`Resets ${ago(s.budget.resetsAt)}.`) : null, note("Shared by every Linear call in this workspace; the dispatcher's issue reads count against it.")]
          : h("p", { class: "muted", text: "No budget numbers from the server (hosted workspaces report the Linear hour)." })),
      h("div", { class: "toolbar" }, disabledWrite("Set up model"), disabledWrite("Rotate key")),
      s.holders.length ? h("div", { class: "panel pad" }, h("h2", { text: "Owners with a model key" }), h("div", { class: "chips" }, s.holders.map((x) => mono(`${x.you ? "you" : x.principal}: ${x.key || "none"}`)))) : null,
    );
  });
};
