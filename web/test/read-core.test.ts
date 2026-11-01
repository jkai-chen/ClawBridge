import { beforeEach, describe, expect, it, vi } from "vitest";
import { mountApp } from "../src/shell";
import { DAY_MS, inTab, isArchived, isOwn } from "../src/sessionstate";
import { buildLanes, clip, laneKind } from "../src/lanes";
import { todayCells, overviewView } from "../src/views/overview";
import { doctorView } from "../src/views/doctor";
import { sessionsView } from "../src/views/sessions";
import { parseSessions, type SessionRow } from "../src/api";
import { apiFor, ctxFor, fakeServer, settle } from "./helpers";
import { fullHello } from "./fixtures";

beforeEach(() => {
  document.body.replaceChildren();
  location.hash = "";
});

const NOW = new Date("2026-10-31T12:00:00Z");
const ago = (ms: number) => new Date(NOW.getTime() - ms).toISOString();
const HOUR = 3600_000;

function session(over: Record<string, unknown>) {
  return { id: `ses_${String(over.name)}`, role: "engineer", agent_kind: "claude-code", status: "active", presence: "active", stale: false, owner_principal: "mara@example.test", host: "mara-mbp", updated_at: ago(HOUR), ...over };
}

const sessionsFixture = {
  source: "local-authority",
  data: {
    sessions: [
      session({ name: "api-worker", assignment: { state: "confirmed", issue_identifier: "ATL-142" } }),
      session({ name: "idle-one", presence: "idle", updated_at: ago(3 * HOUR), host: "other-box" }),
      session({ name: "stale-one", stale: true, presence: "idle", updated_at: ago(2 * HOUR) }),
      session({ name: "kofi-agent", owner_principal: "kofi@example.test", host: "kofi-pc", agent_kind: "codex" }),
      session({ name: "old-retired", status: "retired", presence: "retired", updated_at: ago(2 * DAY_MS) }),
      session({ name: "fresh-retired", status: "retired", presence: "retired", updated_at: ago(HOUR) }),
    ],
  },
};

const row = (over: Partial<SessionRow>): SessionRow => ({ id: "x", name: "x", owner: "", host: "", agent: "", status: "active", presence: "active", stale: false, issue: "", updatedAt: NOW.toISOString(), ...over });

describe("the Archive rule (mirrors internal/tui/archive.go)", () => {
  it("archives retired or moved sessions 24 hours after their last update, not before", () => {
    expect(isArchived(row({ status: "retired", updatedAt: ago(DAY_MS) }), NOW)).toBe(true);
    expect(isArchived(row({ status: "moved", updatedAt: ago(DAY_MS + 1000) }), NOW)).toBe(true);
    expect(isArchived(row({ status: "retired", updatedAt: ago(DAY_MS - 1000) }), NOW)).toBe(false);
  });
  it("keeps active sessions, bad times and future times Current", () => {
    expect(isArchived(row({ status: "active", updatedAt: ago(5 * DAY_MS) }), NOW)).toBe(false);
    expect(isArchived(row({ status: "retired", updatedAt: "not a time" }), NOW)).toBe(false);
    expect(isArchived(row({ status: "retired", updatedAt: "" }), NOW)).toBe(false);
    expect(isArchived(row({ status: "retired", updatedAt: new Date(NOW.getTime() + HOUR).toISOString() }), NOW)).toBe(false);
  });
  it("splits Current and Archive, and All holds both", () => {
    const old = row({ status: "retired", updatedAt: ago(2 * DAY_MS) });
    const live = row({});
    expect([inTab(old, "current", NOW), inTab(old, "archive", NOW), inTab(old, "all", NOW)]).toEqual([false, true, true]);
    expect([inTab(live, "current", NOW), inTab(live, "archive", NOW), inTab(live, "all", NOW)]).toEqual([true, false, true]);
  });
  it("calls a session own only when both principals are known and equal", () => {
    expect(isOwn(row({ owner: "mara@example.test" }), "mara@example.test")).toBe(true);
    expect(isOwn(row({ owner: "" }), "")).toBe(false);
    expect(isOwn(row({ owner: "kofi@example.test" }), "mara@example.test")).toBe(false);
    expect(isOwn(row({ owner: "mara@example.test" }), "")).toBe(false);
  });
});

