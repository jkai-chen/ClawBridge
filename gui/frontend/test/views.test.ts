import { beforeEach, describe, expect, it } from "vitest";
import { Api, parseDoctor, parseSessions } from "../src/api";
import { mountApp } from "../src/shell";
import { doctorView } from "../src/views/doctor";
import { sessionsView } from "../src/views/sessions";
import type { ViewCtx } from "../src/views/common";
import { fixtures } from "./fixtures";
import { RUNNING, apiFor, fakeShell, settle } from "./helpers";

function ctxFor(api: Api, root: HTMLElement): ViewCtx & { badges: Record<string, number> } {
  const badges: Record<string, number> = {};
  return { root, api, status: RUNNING, isCurrent: () => true, setBadge: (n, v) => { badges[n] = v; }, go: () => {}, badges };
}

beforeEach(() => {
  document.body.replaceChildren();
  location.hash = "";
});

describe("parsers", () => {
  it("read the sessions fixture", () => {
    const rows = parseSessions(fixtures.sessionsList);
    expect(rows).toHaveLength(5);
    expect(rows[0]).toMatchObject({ name: "atlas/billing-retry", agent: "claude", issue: "ATL-142", owner: "mara@example.test" });
    expect(rows[2]?.issue).toBe("ATL-149 (proposed)"); // a claim not yet confirmed says so
  });
  it("read the doctor fixture, and never turn an unknown status into a pass", () => {
    const r = parseDoctor(fixtures.doctorChecks);
    expect(r.checks).toHaveLength(14);
    expect(r.checks.filter((c) => c.level === "pass")).toHaveLength(11);
    expect(r.checks.filter((c) => c.level === "fail")).toHaveLength(1);
    expect(parseDoctor({ checks: [{ id: "x", title: "t", status: "bogus" }] }).checks[0]?.level).toBe("info");
  });
  it("survive junk", () => {
    expect(parseSessions(null)).toEqual([]);
    expect(parseSessions("not json")).toEqual([]);
    expect(parseSessions({ sessions: [1, "x", null] })).toHaveLength(3);
    expect(parseDoctor(undefined).checks).toEqual([]);
  });
});

describe("Sessions view", () => {
  it("shows a loading skeleton, then the rows", async () => {
    let release: (v: unknown) => void = () => {};
    const shell = fakeShell({ "sessions.list": () => new Promise((r) => { release = r; }) });
    const root = document.createElement("div");
    sessionsView(ctxFor(apiFor(shell), root));
    expect(root.querySelector("[aria-busy=true]")).not.toBeNull();
    expect(root.querySelectorAll(".sk-bar").length).toBeGreaterThan(5);
    release(fixtures.sessionsList);
    await settle();
    expect(root.querySelector("[aria-busy=true]")).toBeNull();
    expect(root.querySelectorAll("tbody tr")).toHaveLength(5);
    expect(root.textContent).toContain("atlas/billing-retry");
    expect(root.textContent).toContain("5 sessions");
    expect(shell.calls.find((c) => c.method === "Call")?.args[0]).toBe("sessions.list");
  });

  it("shows the empty state", async () => {
    const root = document.createElement("div");
    sessionsView(ctxFor(apiFor(fakeShell({ "sessions.list": () => fixtures.sessionsEmpty })), root));
    await settle();
    expect(root.querySelector(".empty")?.textContent).toContain("No agents connected.");
    expect(root.querySelector("table")).toBeNull();
  });

  it("shows an error block with Retry that loads again", async () => {
    let fail = true;
    const shell = fakeShell({ "sessions.list": () => { if (fail) throw new Error("backend stopped"); return fixtures.sessionsList; } });
    const root = document.createElement("div");
    sessionsView(ctxFor(apiFor(shell), root));
    await settle();
    expect(root.querySelector("[role=alert]")?.textContent).toContain("Couldn't load sessions.");
    fail = false;
    root.querySelector<HTMLButtonElement>("[role=alert] button")?.click();
    await settle();
    expect(root.querySelectorAll("tbody tr")).toHaveLength(5);
  });

  it("renders hostile session data as inert text", async () => {
    const root = document.createElement("div");
    document.body.append(root);
    sessionsView(ctxFor(apiFor(fakeShell({ "sessions.list": () => fixtures.sessionsHostile })), root));
    await settle();
    expect(root.querySelector("img")).toBeNull();
    expect(root.querySelector("script")).toBeNull();
    expect(root.querySelector("[onerror]")).toBeNull();
    const cells = Array.from(root.querySelectorAll("td")).map((td) => td.textContent ?? "");
    expect(cells.some((t) => t.includes("<img src=x onerror=alert(1)>"))).toBe(true);
    const text = root.textContent ?? "";
    expect(text).not.toContain("\u202E");
    expect(text).not.toContain("\u001b");
    expect(text).toContain("‹U+202E›");
    expect(text).toContain("‹U+001B›");
    expect(text).toContain("<script>alert(2)</script>");
  });

  it("drops a late answer once the view is no longer current", async () => {
    let current = true;
    const root = document.createElement("div");
    sessionsView({ ...ctxFor(apiFor(fakeShell({ "sessions.list": () => fixtures.sessionsList })), root), isCurrent: () => current });
    current = false;
    root.replaceChildren();
    await settle();
    expect(root.children).toHaveLength(0);
  });
});

