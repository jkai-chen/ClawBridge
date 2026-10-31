import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

// Belt and braces beside the ESLint rule (eslint.config.js): the source must
// never name a sink that parses markup. Text reaches the DOM only through
// textContent and text nodes (GUI-DESIGN §7).
const SRC = join(__dirname, "..", "src");
const SINKS = /\b(innerHTML|outerHTML|insertAdjacentHTML|createContextualFragment|document\s*\.\s*write(ln)?|DOMParser|eval|new\s+Function)\b/;

function files(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f);
    return statSync(p).isDirectory() ? files(p) : /\.(ts|js|html)$/.test(f) ? [p] : [];
  });
}

describe("no markup-parsing sinks in src", () => {
  const all = files(SRC);
  it("finds the source files", () => expect(all.length).toBeGreaterThan(8));
  for (const f of all) {
    it(f.slice(SRC.length + 1), () => {
      const lines = readFileSync(f, "utf8").split("\n");
      const hits = lines
        .map((text, i) => ({ text, n: i + 1 }))
        // dom.ts names the banned attribute names in order to refuse them.
        .filter(({ text }) => !(f.endsWith("dom.ts") && /REFUSED_ATTRS|checkAttr/.test(text)))
        .filter(({ text }) => SINKS.test(text) && !/^\s*(\/\/|\*|\/\*)/.test(text));
      expect(hits).toEqual([]);
    });
  }
});

describe("index.html", () => {
  const html = readFileSync(join(__dirname, "..", "index.html"), "utf8");
  it("has a CSP with default-src 'self' and Trusted Types", () => {
    expect(html).toMatch(/http-equiv="Content-Security-Policy"/);
    expect(html).toContain("default-src 'self'");
    expect(html).toContain("require-trusted-types-for 'script'");
    expect(html).not.toMatch(/unsafe-(inline|eval)/);
  });
  it("loads no remote asset and has no inline script, style or handler", () => {
    const body = html.replace(/<!--[\s\S]*?-->/g, "");
    expect(body).not.toMatch(/(src|href)="https?:/);
    expect(body).not.toMatch(/<script(?![^>]*\bsrc=)/);
    expect(body).not.toMatch(/<style/);
    expect(body).not.toMatch(/\son[a-z]+=/);
  });
});
