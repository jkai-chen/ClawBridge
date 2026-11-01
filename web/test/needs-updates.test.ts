import { beforeEach, describe, expect, it } from "vitest";
import { parseNeeds, parseUpdateCheck, parseUpdatesStatus } from "../src/needs";
import { mountApp } from "../src/shell";
import { overviewView } from "../src/views/overview";
import { needsFinding, needsView } from "../src/views/needs";
import { sessionsView } from "../src/views/sessions";
import { updatesPage, type UpdatesModel } from "../src/views/updates";
import { apiFor, ctxFor, fakeServer, settle, fixtures } from "./helpers";
import { sessionsHostile } from "./fixtures";

beforeEach(() => {
  document.body.replaceChildren();
  location.hash = "";
});

const needs = parseNeeds(fixtures.needsList);

async function mountNeeds(answer: () => unknown, over: Record<string, unknown> = {}) {
  const root = document.createElement("div");
  document.body.append(root);
  const server = fakeServer({ "needs.list": answer });
  const ctx = { ...ctxFor(apiFor(server), root), ...over };
  needsView(ctx);
  await settle();
  return { root, ctx, server };
}

const clone = (v: unknown) => JSON.parse(JSON.stringify(v)) as Record<string, unknown> & { items: Record<string, unknown>[]; kinds: Record<string, unknown>[] };

describe("needs.list parsing", () => {
  it("reads the fixture: seven items, five kinds, every action disabled", () => {
    expect(needs.items).toHaveLength(7);
    expect(new Set(needs.items.map((n) => n.kind))).toEqual(new Set(["escalation", "claim", "intake", "person", "doctor"]));
    expect(needs.items.flatMap((n) => n.actions).every((a) => !a.enabled && a.reason === "Not in this build yet.")).toBe(true);
    expect(needs.items[0]?.facts).toContainEqual(["revision", "14"]);
  });
  it("never trusts an enabled flag from the wire, and drops an unknown kind", () => {
    const raw = clone(fixtures.needsList);
    (raw.items[0]?.actions as Record<string, unknown>[])[0]!.enabled = true;
    raw.items.push({ kind: "bogus", id: "x", title: "x" });
    const p = parseNeeds(raw);
    expect(p.items).toHaveLength(7);
    expect(p.items[0]?.actions[0]?.enabled).toBe(false);
  });
  it("survives junk", () => {
    expect(parseNeeds(null).items).toEqual([]);
    expect(parseNeeds({ items: "no" }).kinds).toEqual([]);
  });
});

