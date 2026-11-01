import { readFileSync } from "node:fs";
import { join } from "node:path";
import { beforeEach, describe, expect, it } from "vitest";
import { parseDoctor, parseHello, parseSessions } from "../src/api";
import { mountApp, connectionText, initial } from "../src/shell";
import { doctorView } from "../src/views/doctor";
import { sessionsView } from "../src/views/sessions";
import { ConnectionError } from "../src/transport";
import { apiFor, ctxFor, fakeServer, fixtures, lost, settle } from "./helpers";
import { fullHello, sessionsEmpty, sessionsHostile } from "./fixtures";

beforeEach(() => {
  document.body.replaceChildren();
  location.hash = "";
});

describe("parsers", () => {
  it("read sessions.list's {source, data} result", () => {
    const r = parseSessions(fixtures.sessionsList);
    expect(r.source).toBe("local-authority");
    expect(r.rows).toHaveLength(2);
    expect(r.rows[0]).toMatchObject({ name: "engineer-a", agent: "claude-code", presence: "active" });
    expect(r.rows[1]?.stale).toBe(true);
  });
  it("read an assignment, and say when a claim is not confirmed", () => {
    const r = parseSessions({ source: "server", data: { sessions: [{ name: "a", assignment: { state: "proposed", issue_identifier: "ATL-1" } }] } });
    expect(r.rows[0]?.issue).toBe("ATL-1 (proposed)");
  });
  it("read the doctor fixture, and never turn an unknown status into a pass", () => {
    const r = parseDoctor(fixtures.doctorChecks);
    expect(r.checks.map((c) => c.level)).toEqual(["pass", "warn"]);
    expect(r.checks[1]?.action).toBe("clawbridge skill install");
    expect(parseDoctor({ checks: [{ id: "x", title: "t", status: "bogus" }] }).checks[0]?.level).toBe("info");
  });
  it("read hello, the fixture's minimum and the full profile", () => {
    expect(parseHello(fixtures.hello)).toMatchObject({ protocol: 1, mode: "local", workspace: "ws_example", principal: "", device: "" });
    expect(parseHello(fullHello)).toMatchObject({ workspace: "Northwind Labs", principal: "mara@example.test", device: "mara-mbp" });
  });
  it("survive junk", () => {
    expect(parseSessions(null).rows).toEqual([]);
    expect(parseSessions("not json").rows).toEqual([]);
    expect(parseSessions({ data: { sessions: [1, "x", null] } }).rows).toHaveLength(3);
    expect(parseDoctor(undefined).checks).toEqual([]);
    expect(parseHello(undefined).protocol).toBe(0);
  });
});

