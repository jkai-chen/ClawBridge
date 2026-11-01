import { describe, expect, it } from "vitest";
import { applyTheme } from "../src/theme";

describe("theme", () => {
  it("follows the OS setting and its changes", () => {
    let dark = true;
    let listener: () => void = () => {};
    const win = { matchMedia: () => ({ get matches() { return dark; }, addEventListener: (_: string, l: () => void) => { listener = l; }, removeEventListener: () => {} }) } as unknown as Window;
    applyTheme(document, win);
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    dark = false;
    listener();
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });
});
