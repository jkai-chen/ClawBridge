import type { DoctorCheck, DoctorReport } from "../api";
import { CallError } from "../api";
import { ConnectionError } from "../transport";
import { h } from "../dom";
import { icon } from "../icons";
import { hhmm, plural } from "../format";
import { codebox, emptyBlock, errorBlock, safe, skBar, stamp, titleRow, type View, type ViewCtx } from "./common";

function failBlock(c: DoctorCheck): HTMLElement {
  return h("div", { class: "doc-fail" },
    h("div", { class: "hd" }, icon("warn"), h("h3", { text: safe(c.title) })),
    c.message ? h("p", { text: safe(c.message) }) : null,
    c.path ? h("div", null, h("span", { class: "keyid m", text: safe(c.path) })) : null,
    c.action ? codebox(safe(c.action)) : null,
  );
}

function noteRow(c: DoctorCheck): HTMLElement {
  return h("div", { class: "doc-row" },
    icon(c.level === "warn" ? "warn" : "info"),
    h("div", { class: "grow" },
      h("div", { class: "dtitle", text: safe(c.title) }),
      c.message ? h("p", { text: safe(c.message) }) : null,
      c.path ? h("span", { class: "keyid m", text: safe(c.path) }) : null,
      c.action ? h("div", { class: "fix-line" }, codebox(safe(c.action))) : null,
    ),
    h("span", { class: `pill ${c.level === "warn" ? "warn" : ""}`.trim(), text: c.level === "warn" ? "Warning" : "Note" }),
  );
}

export function doctorBody(report: DoctorReport): HTMLElement {
  const fails = report.checks.filter((c) => c.level === "fail");
  const notes = report.checks.filter((c) => c.level === "warn" || c.level === "info");
  const passes = report.checks.filter((c) => c.level === "pass");
  const root = h("div", null);
  if (report.checks.length === 0) {
    root.append(emptyBlock("No checks reported.", "Re-run to check this machine again."));
    return root;
  }
  if (fails.length) root.append(h("div", { class: "section", style: "display:grid;gap:12px" }, fails.map(failBlock)));
  if (notes.length) root.append(h("div", { class: "section panel" }, notes.map(noteRow)));
  if (passes.length) {
    const list = h("ul", { class: "passlist", hidden: true }, passes.map((c) => h("li", null, icon("check"), h("span", { text: safe(c.title) }))));
    const toggle = h("button", { class: "btn ghost folded", type: "button", "aria-expanded": "false", text: `${passes.length} passed` });
    toggle.addEventListener("click", () => {
      const open = list.hidden;
      list.hidden = !open;
      toggle.setAttribute("aria-expanded", String(open));
      toggle.textContent = open ? `Hide ${passes.length} passed` : `${passes.length} passed`;
    });
    root.append(h("div", { class: "section panel" }, toggle, list));
  }
  return root;
}

export function doctorFinding(report: DoctorReport): string {
  const f = report.checks.filter((c) => c.level === "fail").length;
  const w = report.checks.filter((c) => c.level === "warn").length;
  const p = report.checks.filter((c) => c.level === "pass").length;
  if (report.checks.length === 0) return "No checks reported.";
  if (f === 0 && w === 0) return `Nothing is broken on this machine. ${p} passed.`;
  const parts = [f ? `${f} failing` : "", w ? plural(w, "warning") : "", `${p} passed`].filter(Boolean);
  return `${parts.join(", ")}.`;
}

export const doctorView: View = (ctx: ViewCtx) => {
  const { root, api } = ctx;
  let lastGood = "";

  const load = () => {
    root.replaceChildren(
      titleRow("Doctor", "Checking this machine."),
      h("div", { class: "panel pad", "aria-busy": "true" }, [220, 300, 180].map((w) => h("div", { style: "margin:10px 0" }, skBar(w)))),
    );
    api.doctor().then(
      (report) => {
        if (!ctx.isCurrent()) return;
        lastGood = hhmm(report.generatedAt) || hhmm(new Date().toISOString());
        ctx.setBadge("doctor", report.checks.filter((c) => c.level === "fail").length);
        root.replaceChildren(
          titleRow("Doctor", doctorFinding(report), [stamp(`Checked ${lastGood}`), h("button", { class: "btn sm", type: "button", on: { click: load }, text: "Re-run" })]),
          doctorBody(report),
        );
      },
      (e: unknown) => {
        if (!ctx.isCurrent()) return;
        if (e instanceof ConnectionError) return ctx.connection(e.kind);
        root.replaceChildren(titleRow("Doctor"), errorBlock("the doctor checks", lastGood, e instanceof CallError ? e.message : String(e), load));
      },
    );
  };
  load();
};