describe("Doctor view", () => {
  it("lists the failure first, the notes next, and folds the passes", async () => {
    const root = document.createElement("div");
    const ctx = ctxFor(apiFor(fakeShell({ "doctor.checks": () => fixtures.doctorChecks })), root);
    doctorView(ctx);
    await settle();
    expect(root.querySelector("h1")?.textContent).toBe("Doctor");
    expect(root.querySelector(".finding")?.textContent).toBe("1 failing, 1 warning, 11 passed.");
    const fail = root.querySelector(".doc-fail");
    expect(fail?.textContent).toContain("MCP entry for Codex points to an old binary path");
    expect(fail?.querySelector(".codebox code")?.textContent).toBe("clawbridge mcp install --agent codex --session NAME");
    expect(root.querySelectorAll(".doc-row")).toHaveLength(2); // one warn, one info
    const list = root.querySelector<HTMLElement>(".passlist");
    expect(list?.hidden).toBe(true);
    root.querySelector<HTMLButtonElement>(".folded")?.click();
    expect(list?.hidden).toBe(false);
    expect(list?.querySelectorAll("li")).toHaveLength(11);
    expect(ctx.badges.doctor).toBe(1);
  });

  it("shows empty and error states", async () => {
    const root = document.createElement("div");
    doctorView(ctxFor(apiFor(fakeShell({ "doctor.checks": () => ({ status: "pass", checks: [] }) })), root));
    await settle();
    expect(root.querySelector(".empty")?.textContent).toContain("No checks reported.");
    const bad = document.createElement("div");
    doctorView(ctxFor(apiFor(fakeShell({ "doctor.checks": () => { throw new Error("boom"); } })), bad));
    await settle();
    expect(bad.querySelector("[role=alert]")?.textContent).toContain("Couldn't load the doctor checks.");
  });

  it("renders a hostile check as inert text", async () => {
    const hostile = { status: "fail", generated_at: "2026-10-31T10:00:00Z", checks: [{ id: "x", title: "<img src=x onerror=alert(1)>", status: "fail", message: "\u202Eevil \u001b[31m", action: "rm -rf <b>" }] };
    const root = document.createElement("div");
    doctorView(ctxFor(apiFor(fakeShell({ "doctor.checks": () => hostile })), root));
    await settle();
    expect(root.querySelector("img, b")).toBeNull();
    expect(root.textContent).toContain("<img src=x onerror=alert(1)>");
    expect(["\u202E", "\u001b"].some((c) => (root.textContent ?? "").includes(c))).toBe(false);
  });
});

