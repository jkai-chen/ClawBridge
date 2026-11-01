import { describe, expect, it } from "vitest";
import { h, rich, setChildren, visible } from "../src/dom";

const HOSTILE = "<img src=x onerror=alert(1)>";
const BIDI = "invoice\u202Efdp.exe";
const ANSI = "\u001b[31mred\u001b[0m";

describe("h() never parses HTML", () => {
  it("renders a hostile string as inert text in every position", () => {
    const el = h("div", { text: HOSTILE }, h("span", null, HOSTILE), HOSTILE);
    expect(el.querySelector("img")).toBeNull();
    expect(el.querySelectorAll("*").length).toBe(1); // only the <span>
    expect(el.textContent).toBe(HOSTILE + HOSTILE + HOSTILE); // text prop, span, child string
  });

  it("puts text props and child strings in as text nodes", () => {
    const a = h("p", { text: HOSTILE });
    expect(a.children.length).toBe(0);
    expect(a.textContent).toBe(HOSTILE);
    const b = h("p", null, HOSTILE, 42, ["<b>x</b>"]);
    expect(b.children.length).toBe(0);
    expect(b.textContent).toBe(`${HOSTILE}42<b>x</b>`);
    expect(b.childNodes[0]?.nodeType).toBe(Node.TEXT_NODE);
  });

  it("has no hostile attribute path: event handlers, srcdoc and outside URLs are refused", () => {
    expect(() => h("img", { onerror: "alert(1)" })).toThrow();
    expect(() => h("img", { OnLoad: "alert(1)" })).toThrow();
    expect(() => h("iframe", { srcdoc: HOSTILE })).toThrow();
    expect(() => h("div", { innerHTML: HOSTILE })).toThrow();
    expect(() => h("div", { outerHTML: HOSTILE })).toThrow();
    expect(() => h("a", { href: "javascript:alert(1)" })).toThrow();
    expect(() => h("a", { href: "https://evil.example" })).toThrow();
    expect(() => h("a", { href: "//evil.example" })).toThrow();
    expect(() => h("a", { href: "/\\evil.example" })).toThrow();
    expect(h("a", { href: "/assets/x" }).getAttribute("href")).toBe("/assets/x");
    expect(h("a", { href: "#/sessions" }).getAttribute("href")).toBe("#/sessions");
  });

  it("applies style through the CSSOM, not as an attribute", () => {
    const el = h("div", { style: "display:grid" });
    expect(el.style.display).toBe("grid");
  });

  it("keeps backticks-spans as code elements and the rest as text", () => {
    const wrap = h("p", null, rich(`run \`${HOSTILE}\` now <b>`));
    expect(wrap.querySelectorAll("code").length).toBe(1);
    expect(wrap.querySelector("code")?.textContent).toBe(HOSTILE);
    expect(wrap.querySelector("img")).toBeNull();
    expect(wrap.querySelector("b")).toBeNull();
  });

  it("setChildren replaces and stays inert", () => {
    const el = h("div", null, "old");
    setChildren(el, HOSTILE);
    expect(el.textContent).toBe(HOSTILE);
    expect(el.children.length).toBe(0);
  });
});

describe("visible()", () => {
  it("shows bidi overrides and escape bytes as marks, not as characters", () => {
    const out = visible(BIDI + ANSI);
    expect(out).not.toContain("\u202E");
    expect(out).not.toContain("\u001b");
    expect(out).toContain("‹U+202E›");
    expect(out).toContain("‹U+001B›");
    expect(out).toContain("[31mred");
  });
  it("covers isolates, zero-width, C1, DEL and BOM, and keeps newlines and tabs", () => {
    const out = visible("a\u2066b\u200Bc\u0085d\u007Fe\uFEFFf\ng\th");
    for (const c of ["\u2066", "\u200B", "\u0085", "\u007F", "\uFEFF"]) expect(out).not.toContain(c);
    expect(out).toContain("\n");
    expect(out).toContain("\t");
  });
  it("leaves markup characters alone (they are rendered as text, not stripped)", () => {
    expect(visible(HOSTILE)).toBe(HOSTILE);
  });
  it("marks Unicode tag characters and annotation marks", () => {
    const tag = String.fromCodePoint(0xe0041, 0xe007f);
    const out = visible("a" + tag + "b\uFFF9c");
    expect(out).not.toContain(String.fromCodePoint(0xe0041));
    expect(out).toContain("‹U+E0041›");
    expect(out).toContain("‹U+E007F›");
    expect(out).toContain("‹U+FFF9›");
  });
  it("copes with non-strings", () => {
    expect(visible(null)).toBe("");
    expect(visible(undefined)).toBe("");
    expect(visible(7)).toBe("7");
  });
});

describe("a hostile string rendered through h() and visible() together", () => {
  it("is inert text with no element, no handler and no bidi or ESC character", () => {
    const el = h("td", { text: visible(HOSTILE + BIDI + ANSI) });
    expect(el.children.length).toBe(0);
    expect(el.querySelector("img")).toBeNull();
    expect(el.textContent).toContain(HOSTILE);
    expect(["\u202E", "\u001b"].some((c) => (el.textContent ?? "").includes(c))).toBe(false);
    expect(el.outerHTML).toContain("&lt;img src=x onerror=alert(1)&gt;"); // serialised, it is escaped text
  });
});