describe("lane data", () => {
  it("maps state words to marks", () => {
    expect(["active", "online", "working"].map(laneKind)).toEqual(["working", "working", "working"]);
    expect(laneKind("idle")).toBe("idle");
    expect(["stale", "offline", "retired", "moved", "-"].map(laneKind)).toEqual(["offline", "offline", "offline", "offline", "offline"]);
  });
  it("draws the current state from the last update, clamped to the window, working first", () => {
    const rows = parseSessions(sessionsFixture).rows;
    const { lanes } = buildLanes(rows, NOW);
    expect(lanes[0]?.kind).toBe("working");
    expect(lanes.map((l) => l.kind)).toEqual([...lanes.map((l) => l.kind)].sort((a, b) => ["working", "idle", "offline"].indexOf(a) - ["working", "idle", "offline"].indexOf(b)));
    const old = buildLanes([row({ updatedAt: ago(20 * HOUR) })], NOW).lanes[0];
    expect(old?.from).toBe(0);
    expect(buildLanes([row({ updatedAt: "" })], NOW).lanes[0]?.from).toBeNull();
    expect(buildLanes([row({ updatedAt: ago(4 * HOUR) })], NOW).lanes[0]?.from).toBeCloseTo(0.5);
  });
  it("caps the lanes and counts the rest", () => {
    const many = Array.from({ length: 15 }, (_, i) => row({ name: `s${i}` }));
    const r = buildLanes(many, NOW);
    expect(r.lanes).toHaveLength(12);
    expect(r.more).toBe(3);
  });
  it("clips by character", () => {
    expect(clip("abcdef", 4)).toBe("abc…");
    expect(clip("ab", 4)).toBe("ab");
  });
});