describe("Needs you", () => {
  it("shows the finding, chips with counts, a list and a detail pane", async () => {
    const { root } = await mountNeeds(() => fixtures.needsList);
    expect(root.querySelector(".finding")?.textContent).toMatch(/^7 things need you; the oldest, an escalation on atlas, for /);
    const chips = Array.from(root.querySelectorAll(".fchips button")).map((b) => b.textContent);
    expect(chips).toEqual(["All7", "Escalations1", "Claims1", "Intake1", "People2", "Doctor2"]);
    expect(root.querySelectorAll(".nrow")).toHaveLength(7);
    const first = root.querySelector(".nrow");
    expect(first?.getAttribute("data-kind")).toBe("escalation");
    expect(first?.querySelectorAll("button")).toHaveLength(3);
    const d = root.querySelector("[data-detail]");
    expect(d?.textContent).toContain("Take ATL-212? It overlaps a claim.");
    expect(Array.from(d?.querySelectorAll("dt") ?? []).map((x) => x.textContent)).toEqual(expect.arrayContaining(["Session", "Issue", "Revision"]));
  });
  it("lists the actions but disables them all, each saying why", async () => {
    const { root } = await mountNeeds(() => fixtures.needsList);
    const buttons = Array.from(root.querySelectorAll<HTMLButtonElement>("button[data-write]"));
    expect(buttons.length).toBeGreaterThan(6);
    for (const b of buttons) {
      expect(b.disabled).toBe(true);
      expect(b.title).toBe("Not in this build yet.");
    }
  });
  it("draws a disabled action muted and dashed, never as the filled primary", async () => {
    const { root } = await mountNeeds(() => fixtures.needsList);
    const approve = root.querySelector<HTMLButtonElement>("[data-detail] [data-act=approve-claim]")!;
    expect(approve.classList.contains("is-disabled")).toBe(true);
    expect(approve.classList.contains("primary")).toBe(false);
    expect(approve.getAttribute("aria-disabled")).toBe("true");
    expect(approve.tabIndex).not.toBeGreaterThan(0); // never a tab stop that traps focus
    for (const b of root.querySelectorAll<HTMLButtonElement>("button[data-write]")) {
      expect(b.classList.contains("primary")).toBe(false);
      expect(b.getAttribute("aria-disabled")).toBe("true");
    }
  });
  it("filters by chip and selects a row into the detail", async () => {
    const { root } = await mountNeeds(() => fixtures.needsList);
    root.querySelector<HTMLButtonElement>("[data-chip=person]")!.click();
    expect(root.querySelectorAll(".nrow")).toHaveLength(2);
    root.querySelectorAll<HTMLElement>(".nrow")[1]!.click();
    expect(root.querySelector("[data-detail]")?.getAttribute("data-detail")).toBe("lin_priya");
  });
  it("sets the sidebar badge to the queue length", async () => {
    const { ctx } = await mountNeeds(() => fixtures.needsList);
    expect((ctx as unknown as { badges: Record<string, number> }).badges["needs-you"]).toBe(7);
  });
  it("shows the empty state", async () => {
    const { root } = await mountNeeds(() => ({ items: [], kinds: [{ kind: "claim", status: "ok", count: 0 }], generated_at: "x" }));
    expect(root.querySelector(".finding")?.textContent).toBe("Nothing needs you.");
    expect(root.querySelector(".empty b")?.textContent).toBe("Nothing needs you.");
  });
  it("shows an unavailable kind with its reason, and the rest still listed", async () => {
    const raw = clone(fixtures.needsList);
    raw.items = raw.items.filter((i) => i.kind !== "person");
    raw.kinds = raw.kinds.map((k) => (k.kind === "person" ? { kind: "person", status: "unavailable", reason: "refused: only the workspace owner may list users", count: 0 } : k));
    const { root } = await mountNeeds(() => raw);
    expect(root.querySelector("[data-block=unavailable]")?.textContent).toContain("People are unavailable: refused: only the workspace owner may list users");
    expect(root.querySelectorAll(".nrow")).toHaveLength(5);
    expect(root.querySelector("[data-chip=person]")?.textContent).toBe("People–");
  });
  it("says so when nothing could be read, instead of claiming nothing needs the person", async () => {
    const raw = { items: [], kinds: [{ kind: "claim", status: "unavailable", reason: "refused", count: 0 }], generated_at: "x" };
    const { root } = await mountNeeds(() => raw);
    expect(root.textContent).not.toContain("Nothing needs you.");
    expect(root.querySelector(".finding")?.textContent).toContain("could be read");
  });
  it("shows an error block with Retry when the call fails", async () => {
    const { root } = await mountNeeds(() => { throw new Error("boom"); });
    expect(root.querySelector(".errblock")?.textContent).toContain("Couldn't load Needs you");
  });
  it("reads again on a needs.changed nudge", async () => {
    let n = 0;
    let nudge = () => {};
    const { root } = await mountNeeds(() => { n++; return n === 1 ? fixtures.needsList : { items: [], kinds: [], generated_at: "x" }; }, { needsChanged: (fn: () => void) => { nudge = fn; return () => {}; } });
    expect(root.querySelectorAll(".nrow")).toHaveLength(7);
    nudge();
    await settle();
    expect(root.querySelectorAll(".nrow")).toHaveLength(0);
  });
});

