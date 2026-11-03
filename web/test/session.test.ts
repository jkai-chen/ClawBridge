import { beforeEach, describe, expect, it } from "vitest";
import { STORAGE_KEY, forgetSecret, takeSecret } from "../src/session";

const SECRET = "ab".repeat(32);

function env(metaContent?: string) {
  const doc = document.implementation.createHTMLDocument("t");
  if (metaContent !== undefined) {
    const m = doc.createElement("meta");
    m.setAttribute("name", "cb-session");
    m.setAttribute("content", metaContent);
    doc.head.append(m);
  }
  const data = new Map<string, string>();
  const store = { getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => { data.set(k, v); }, removeItem: (k: string) => { data.delete(k); } };
  const replaced: unknown[][] = [];
  const history = { replaceState: (...a: unknown[]) => { replaced.push(a); } };
  return { doc, store, history, data, replaced };
}

beforeEach(() => sessionStorage.clear());

describe("takeSecret", () => {
  it("moves the meta's secret into storage, removes the meta and resets the address to /", () => {
    const e = env(SECRET);
    expect(takeSecret(e)).toBe(SECRET);
    expect(e.data.get(STORAGE_KEY)).toBe(SECRET);
    expect(e.doc.querySelector('meta[name="cb-session"]')).toBeNull();
    expect(e.replaced).toEqual([[null, "", "/"]]);
  });
  it("reads storage on a reload, when there is no meta, and leaves the address alone", () => {
    const e = env();
    e.data.set(STORAGE_KEY, SECRET);
    expect(takeSecret(e)).toBe(SECRET);
    expect(e.replaced).toEqual([]);
  });
  it("has no secret in a new tab", () => {
    expect(takeSecret(env())).toBeNull();
  });
  it("ignores an empty meta", () => {
    const e = env("");
    expect(takeSecret(e)).toBeNull();
    expect(e.doc.querySelector("meta[name=cb-session]")).toBeNull();
  });
  it("forgets the secret", () => {
    sessionStorage.setItem(STORAGE_KEY, SECRET);
    forgetSecret();
    expect(sessionStorage.getItem(STORAGE_KEY)).toBeNull();
  });
});