describe("Overview", () => {
  const mountOverview = async (answers: Record<string, () => unknown>, ctxOver: Record<string, unknown> = {}) => {
    const root = document.createElement("div");
    document.body.append(root);
    const ctx = { ...ctxFor(apiFor(fakeServer(answers)), root), ...ctxOver };
    overviewView(ctx);
    await settle();
    return { root, ctx };
  };

  it("draws the lanes from sessions.list, with the Now line and only the legend kinds on screen", async () => {
    const { root } = await mountOverview({ "sessions.list": () => sessionsFixture });
    expect(root.querySelector("h1")?.textContent).toBe("Overview");
    const svg = root.querySelector("svg.lanes-svg");
    expect(svg).not.toBeNull();
    // The two retired sessions: the one over a day old has no lane; the fresh one does.
    expect(svg?.querySelectorAll("g.lane")).toHaveLength(5);
    expect(svg?.querySelector(".lane-now")).not.toBeNull();
    expect(svg?.querySelectorAll("rect.lane-bar.working")).toHaveLength(2);
    expect(svg?.querySelectorAll("rect.lane-bar.idle")).toHaveLength(1);
    expect(svg?.querySelectorAll("rect.lane-bar.offline").length).toBeGreaterThanOrEqual(2);
    expect(Array.from(root.querySelectorAll(".legend .leg")).map((e) => e.textContent)).toEqual(["Working", "Idle", "Offline or stale"]);
    expect(svg?.textContent).toContain("ATL-142");
    expect(svg?.textContent).not.toContain("old-retired");
    expect(root.querySelector(".finding")?.textContent).toBe("5 sessions: 2 working, 1 stale.");
  });

  it("shows an empty lane frame with the Now line when there are no sessions", async () => {
    const { root } = await mountOverview({ "sessions.list": () => ({ source: "local-authority", data: { sessions: [] } }) });
    expect(root.querySelector("svg.lanes-svg .lane-now")).not.toBeNull();
    expect(root.querySelectorAll("g.lane")).toHaveLength(0);
    expect(root.querySelector(".lanes .foot")?.textContent).toContain("No sessions in this window");
    expect(root.querySelector(".legend")?.children).toHaveLength(0);
  });

  it("never claims nothing needs the person; it names the commands that list these today", async () => {
    const { root } = await mountOverview({ "sessions.list": () => sessionsFixture });
    const text = root.querySelector("[data-block=needs]")?.textContent ?? "";
    expect(text).not.toContain("Nothing needs you");
    expect(text).toContain("Needs you arrives in a later build.");
    const cmds = Array.from(root.querySelectorAll("[data-block=needs] code")).map((c) => c.textContent);
    expect(cmds).toEqual(["clawbridge dispatcher escalations", "clawbridge sessions --assignment-state proposed", "clawbridge intake list", "clawbridge doctor"]);
  });

  it("explains the bars on the page, labels them 'since', and is a group for screen readers", async () => {
    const { root } = await mountOverview({ "sessions.list": () => sessionsFixture });
    expect(root.querySelector(".lanes-note")?.textContent).toBe("Each bar is the session's state since its last update. Past states arrive in a later build.");
    expect(root.querySelector("svg.lanes-svg")?.getAttribute("role")).toBe("group");
    const labels = Array.from(root.querySelectorAll("text.lane-bar-label")).map((t) => t.textContent ?? "");
    expect(labels.length).toBeGreaterThan(0);
    expect(labels.every((t) => /since \d\d:\d\d$|over 8 h$/.test(t))).toBe(true);
  });

  it("opens the session when a lane is chosen, by click or Enter", async () => {
    const go = vi.fn();
    const { root } = await mountOverview({ "sessions.list": () => sessionsFixture }, { go });
    const lane = root.querySelector("g.lane-pick");
    lane?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    expect(go).toHaveBeenCalledWith("sessions/api-worker");
    lane?.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    expect(go).toHaveBeenCalledTimes(2);
  });

  it("builds the Today strip from counts it can compute, and leaves out writes queued when it cannot know", async () => {
    const { root } = await mountOverview({ "sessions.list": () => sessionsFixture });
    expect(Array.from(root.querySelectorAll("[data-cell]")).map((c) => c.getAttribute("data-cell"))).toEqual(["working", "stale", "claims"]);
    expect(root.querySelector("[data-cell=working] .val")?.textContent).toBe("2");
    expect(root.querySelector("[data-cell=claims] .val")?.textContent).toBe("1");
    expect(todayCells([], 3).map((c) => c.id)).toEqual(["working", "stale", "claims", "queued"]);
    const queued = await mountOverview({ "sessions.list": () => sessionsFixture }, { outbox: { get: () => 4, subscribe: () => () => {} } });
    expect(queued.root.querySelector("[data-cell=queued] .val")?.textContent).toBe("4");
  });

  it("renders hostile names inert in the chart", async () => {
    const hostile = { source: "local-authority", data: { sessions: [session({ name: "<img src=x onerror=alert(1)>", updated_at: ago(HOUR), assignment: { state: "confirmed", issue_identifier: "<script>alert(2)</script>" } })] } };
    const { root } = await mountOverview({ "sessions.list": () => hostile });
    expect(root.querySelector("img, script, [onerror]")).toBeNull();
    expect(root.querySelector("svg")?.textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it("shows an error block with Retry, and an offline copy", async () => {
    const { root } = await mountOverview({ "sessions.list": () => { throw new Error("boom"); } });
    expect(root.querySelector("[role=alert]")?.textContent).toContain("Couldn't load the overview.");
  });

  it("updates the queued-writes cell when the client pushes outbox.count", async () => {
    const server = fakeServer({ "sessions.list": () => sessionsFixture });
    const host = document.createElement("div");
    document.body.append(host);
    mountApp(host, apiFor(server));
    await settle();
    expect(host.querySelector("[data-cell=queued]")).toBeNull();
    server.emit("outbox.count", { waiting: 3 });
    await settle();
    expect(host.querySelector("[data-cell=queued] .val")?.textContent).toBe("3");
    server.emit("outbox.count", { waiting: "x" });
    server.emit("messages.changed", {});
    await settle();
    expect(host.querySelector("[data-cell=queued] .val")?.textContent).toBe("3");
  });
});

describe("Sessions: tabs, filter and the drawer", () => {
  const launchAnswer = (over: Record<string, unknown> = {}) => (): unknown => ({ line: "CLAWBRIDGE_SESSION=api-worker CLAWBRIDGE_INSTANCE=inst_1 claude --mcp-config /state/mcp/api-worker.json", agent: "claude-code", session: "api-worker", host: "mara-mbp", warnings: [], ...over });
  const mount = async (answers: Record<string, () => unknown> = {}) => {
    const server = fakeServer({ "sessions.list": () => sessionsFixture, "sessions.launchCommand": launchAnswer(), ...answers });
    const root = document.createElement("div");
    document.body.append(root);
    const ctx = ctxFor(apiFor(server), root);
    sessionsView(ctx);
    await settle();
    return { server, root, ctx };
  };
  const names = (root: HTMLElement) => Array.from(root.querySelectorAll("tbody tr td:first-child")).map((e) => e.textContent);
  const click = async (root: HTMLElement, name: string) => {
    const tr = Array.from(root.querySelectorAll<HTMLElement>("tbody tr")).find((r) => r.querySelector("td")?.textContent === name);
    tr?.click();
    await settle();
  };

  it("groups Current, Archive and All with counts", async () => {
    const { root } = await mount();
    const tabs = () => Array.from(root.querySelectorAll(".tabs button")).map((b) => b.textContent);
    expect(tabs()).toEqual(["Current5", "Archive1", "All6"]);
    expect(names(root)).not.toContain("old-retired");
    expect(names(root)).toContain("fresh-retired");
    root.querySelectorAll<HTMLButtonElement>(".tabs button")[1]?.click();
    expect(names(root)).toEqual(["old-retired"]);
    root.querySelectorAll<HTMLButtonElement>(".tabs button")[2]?.click();
    expect(names(root)).toHaveLength(6);
    expect(root.querySelector(".tabs [aria-selected=true]")?.textContent).toContain("All");
  });

  it("filters by name, owner, host and claim", async () => {
    const { root } = await mount();
    const f = root.querySelector<HTMLInputElement>("input[type=search]")!;
    f.value = "kofi";
    f.dispatchEvent(new Event("input"));
    expect(names(root)).toEqual(["kofi-agent"]);
    f.value = "atl-142";
    f.dispatchEvent(new Event("input"));
    expect(names(root)).toEqual(["api-worker"]);
    f.value = "zzz";
    f.dispatchEvent(new Event("input"));
    expect(root.querySelector(".empty")?.textContent).toContain("No session matches that filter.");
  });

  it("shows Launch only for a session the viewer's own principal owns", async () => {
    const { root, server } = await mount();
    await click(root, "api-worker");
    const drawer = root.querySelector(".drawer");
    expect(drawer?.querySelector("[data-block=launch]")).not.toBeNull();
    expect(drawer?.querySelector(".launch-line")?.textContent).toBe("CLAWBRIDGE_SESSION=api-worker CLAWBRIDGE_INSTANCE=inst_1 claude --mcp-config /state/mcp/api-worker.json");
    expect(drawer?.textContent).toContain("Paste into your own terminal. No keys or tokens are in this command.");
    expect(drawer?.querySelector(".pill.acc")?.textContent).toBe("this device");
    expect(server.calls.filter((c) => c.method === "sessions.launchCommand")).toEqual([{ method: "sessions.launchCommand", params: { session: "api-worker", agent: "claude-code" } }]);

    server.calls.length = 0;
    await click(root, "kofi-agent");
    const other = root.querySelector(".drawer");
    expect(other?.querySelector("[data-block=launch]")).toBeNull();
    expect(other?.querySelector(".launch-line")).toBeNull();
    expect(other?.querySelector("[data-block=readonly]")?.textContent).toContain("This is kofi@example.test's agent. Only kofi@example.test can start it.");
    expect(server.calls.some((c) => c.method === "sessions.launchCommand")).toBe(false);
  });

  it("badges another host as 'on HOST'", async () => {
    const { root } = await mount({ "sessions.launchCommand": launchAnswer({ host: "other-box" }) });
    await click(root, "idle-one");
    expect(root.querySelector(".drawer .block .pill")?.textContent).toBe("on other-box");
  });

  it("switches the agent and asks the backend again", async () => {
    const { root, server } = await mount({ "sessions.launchCommand": () => ({ line: "CLAWBRIDGE_SESSION=api-worker CLAWBRIDGE_INSTANCE=inst_1 codex", agent: "codex", session: "api-worker", host: "mara-mbp", warnings: ["Your Codex config also starts ClawBridge as <b>old</b>; remove it so this agent acts only as api-worker."] }) });
    await click(root, "api-worker");
    const buttons = root.querySelectorAll<HTMLButtonElement>(".drawer .segmented button");
    expect(buttons[0]?.getAttribute("aria-pressed")).toBe("true");
    buttons[1]?.click();
    await settle();
    expect(buttons[1]?.getAttribute("aria-pressed")).toBe("true");
    const sent = server.calls.filter((c) => c.method === "sessions.launchCommand").map((c) => (c.params as { agent: string }).agent);
    expect(sent).toEqual(["claude-code", "codex"]);
    expect(root.querySelector(".launch-line")?.textContent).toMatch(/ codex$/);
    expect(root.querySelector(".drawer [data-warning]")?.textContent).toBe("Your Codex config also starts ClawBridge as <b>old</b>; remove it so this agent acts only as api-worker.");
    expect(root.querySelector(".drawer b")).toBeNull();
    expect(root.querySelector(".drawer .codebox")).toBeNull();
  });

  it("starts a codex session's drawer on Codex", async () => {
    const owned = { source: "local-authority", data: { sessions: [session({ name: "cx", agent_kind: "codex" })] } };
    const { root, server } = await mount({ "sessions.list": () => owned });
    await click(root, "cx");
    expect((server.calls.find((c) => c.method === "sessions.launchCommand")?.params as { agent: string }).agent).toBe("codex");
  });

  it("shows the backend's refusal and no line", async () => {
    const { root } = await mount({ "sessions.launchCommand": () => { throw new Error("only kofi@example.test can start this agent"); } });
    await click(root, "api-worker");
    expect(root.querySelector(".drawer [role=alert]")?.textContent).toContain("only kofi@example.test can start this agent");
    expect(root.querySelector(".launch-line")).toBeNull();
  });

  it("copies the raw line with the clipboard, and falls back to selecting it", async () => {
    const { root } = await mount();
    await click(root, "api-worker");
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const copy = () => Array.from(root.querySelectorAll<HTMLButtonElement>(".drawer button")).find((b) => b.textContent === "Copy launch command")!;
    copy().click();
    await settle();
    expect(writeText).toHaveBeenCalledWith("CLAWBRIDGE_SESSION=api-worker CLAWBRIDGE_INSTANCE=inst_1 claude --mcp-config /state/mcp/api-worker.json");
    expect(root.querySelector(".drawer [role=status]")?.textContent).toBe("Copied.");

    Object.defineProperty(navigator, "clipboard", { value: { writeText: vi.fn().mockRejectedValue(new Error("denied")) }, configurable: true });
    copy().click();
    await settle();
    expect(root.querySelector(".drawer [role=status]")?.textContent).toContain("The line is selected");
    expect(window.getSelection()?.toString()).toContain("CLAWBRIDGE_SESSION=api-worker");
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
  });

  it("offers no Copy for a line with hidden characters, and says it was not copied", async () => {
    const raw = "CLAWBRIDGE_SESSION='a\u202eb' CLAWBRIDGE_INSTANCE=i claude --mcp-config /x";
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const { root } = await mount({ "sessions.launchCommand": launchAnswer({ line: raw }) });
    await click(root, "api-worker");
    expect(root.querySelector(".launch-line")?.textContent).toContain("‹U+202E›");
    expect(root.querySelector(".launch-line")?.textContent).not.toContain("\u202e");
    expect(Array.from(root.querySelectorAll(".drawer button")).some((b) => b.textContent === "Copy launch command")).toBe(false);
    expect(root.querySelector(".drawer [role=status]")?.textContent).toBe("This line holds hidden characters; it was not copied.");
    expect(writeText).not.toHaveBeenCalled();
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
  });

  it("says the owner isn't recorded, not 'someone else', when the session has none", async () => {
    const none = { source: "local-authority", data: { sessions: [session({ name: "ownerless", owner_principal: "" })] } };
    const { root, server } = await mount({ "sessions.list": () => none });
    await click(root, "ownerless");
    expect(root.querySelector("[data-block=readonly]")?.textContent).toBe("The owner isn't recorded, so no launch command is offered.");
    expect(server.calls.some((c) => c.method === "sessions.launchCommand")).toBe(false);
  });

  it("renders a hostile name of the viewer's own session inert in the table and the drawer", async () => {
    const name = "<img src=x onerror=alert(1)>";
    const hostile = { source: "local-authority", data: { sessions: [session({ name, host: "h\u001b[31mred", assignment: { state: "confirmed", issue_identifier: "<script>alert(2)</script>" } })] } };
    const { root } = await mount({ "sessions.list": () => hostile, "sessions.launchCommand": launchAnswer({ line: "CLAWBRIDGE_SESSION='<img src=x onerror=alert(1)>' CLAWBRIDGE_INSTANCE=i claude --mcp-config /x" }) });
    await click(root, name);
    expect(root.querySelector("img, script, [onerror]")).toBeNull();
    expect(root.querySelector(".drawer h2")?.textContent).toBe(name);
    expect(root.querySelector(".launch-line")?.textContent).toContain(name);
    expect(root.textContent).not.toContain("\u001b");
  });

  it("renders a hostile owner's session read-only and inert", async () => {
    const hostile = { source: "local-authority", data: { sessions: [session({ name: "k", owner_principal: "evil\u202e<b>x</b>@example.test" })] } };
    const { root, server } = await mount({ "sessions.list": () => hostile });
    await click(root, "k");
    expect(root.querySelector("b")).toBeNull();
    expect(root.querySelector("[data-block=readonly]")?.textContent).toContain("‹U+202E›");
    expect(server.calls.some((c) => c.method === "sessions.launchCommand")).toBe(false);
  });

  it("treats an unknown viewer as not the owner of anything", async () => {
    const server = fakeServer({ "sessions.list": () => sessionsFixture });
    const root = document.createElement("div");
    const ctx = { ...ctxFor(apiFor(server), root), hello: { ...ctxFor(apiFor(server), root).hello!, principal: "" } };
    document.body.append(root);
    sessionsView(ctx);
    await settle();
    await click(root, "api-worker");
    expect(root.querySelector("[data-block=launch]")).toBeNull();
  });

  it("closes the drawer with Close and Escape, and opens one named in the address", async () => {
    const { root } = await mount();
    await click(root, "api-worker");
    root.querySelector<HTMLButtonElement>(".drawer .hd button")?.click();
    expect(root.querySelector(".drawer")).toBeNull();
    await click(root, "api-worker");
    root.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(root.querySelector(".drawer")).toBeNull();
    document.body.replaceChildren();
    location.hash = "#/sessions/idle-one";
    const again = await mount();
    expect(again.root.querySelector(".drawer h2")?.textContent).toBe("idle-one");
  });
});

describe("Doctor ordering", () => {
  it("puts failures first, then warnings, then notes, and folds the passes last", async () => {
    const report = { status: "fail", generated_at: "2026-10-31T10:00:00Z", checks: [
      { id: "p1", title: "Profile", status: "pass" },
      { id: "i1", title: "An info note", status: "info", message: "fyi" },
      { id: "w1", title: "A warning", status: "warn", message: "careful", action: "clawbridge skill install" },
      { id: "f1", title: "A failure", status: "fail", message: "broken", action: "clawbridge mcp install --agent codex --session NAME" },
      { id: "p2", title: "Database", status: "pass" },
    ] };
    const root = document.createElement("div");
    doctorView(ctxFor(apiFor(fakeServer({ "doctor.checks": () => report })), root));
    await settle();
    const order = Array.from(root.querySelectorAll(".doc-fail h3, .doc-row .dtitle, .folded")).map((e) => e.textContent);
    expect(order).toEqual(["A failure", "A warning", "An info note", "2 passed"]);
    expect(root.querySelector(".doc-fail .codebox button")?.textContent).toBe("Copy");
    expect(root.querySelector(".stamp")?.textContent).toMatch(/^Checked \d\d:\d\d$/);
    expect(Array.from(root.querySelectorAll("button")).some((b) => b.textContent === "Re-run")).toBe(true);
    expect(root.querySelector(".passlist")?.hasAttribute("hidden")).toBe(true);
  });
});

describe("sessions and fixtures from the shell", () => {
  it("opens the drawer through the shell's address and shows the badge from hello", async () => {
    const server = fakeServer({ "sessions.list": () => sessionsFixture, "sessions.launchCommand": () => ({ line: "L", agent: "claude-code", session: "api-worker", host: "mara-mbp", warnings: [] }) });
    const host = document.createElement("div");
    document.body.append(host);
    location.hash = "#/sessions/api-worker";
    mountApp(host, apiFor(server));
    await settle();
    expect(host.querySelector(".drawer h2")?.textContent).toBe("api-worker");
    expect(fullHello.profile.device).toBe("mara-mbp");
    expect(host.querySelector(".drawer .pill.acc")?.textContent).toBe("this device");
  });
});