describe("Viewing as: who may act", () => {
  it("shows a member the facts and a sentence in place of Approve and Release, and keeps Send to session", async () => {
    const raw = clone(fixtures.needsList);
    for (const it of raw.items) {
      if (it.kind === "claim" || it.kind === "escalation") it.who_may_act = { state: "no", text: "Only the project owner or a team lead can approve." };
    }
    const { root } = await mountNeeds(() => raw);
    const esc = root.querySelector("[data-detail]");
    expect(esc?.textContent).toContain("Only the project owner or a team lead can approve.");
    expect(esc?.querySelector("[data-act=approve-claim]")).toBeNull();
    expect(esc?.querySelector("[data-act=release]")).toBeNull();
    root.querySelector<HTMLButtonElement>("[data-chip=claim]")!.click();
    const claim = root.querySelector("[data-detail]");
    expect(Array.from(claim?.querySelectorAll("button") ?? []).map((b) => b.getAttribute("data-act"))).toEqual(["send-to-session"]);
    expect(claim?.textContent).toContain("HRB-88");
  });
  it("shows the owner the buttons and the owner sentence", async () => {
    const { root } = await mountNeeds(() => fixtures.needsList);
    expect(root.querySelector("[data-detail] .who-may")?.textContent).toBe("You can approve this as the project owner.");
    expect(root.querySelectorAll("[data-detail] button[data-act]")).toHaveLength(3);
  });
  it("shows a lead the 'depends' sentence", async () => {
    const raw = clone(fixtures.needsList);
    raw.items[0]!.who_may_act = { state: "depends", text: "You lead a team. You can approve it if the issue is in that team; the server checks." };
    const { root } = await mountNeeds(() => raw);
    expect(root.querySelector("[data-detail] .who-may")?.getAttribute("data-actor")).toBe("depends");
  });
});

describe("the Overview block", () => {
  async function overview(answer: () => unknown) {
    const root = document.createElement("div");
    document.body.append(root);
    const gone: string[] = [];
    const server = fakeServer({ "sessions.list": () => ({ source: "local-authority", data: { sessions: [] } }), "needs.list": answer });
    overviewView({ ...ctxFor(apiFor(server), root), go: (p) => { gone.push(p); } });
    await settle();
    return { root, gone };
  }
  it("shows the top three and 'See all'", async () => {
    const { root, gone } = await overview(() => fixtures.needsList);
    expect(root.querySelectorAll("[data-block=needs] .row")).toHaveLength(3);
    const see = Array.from(root.querySelectorAll<HTMLButtonElement>("[data-block=needs] button")).find((b) => b.textContent === "See all 7");
    see!.click();
    expect(gone).toEqual(["needs-you"]);
  });
  it("says it could not load, rather than claiming nothing needs the person", async () => {
    const { root } = await overview(() => { throw new Error("nope"); });
    const t = root.querySelector("[data-block=needs]")?.textContent ?? "";
    expect(t).toContain("couldn't be loaded");
    expect(t).not.toContain("Nothing needs you");
  });
  it("shows the empty line when the queue is empty", async () => {
    const { root } = await overview(() => ({ items: [], kinds: [], generated_at: "x" }));
    expect(root.querySelector("[data-block=needs]")?.textContent).toContain("Nothing needs you");
  });
});

describe("hostile text", () => {
  const hostile = "<img src=x onerror=alert(1)> ‮evil \u001b[31m";
  it("stays inert in the list, the detail, the facts and the unavailable reason", async () => {
    const raw = clone(fixtures.needsList);
    raw.items[0] = { ...raw.items[0]!, title: hostile, preview: hostile, project: hostile, facts: { session: hostile, issue: hostile } };
    raw.kinds[1] = { kind: "claim", status: "unavailable", reason: hostile, count: 0 };
    const { root } = await mountNeeds(() => raw);
    expect(root.querySelector("img")).toBeNull();
    expect(root.textContent).toContain("<img src=x onerror=alert(1)>");
    expect(root.textContent).not.toContain("‮");
    expect(root.textContent).not.toContain("\u001b");
    expect(root.textContent).toContain("‹U+202E›");
  });
  it("stays inert in finding text", () => {
    const raw = clone(fixtures.needsList);
    raw.items[0]!.project = "<script>x</script>";
    expect(needsFinding(parseNeeds(raw))).toContain("<script>x</script>");
  });
});