describe("Sessions view", () => {
  it("shows a loading skeleton, then the rows", async () => {
    let release: (v: unknown) => void = () => {};
    const server = fakeServer({ "sessions.list": () => new Promise((r) => { release = r; }) });
    const root = document.createElement("div");
    sessionsView(ctxFor(apiFor(server), root));
    expect(root.querySelector("[aria-busy=true]")).not.toBeNull();
    expect(root.querySelectorAll(".sk-bar").length).toBeGreaterThan(5);
    release(fixtures.sessionsList);
    await settle();
    expect(root.querySelector("[aria-busy=true]")).toBeNull();
    expect(root.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(root.textContent).toContain("engineer-a");
    expect(root.textContent).toContain("2 sessions, 1 stale");
    expect(server.calls.find((c) => c.method === "sessions.list")).toBeDefined();
  });

  it("shows the empty state", async () => {
    const root = document.createElement("div");
    sessionsView(ctxFor(apiFor(fakeServer({ "sessions.list": () => sessionsEmpty })), root));
    await settle();
    expect(root.querySelector(".empty")?.textContent).toContain("No agents connected.");
    expect(root.querySelector("table")).toBeNull();
  });

  it("shows an error block with Retry that loads again", async () => {
    let fail = true;
    const server = fakeServer({ "sessions.list": () => { if (fail) throw new Error("boom"); return fixtures.sessionsList; } });
    const root = document.createElement("div");
    sessionsView(ctxFor(apiFor(server), root));
    await settle();
    expect(root.querySelector("[role=alert]")?.textContent).toContain("Couldn't load sessions.");
    fail = false;
    root.querySelector<HTMLButtonElement>("[role=alert] button")?.click();
    await settle();
    expect(root.querySelectorAll("tbody tr")).toHaveLength(2);
  });

  it("hands a lost connection to the shell instead of drawing an error", async () => {
    const root = document.createElement("div");
    const ctx = ctxFor(apiFor(fakeServer({ "sessions.list": lost })), root);
    sessionsView(ctx);
    await settle();
    expect(ctx.seen.connection).toEqual(["lost"]);
    expect(root.querySelector("[role=alert]")).toBeNull();
  });

  it("marks an offline copy", async () => {
    const copy = { source: "offline-mirror", data: { sessions: [], mirror: { updated_at: "2026-10-31T10:42:00Z" } } };
    const root = document.createElement("div");
    const ctx = ctxFor(apiFor(fakeServer({ "sessions.list": () => copy })), root);
    sessionsView(ctx);
    await settle();
    expect(ctx.seen.offline.at(-1)).toMatch(/^\d\d:\d\d$/);
    expect(root.querySelector(".stamp")?.textContent).toMatch(/^Copy from \d\d:\d\d$/);
  });

  it("says 'a saved copy' and stamps 'Saved copy' when the mirror has no time, never now", async () => {
    const copy = { source: "offline-mirror", data: { sessions: [] } };
    const root = document.createElement("div");
    const ctx = ctxFor(apiFor(fakeServer({ "sessions.list": () => copy })), root);
    sessionsView(ctx);
    await settle();
    expect(ctx.seen.offline.at(-1)).toBe("");
    expect(root.querySelector(".stamp")?.textContent).toBe("Saved copy");
    expect(connectionText("ok", null, "").text).toBe("Offline, showing a saved copy");
  });

  it("renders hostile session data as inert text", async () => {
    const root = document.createElement("div");
    document.body.append(root);
    sessionsView(ctxFor(apiFor(fakeServer({ "sessions.list": () => sessionsHostile })), root));
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
    sessionsView({ ...ctxFor(apiFor(fakeServer({ "sessions.list": () => fixtures.sessionsList })), root), isCurrent: () => current });
    current = false;
    root.replaceChildren();
    await settle();
    expect(root.children).toHaveLength(0);
  });
});

describe("Doctor view", () => {
  const failing = { status: "fail", generated_at: "2026-10-31T10:00:00Z", checks: [
    { id: "mcp", title: "MCP entry for Codex points to an old binary path", status: "fail", message: "old path", action: "clawbridge mcp install --agent codex --session NAME" },
    { id: "skill", title: "Codex skill is one version behind", status: "warn", message: "behind", action: "clawbridge skill install" },
    { id: "tick", title: "Watcher tick", status: "warn", message: "last good tick 9 min ago" },
    { id: "ok", title: "Profile", status: "pass" },
  ] };

  it("lists the failure first, the notes next, and folds the passes", async () => {
    const root = document.createElement("div");
    const ctx = ctxFor(apiFor(fakeServer({ "doctor.checks": () => failing })), root);
    doctorView(ctx);
    await settle();
    expect(root.querySelector("h1")?.textContent).toBe("Doctor");
    expect(root.querySelector(".finding")?.textContent).toBe("1 failing, 2 warnings, 1 passed.");
    expect(root.querySelector(".doc-fail .codebox code")?.textContent).toBe("clawbridge mcp install --agent codex --session NAME");
    expect(root.querySelectorAll(".doc-row")).toHaveLength(2);
    const list = root.querySelector<HTMLElement>(".passlist");
    expect(list?.hidden).toBe(true);
    root.querySelector<HTMLButtonElement>(".folded")?.click();
    expect(list?.hidden).toBe(false);
    expect(ctx.badges.doctor).toBe(1);
  });

  it("gives the fail block's and the note row's command box the same width rule", () => {
    // One .codebox rule, no per-container width (R2 L6).
    const css = readFileSync(join(__dirname, "..", "src", "styles.css"), "utf8");
    expect(css).toMatch(/\.codebox \{[^}]*width: 100%/);
    expect(css).not.toMatch(/\.doc-fail \.codebox|\.fix-line \.codebox|\.state-panel \.codebox/);
  });

  it("shows the live fixture", async () => {
    const root = document.createElement("div");
    doctorView(ctxFor(apiFor(fakeServer({ "doctor.checks": () => fixtures.doctorChecks })), root));
    await settle();
    expect(root.querySelector(".finding")?.textContent).toBe("1 warning, 1 passed.");
    expect(root.querySelector(".doc-row .codebox code")?.textContent).toBe("clawbridge skill install");
  });

  it("shows empty and error states, and hands a lost connection to the shell", async () => {
    const root = document.createElement("div");
    doctorView(ctxFor(apiFor(fakeServer({ "doctor.checks": () => ({ status: "pass", checks: [] }) })), root));
    await settle();
    expect(root.querySelector(".empty")?.textContent).toContain("No checks reported.");
    const bad = document.createElement("div");
    doctorView(ctxFor(apiFor(fakeServer({ "doctor.checks": () => { throw new Error("boom"); } })), bad));
    await settle();
    expect(bad.querySelector("[role=alert]")?.textContent).toContain("Couldn't load the doctor checks.");
    const gone = document.createElement("div");
    const ctx = ctxFor(apiFor(fakeServer({ "doctor.checks": () => { throw new ConnectionError("unauthorized"); } })), gone);
    doctorView(ctx);
    await settle();
    expect(ctx.seen.connection).toEqual(["unauthorized"]);
  });

  it("renders a hostile check as inert text", async () => {
    const hostile = { status: "fail", generated_at: "2026-10-31T10:00:00Z", checks: [{ id: "x", title: "<img src=x onerror=alert(1)>", status: "fail", message: "\u202Eevil \u001b[31m", action: "rm -rf <b>" }] };
    const root = document.createElement("div");
    doctorView(ctxFor(apiFor(fakeServer({ "doctor.checks": () => hostile })), root));
    await settle();
    expect(root.querySelector("img, b")).toBeNull();
    expect(root.textContent).toContain("<img src=x onerror=alert(1)>");
    expect(["\u202E", "\u001b"].some((c) => (root.textContent ?? "").includes(c))).toBe(false);
  });
});

describe("the app shell", () => {
  const mount = async (answers: Record<string, () => unknown> = { "sessions.list": () => fixtures.sessionsList, "doctor.checks": () => fixtures.doctorChecks }) => {
    const server = fakeServer(answers);
    const host = document.createElement("div");
    document.body.append(host);
    mountApp(host, apiFor(server));
    await settle();
    return { server, host };
  };
  const visit = async (hash: string) => {
    location.hash = hash;
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    await settle();
  };

  it("has the sidebar in order, Settings at the bottom, a search placeholder and the connection state", async () => {
    const { host } = await mount();
    const labels = Array.from(host.querySelectorAll(".sidebar nav[aria-label=Main] a")).map((a) => a.textContent);
    expect(labels).toEqual(["Overview", "Needs you", "Sessions", "Projects", "Teams & People", "Linear", "Dispatcher", "Account", "Doctor"]);
    expect(host.querySelector(".sidebar .bottom a")?.textContent).toBe("Settings");
    expect(host.querySelector(".search input")?.getAttribute("placeholder")).toBe("Search sessions, issues, messages");
    expect(host.querySelector(".conn")?.textContent).toBe("Connected · client v1.0.0-test");
  });

  it("fills the workspace, the device and the person's initial from hello (R2 M3)", async () => {
    const { host } = await mount();
    expect(host.querySelector(".ws-btn b")?.textContent).toBe("Northwind Labs");
    expect(host.querySelector(".ws-btn .t span")?.textContent).toBe("atlas-api");
    expect(host.querySelector(".dev")?.textContent).toBe("This device: mara-mbp");
    expect(host.querySelector(".topbar .avatar")?.textContent).toBe("M");
  });

  it("leaves the device line and the initial out when hello has neither", async () => {
    const { host } = await mount({ hello: () => fixtures.hello });
    expect(host.querySelector<HTMLElement>(".dev")?.hidden).toBe(true);
    expect(host.querySelector<HTMLElement>(".topbar .avatar")?.hidden).toBe(true);
    expect(host.querySelector(".ws-btn b")?.textContent).toBe("ws_example");
  });

  it("routes by hash and marks the current item", async () => {
    const { host } = await mount();
    expect(host.querySelector("h1")?.textContent).toBe("Overview");
    await visit("#/sessions");
    expect(host.querySelector("h1")?.textContent).toBe("Sessions");
    expect(host.querySelector("a[aria-current=page]")?.textContent).toBe("Sessions");
    await visit("#/not-a-screen");
    expect(host.querySelector("h1")?.textContent).toBe("Overview");
  });

  it("makes every other screen a placeholder that says 'Not in this build yet.'", async () => {
    const { host } = await mount();
    for (const id of ["overview", "needs-you", "projects", "teams", "linear", "dispatcher", "account"]) {
      await visit(`#/${id}`);
      expect(host.querySelector(".empty b")?.textContent).toBe("Not in this build yet.");
    }
    expect(host.textContent).not.toMatch(/next release/i);
  });

  it("shows 'Offline, showing the copy from …' when sessions come from the mirror", async () => {
    const copy = { source: "offline-mirror", data: { sessions: [], mirror: { updated_at: "2026-10-31T10:42:00Z" } } };
    const { host } = await mount({ "sessions.list": () => copy });
    await visit("#/sessions");
    expect(host.querySelector(".conn")?.textContent).toMatch(/^Offline, showing the copy from \d\d:\d\d$/);
    expect(host.querySelector(".dot.off")).not.toBeNull();
  });

  it("shows Connection lost when the stream drops, and recovers with a fresh load", async () => {
    const { server, host } = await mount();
    await visit("#/sessions");
    expect(host.querySelectorAll("tbody tr")).toHaveLength(2);
    server.setState("lost");
    await settle();
    expect(host.querySelector(".state-panel h1")?.textContent).toBe("Connection lost");
    expect(host.querySelector(".state-panel")?.textContent).toContain("ClawBridge UI stopped (it stops a minute after its last tab closes). Run clawbridge ui again.");
    expect(host.querySelector(".conn")?.textContent).toBe("Connection lost");
    server.setState("ok");
    await settle();
    expect(host.querySelectorAll("tbody tr")).toHaveLength(2);
  });

  it("shows 'Open this from clawbridge ui' for no secret and for a refused one", async () => {
    for (const kind of ["no-secret", "unauthorized"] as const) {
      document.body.replaceChildren();
      const { server, host } = await mount();
      server.setState(kind);
      await settle();
      expect(host.querySelector(".state-panel h1")?.textContent).toBe("Open this from clawbridge ui");
      expect(host.querySelector(".state-panel h1 code")?.textContent).toBe("clawbridge ui");
      expect(host.querySelector(".state-panel")?.textContent).toContain("This tab isn't connected. Run clawbridge ui in your terminal to open ClawBridge.");
    }
  });

  it("asks for a newer client when the protocol differs (R2 M2)", async () => {
    const { host } = await mount({ hello: () => ({ ...fullHello, protocol: 0 }) });
    expect(host.querySelector(".state-panel")?.textContent).toContain("Reload this tab.");
    expect(host.querySelector(".state-panel")?.textContent).toContain("this app needs a newer ClawBridge client");
    expect(host.querySelector(".state-panel code")?.textContent).toBe("clawbridge self-update");
  });

  it("a view that finds the connection gone swaps in the state panel", async () => {
    const { host } = await mount({ "sessions.list": lost });
    await visit("#/sessions");
    expect(host.querySelector(".state-panel h1")?.textContent).toBe("Connection lost");
  });
});

describe("connectionText and initial", () => {
  it("covers each connection state", () => {
    expect(connectionText("connecting", null, null).text).toBe("Connecting…");
    expect(connectionText("lost", null, null).text).toBe("Connection lost");
    expect(connectionText("no-secret", null, null).text).toBe("Not connected");
    expect(connectionText("ok", null, "10:42").text).toBe("Offline, showing the copy from 10:42");
    expect(connectionText("ok", { ...parseHello(fullHello), platform: "wsl" }, null).text).toBe("WSL · client v1.0.0-test");
    expect(connectionText("ok", { ...parseHello({ ...fullHello, platform: "wsl", distro: "Ubuntu" }) }, null).text).toBe("Ubuntu (WSL) · client v1.0.0-test");
    expect(parseHello(fullHello).distro).toBe("");
  });
  it("takes a person's first character, whole code point", () => {
    expect(initial("mara@example.test")).toBe("M");
    expect(initial("  ines")).toBe("I");
    expect(initial("")).toBe("");
  });
});
