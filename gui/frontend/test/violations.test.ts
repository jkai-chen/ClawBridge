import { describe, expect, it } from "vitest";
import { watchViolations } from "../src/violations";

function violation(directive: string, blocked: string): Event {
  const e = new Event("securitypolicyviolation");
  Object.assign(e, { violatedDirective: directive, blockedURI: blocked, sourceFile: "", lineNumber: 0 });
  return e;
}

describe("watchViolations", () => {
  it("buffers until the shell is connected, then reports in order", () => {
    const doc = document.implementation.createHTMLDocument("t");
    const win = new EventTarget() as unknown as Window;
    const sink = watchViolations(doc, win);
    doc.dispatchEvent(violation("script-src-elem", "inline"));
    const seen: string[] = [];
    sink.connect((t) => seen.push(t));
    doc.dispatchEvent(violation("require-trusted-types-for", "inline"));
    expect(seen).toEqual(["script-src-elem blocked inline", "require-trusted-types-for blocked inline"]);
  });

  it("reports Trusted Types errors and ignores unrelated ones", () => {
    const doc = document.implementation.createHTMLDocument("t");
    const win = new EventTarget() as unknown as Window;
    const seen: string[] = [];
    watchViolations(doc, win).connect((t) => seen.push(t));
    win.dispatchEvent(Object.assign(new Event("error"), { message: "This document requires 'TrustedHTML' assignment." }));
    win.dispatchEvent(Object.assign(new Event("error"), { message: "ResizeObserver loop" }));
    expect(seen).toEqual(["trusted-types error: This document requires 'TrustedHTML' assignment."]);
  });
});