describe("the sessions drawer History", () => {
  async function drawerFor(name: string) {
    const root = document.createElement("div");
    document.body.append(root);
    location.hash = `#/sessions/${encodeURIComponent(name)}`;
    const server = fakeServer({ "sessions.list": () => ({ source: "local-authority", data: { sessions: [
      { id: "ses_1", name: "mine", owner_principal: "mara@example.test", host: "mara-mbp", agent_kind: "claude-code", status: "active", presence: "active", updated_at: new Date().toISOString() },
      { id: "ses_2", name: "kofis", owner_principal: "kofi@example.test", host: "kofi-pc", agent_kind: "codex", status: "active", presence: "active", updated_at: new Date().toISOString() },
    ] } }), "sessions.launchCommand": () => ({ line: "x", agent: "claude-code", session: "mine", host: "mara-mbp", setup: "" }) });
    sessionsView(ctxFor(apiFor(server), root));
    await settle();
    return { root, server };
  }
  it("says History arrives in a later build for your own session, and reads no inbox", async () => {
    const { root, server } = await drawerFor("mine");
    expect(root.querySelector("[data-block=history]")?.textContent).toBe("HistoryHistory arrives in a later build.");
    expect(server.calls.map((c) => c.method).filter((m) => /history|inbox/.test(m))).toEqual([]);
  });
  it("says what the other person's session will show", async () => {
    const { root } = await drawerFor("kofis");
    expect(root.querySelector("[data-block=history]")?.textContent).toContain("Messages you exchanged with this session arrive in a later build.");
    expect(root.querySelector("[data-block=launch]")).toBeNull();
  });
  it("keeps a hostile session name inert", async () => {
    const root = document.createElement("div");
    document.body.append(root);
    const hostileName = (sessionsHostile.data.sessions[0] as { name: string }).name;
    location.hash = `#/sessions/${encodeURIComponent(hostileName)}`;
    sessionsView(ctxFor(apiFor(fakeServer({ "sessions.list": () => sessionsHostile })), root));
    await settle();
    expect(root.querySelector("img")).toBeNull();
    expect(root.querySelector("[data-block=history]")).not.toBeNull();
  });
});

const baseStatus = parseUpdatesStatus(fixtures.updatesStatus);
const model = (over: Partial<UpdatesModel>): UpdatesModel => ({ status: { ...baseStatus, restartNeeded: false, onDisk: baseStatus.version }, check: null, checkError: "", checking: false, platform: "linux", distro: "", ...over });
const check = (over: Record<string, unknown>) => parseUpdateCheck({ ...(fixtures.updatesCheck as Record<string, unknown>), ...over });
const page = (m: UpdatesModel) => updatesPage(m, () => {});

