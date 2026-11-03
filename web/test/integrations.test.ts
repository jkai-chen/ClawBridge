import { readFileSync } from "node:fs";
import { join } from "node:path";
import { beforeEach, describe, expect, it } from "vitest";
import { CallError } from "../src/api";
import { routeArg, routeArg2, routeSegment } from "../src/router";
import { integrationsView } from "../src/views/integrations";
import { apiFor, ctxFor, fakeServer, settle } from "./helpers";

const DIR = join(__dirname, "..", "..", "internal", "guibackend", "testdata", "rpc");
const fx = (name: string): unknown => (JSON.parse(readFileSync(join(DIR, name), "utf8")) as { response: { result: unknown } }).response.result;
const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v)) as T;
const HOSTILE = "<img src=x onerror=alert(1)>";

const base = (): Record<string, () => unknown> => ({
  "linear.connection": () => clone(fx("read-linear-connection.json")),
  "linear.binding": () => clone(fx("read-linear-binding.json")),
  "linear.tasks": () => clone(fx("read-linear-tasks.json")),
});

async function mount(hash: string, over: Record<string, () => unknown> = {}) {
  document.body.replaceChildren();
  location.hash = hash;
  const root = document.createElement("div");
  document.body.append(root);
  const ctx = ctxFor(apiFor(fakeServer({ ...base(), ...over })), root);
  integrationsView(ctx);
  await settle();
  return root;
}

beforeEach(() => { document.body.replaceChildren(); location.hash = ""; });

describe("router segments", () => {
  it("reads the second and third segments, decoded", () => {
    expect(routeArg("#/integrations/linear/tasks")).toBe("linear");
    expect(routeArg2("#/integrations/linear/tasks")).toBe("tasks");
    expect(routeArg2("#/integrations/linear/a%20b?x=1")).toBe("a b");
    expect(routeArg2("#/integrations/linear")).toBe("");
    expect(routeSegment("#/integrations", 2)).toBe("");
  });
  it("treats a malformed escape as absent", () => {
    expect(routeArg2("#/integrations/linear/%E0")).toBe("");
    expect(routeArg("#/integrations/%E0")).toBe("");
  });
});

describe("Project integrations landing", () => {
  it("shows one provider card, headed Linear, and nothing else names a provider", async () => {
    const root = await mount("#/integrations");
    expect(root.querySelector("h1")?.textContent).toBe("Project integrations");
    const cards = root.querySelectorAll("[data-provider]");
    expect(cards.length).toBe(1);
    expect(cards[0]?.querySelector("h2")?.textContent).toBe("Linear");
    const outside = root.cloneNode(true) as HTMLElement;
    outside.querySelector("[data-provider]")?.remove();
    expect(outside.textContent).not.toContain("Linear");
    expect(root.textContent).not.toContain("GitHub");
    expect(root.textContent).not.toMatch(/coming soon/i);
  });
  it("says Connected with the workspace and the bound teams", async () => {
    const root = await mount("#/integrations");
    const card = root.querySelector("[data-provider]")!;
    expect(card.querySelector(".pill")?.textContent).toBe("Connected");
    expect(card.textContent).toContain("mara (Northwind)");
    expect(card.textContent).toContain("Atlas");
    expect(card.textContent).toContain("Harbor");
    expect(card.querySelector("a")?.getAttribute("href")).toBe("#/integrations/linear");
    expect(card.querySelector("a")?.textContent).toBe("Open Linear");
  });
  it("says No board linked when the binding is empty", async () => {
    const root = await mount("#/integrations", { "linear.binding": () => ({ binding: { configured: false } }) });
    expect(root.querySelector("[data-provider]")?.textContent).toContain("No board linked");
  });
  it("says the board couldn't be read when the binding call fails", async () => {
    const root = await mount("#/integrations", { "linear.binding": () => { throw new CallError("binding unavailable"); } });
    const card = root.querySelector("[data-provider]")!;
    expect(card.querySelector(".pill")?.textContent).toBe("Connected");
    expect(card.textContent).toContain("The board couldn't be read");
    expect(card.textContent).toContain("binding unavailable");
    expect(card.textContent).not.toContain("No board linked");
  });
  it("says Not connected and shows the connect hint", async () => {
    const root = await mount("#/integrations", { "linear.connection": () => ({ profiles: [] }) });
    const card = root.querySelector("[data-provider]")!;
    expect(card.querySelector(".pill")?.textContent).toBe("Not connected");
    expect(card.textContent).toContain("clawbridge linear connect --name NAME --access read");
  });
  it("says Couldn't be read on an error", async () => {
    const root = await mount("#/integrations", { "linear.connection": () => { throw new CallError("Linear refused the read"); } });
    const card = root.querySelector("[data-provider]")!;
    expect(card.querySelector(".pill")?.textContent).toBe("Couldn't be read");
    expect(card.textContent).toContain("Linear refused the read");
  });
  it("renders a hostile connection name as text", async () => {
    const c = clone(fx("read-linear-connection.json")) as { profiles: Record<string, unknown>[] };
    c.profiles[0]!.name = HOSTILE;
    const root = await mount("#/integrations", { "linear.connection": () => c });
    expect(root.querySelector("img, script")).toBeNull();
    expect(root.querySelector("[data-provider]")?.textContent).toContain(HOSTILE);
  });
  it("shows the landing for an unknown provider", async () => {
    const root = await mount("#/integrations/nope");
    expect(root.querySelector("h1")?.textContent).toBe("Project integrations");
    expect(root.querySelectorAll("[data-provider]").length).toBe(1);
  });
});

describe("Linear section", () => {
  it("has a breadcrumb back and defaults to the binding tab", async () => {
    const root = await mount("#/integrations/linear");
    expect(root.querySelector(".crumb a")?.getAttribute("href")).toBe("#/integrations");
    expect(root.querySelector(".crumb a")?.textContent).toBe("Project integrations");
    expect(root.querySelector("h1")?.textContent).toBe("Linear");
    expect(root.querySelector("[role=tab][aria-selected=true]")?.textContent).toBe("Binding");
    expect(root.querySelector("[role=tab]")?.getAttribute("href")).toBe("#/integrations/linear/connection");
  });
  it("opens the tab named in the third segment", async () => {
    const root = await mount("#/integrations/linear/tasks");
    expect(root.querySelector("[role=tab][aria-selected=true]")?.textContent).toBe("Tasks");
  });
});