describe("the app shell", () => {
  const mount = async (status = RUNNING, answers: Record<string, () => unknown> = { "sessions.list": () => fixtures.sessionsList, "doctor.checks": () => fixtures.doctorChecks }) => {
    const shell = fakeShell(answers, status);
    const host = document.createElement("div");
    document.body.append(host);
    mountApp(host, apiFor(shell));
    await settle();
    return { shell, host };
  };

  it("has the sidebar in order, with Settings at the bottom, a search placeholder and the connection state", async () => {
    const { host } = await mount();
    const labels = Array.from(host.querySelectorAll(".sidebar nav[aria-label=Main] a")).map((a) => a.textContent);
    expect(labels).toEqual(["Overview", "Needs you", "Sessions", "Projects", "Teams & People", "Linear", "Dispatcher", "Account", "Doctor"]);
    expect(host.querySelector(".sidebar .bottom a")?.textContent).toBe("Settings");
    expect(host.querySelector(".search input")?.getAttribute("placeholder")).toBe("Search sessions, issues, messages");
    expect(host.querySelector(".conn")?.textContent).toBe("Connected · client v1.1.0");
  });

  it("routes by hash and marks the current item", async () => {
    const { host } = await mount();
    expect(host.querySelector("h1")?.textContent).toBe("Overview");
    location.hash = "#/sessions";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    await settle();
    expect(host.querySelector("h1")?.textContent).toBe("Sessions");
    expect(host.querySelector("a[aria-current=page]")?.textContent).toBe("Sessions");
    location.hash = "#/not-a-screen";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    await settle();
    expect(host.querySelector("h1")?.textContent).toBe("Overview");
  });

  it("makes every other screen a placeholder", async () => {
    const { host } = await mount();
    for (const id of ["needs-you", "projects", "teams", "linear", "dispatcher", "account"]) {
      location.hash = `#/${id}`;
      window.dispatchEvent(new HashChangeEvent("hashchange"));
      await settle();
      expect(host.querySelector(".empty b")?.textContent).toBe("Not in this build yet.");
    }
  });

  it("says backend stopped, offers Restart, and replays nothing", async () => {
    const { shell, host } = await mount();
    location.hash = "#/sessions";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    await settle();
    const callsBefore = shell.calls.filter((c) => c.method === "Call").length;
    shell.emit("backend:state", { state: "stopped", os: "darwin", code: "exited", reason: "The ClawBridge client stopped." });
    await settle();
    expect(host.querySelector(".state-panel h1")?.textContent).toContain("The ClawBridge client stopped");
    expect(host.querySelector(".conn")?.textContent).toBe("Client stopped");
    host.querySelector<HTMLButtonElement>(".state-panel button")?.click();
    await settle();
    expect(shell.calls.some((c) => c.method === "Restart")).toBe(true);
    expect(shell.calls.filter((c) => c.method === "Call").length).toBe(callsBefore); // the UI re-sent nothing
    shell.emit("backend:state", RUNNING);
    await settle();
    expect(host.querySelectorAll("tbody tr").length).toBe(5); // it loads fresh once running again
  });

  it("names WSL in the stopped and update-needed panels on Windows", async () => {
    const win = { state: "stopped" as const, os: "windows", distro: "Ubuntu", target: "Ubuntu (WSL)" };
    const { shell, host } = await mount(win);
    expect(host.querySelector(".state-panel h1")?.textContent).toBe("The ClawBridge client stopped in Ubuntu (WSL).");
    shell.emit("backend:state", { ...win, state: "update-needed", client_version: "1.0.0" });
    await settle();
    expect(host.querySelector(".state-panel")?.textContent).toContain("This app needs a newer ClawBridge client");
    expect(host.querySelector(".state-panel code")?.textContent).toBe("clawbridge self-update");
    expect(host.querySelector(".state-panel")?.textContent).toContain("In Ubuntu (WSL), run:");
  });

  it("shows the distro picker from the shell's list when setup is needed on Windows", async () => {
    const { shell, host } = await mount({ state: "needs-setup", os: "windows", code: "needs-distro", reason: "Pick the WSL distro." });
    const options = Array.from(host.querySelectorAll("select option")).map((o) => o.textContent);
    expect(options).toEqual(["Ubuntu", "Debian"]);
    host.querySelector<HTMLButtonElement>(".picker .btn.primary")?.click();
    await settle();
    expect(shell.calls.find((c) => c.method === "SetWSLDistro")?.args).toEqual(["Ubuntu"]);
  });

  it("keeps Settings and Logs reachable while the client is down", async () => {
    const { host } = await mount({ state: "stopped", os: "darwin" });
    location.hash = "#/logs";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    await settle();
    expect(host.querySelector("h1")?.textContent).toBe("Logs");
    expect(host.querySelector("pre.logs")?.textContent).toBe("discarded: motd");
  });

  it("follows the Help → Logs menu event", async () => {
    const { shell, host } = await mount();
    shell.emit("ui/open-logs", null);
    await settle();
    expect(host.querySelector("h1")?.textContent).toBe("Logs");
  });
});