describe("Settings → Updates", () => {
  it("parses the fixtures", () => {
    expect(baseStatus).toMatchObject({ version: "v1.0.0", onDisk: "v1.0.1", restartNeeded: true, autoAvailable: false });
    expect(baseStatus.skills.map((s) => s.version)).toEqual(["v1.0.0", "v0.9.4"]);
    expect(parseUpdateCheck(fixtures.updatesCheck)).toMatchObject({ target: "v1.1.0", available: true, manualRequired: true, hold: false });
  });
  it("lists versions with no GUI app row, and the MCP entries by how doctor found them", () => {
    const el = page(model({}));
    const parts = Array.from(el.querySelectorAll("tbody td:first-child")).map((t) => t.textContent ?? "");
    expect(parts.some((p) => /GUI/i.test(p))).toBe(false);
    expect(parts.map((p) => p.replace(/(run clawbridge|points at).*$/, ""))).toEqual(["Client (this binary)", "Skill (Claude Code)", "Skill (Codex)", "MCP entry (Codex)"]);
  });
  it("(before a check) says it has not checked, and shows no card", () => {
    const el = page(model({}));
    expect(el.textContent).toContain("Not checked yet");
    expect(el.querySelector(".upd-card")).toBeNull();
  });
  it("says when a reused answer was checked", () => {
    const el = page(model({ check: check({ cached: true }) }));
    expect(el.textContent).toContain("answer reused from the last minute");
    expect(page(model({ check: check({ cached: false }) })).textContent).not.toContain("reused");
  });
  it("(a) up to date", () => {
    const el = page(model({ check: check({ update_available: false, manual_required: false, action: "up_to_date", target: "v1.0.0" }) }));
    expect(el.querySelector("[data-card=current]")?.textContent).toBe("Everything is up to date.");
    expect(el.textContent).toContain("Last checked");
  });
  it("(b) update available, with Update now disabled and the command to run", () => {
    const el = page(model({ check: check({ manual_required: false, manual_text: "" }) }));
    expect(el.querySelector("[data-card=available]h3, [data-card=available] h3")?.textContent).toBe("v1.1.0 is available");
    const b = el.querySelector<HTMLButtonElement>("[data-card=available] button[data-write]");
    expect(b?.textContent).toBe("Update now");
    expect(b?.disabled).toBe(true);
    expect(b?.classList.contains("primary")).toBe(false);
    expect(b?.getAttribute("aria-disabled")).toBe("true");
    expect(el.querySelector("[data-card=available] code")?.textContent).toBe("clawbridge self-update");
  });
  it("(b) says a held release is skipped by automatic updates", () => {
    const el = page(model({ check: check({ manual_required: false, hold: true }) }));
    expect(el.querySelector("[data-card=available]")?.textContent).toContain("held");
  });
  it("(c) manual update required: warning card, steps in mono, no Update now, toggle note", () => {
    const el = page(model({ check: check({}) }));
    const card = el.querySelector("[data-card=manual]");
    expect(card?.querySelector("h3")?.textContent).toBe("v1.1.0 needs a manual update");
    expect(card?.querySelector("pre")?.textContent).toContain("sign in again after installing");
    expect(Array.from(el.querySelectorAll("button")).some((b) => b.textContent === "Update now")).toBe(false);
    expect(el.textContent).toContain("Automatic updates skip this release");
  });
  it("(d) a failed check says nothing changed", () => {
    const el = page(model({ checkError: "latest release discovery failed" }));
    const c = el.querySelector("[data-card=failed]");
    expect(c?.getAttribute("role")).toBe("alert");
    expect(c?.textContent).toContain("Nothing changed.");
    expect(c?.textContent).toContain("latest release discovery failed");
  });
  it("(e) WSL names the distro", () => {
    const el = page(model({ platform: "wsl", distro: "Ubuntu" }));
    expect(el.querySelector("[data-note=wsl]")?.textContent).toBe("Updates run inside Ubuntu (WSL)");
    expect(page(model({})).querySelector("[data-note=wsl]")).toBeNull();
    expect(page(model({ platform: "wsl", distro: "" })).querySelector("[data-note=wsl]")?.textContent).toBe("Updates run inside WSL");
  });
  it("tells the person to restart clawbridge ui after an update, when the file on disk is newer", () => {
    const el = page(model({ status: baseStatus }));
    expect(el.querySelector("[data-note=restart]")?.textContent).toBe("Updated to v1.0.1. Restart clawbridge ui to use it.");
    expect(el.querySelector("[data-note=restart] code")?.textContent).toBe("clawbridge ui");
    expect(page(model({})).querySelector("[data-note=restart]")).toBeNull();
  });
  it("renders the toggle, Update now and Revert disabled, with the reason, and automatic updates unavailable", () => {
    const el = page(model({}));
    const sw = el.querySelector<HTMLInputElement>("input[role=switch]");
    expect(sw?.disabled).toBe(true);
    expect(el.textContent).toContain("Automatic updates aren't available in this build");
    const revert = Array.from(el.querySelectorAll<HTMLButtonElement>("button")).find((b) => b.textContent === "Revert to previous version");
    expect(revert?.disabled).toBe(true);
    expect(revert?.title).toBe("Not in this build yet.");
  });
  it("keeps hostile release notes and failure text inert", () => {
    const evil = "<img src=x onerror=alert(1)>‮";
    const manual = page(model({ check: check({ manual_text: evil, target: "<b>v9</b>" }) }));
    expect(manual.querySelector("img")).toBeNull();
    expect(manual.querySelector("[data-card] b")).toBeNull();
    expect(manual.querySelector("pre")?.textContent).toContain("<img src=x onerror=alert(1)>");
    expect(manual.textContent).not.toContain("‮");
    const failed = page(model({ checkError: evil }));
    expect(failed.querySelector("img")).toBeNull();
  });
  it("calls the check only when Check now is clicked", async () => {
    location.hash = "#/settings/updates";
    const host = document.createElement("div");
    document.body.append(host);
    const server = fakeServer({ "needs.list": () => ({ items: [], kinds: [] }), "updates.status": () => fixtures.updatesStatus, "updates.check": () => fixtures.updatesCheck });
    mountApp(host, apiFor(server));
    await settle();
    expect(server.calls.map((c) => c.method)).not.toContain("updates.check");
    expect(host.textContent).toContain("Updated to v1.0.1. Restart clawbridge ui to use it.");
    host.querySelector<HTMLButtonElement>("button[data-act=check]")!.click();
    await settle();
    expect(server.calls.filter((c) => c.method === "updates.check")).toHaveLength(1);
    expect(host.querySelector("[data-card=manual]")).not.toBeNull();
  });
});
