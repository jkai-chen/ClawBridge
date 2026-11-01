import { readFileSync } from "node:fs";
import { join } from "node:path";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { mountApp } from "../src/shell";
import { CallError } from "../src/api";
import { RpcError } from "../src/transport";
import { parseBinding, parseDispatcher, parseProjects, parseRoles } from "../src/pages";
import { accountView, LOCAL_ACCOUNT } from "../src/views/account";
import { dispatcherView, lastTest, modeWords } from "../src/views/dispatcher";
import { linearView } from "../src/views/linear";
import { projectsView, THREADS_RATE_LIMITED, THREADS_REFUSED } from "../src/views/projects";
import { linkOf, peopleOf, roleOn, teamsFinding, teamsView, type TeamsData } from "../src/views/teams";
import { apiFor, ctxFor, fakeServer, HELLO, settle } from "./helpers";

const DIR = join(__dirname, "..", "..", "internal", "guibackend", "testdata", "rpc");
const fx = (name: string): unknown => (JSON.parse(readFileSync(join(DIR, name), "utf8")) as { response: { result: unknown } }).response.result;
const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v)) as T;

const ALL: Record<string, string> = {
  "projects.list": "read-projects-list.json", "project.threads": "read-project-threads.json", "project.thread": "read-project-thread.json",
  "linear.connection": "read-linear-connection.json", "linear.binding": "read-linear-binding.json", "linear.tasks": "read-linear-tasks.json",
  "linear.issue": "read-linear-issue.json", "linear.intake": "read-linear-intake.json", "linear.cancelKinds": "read-linear-cancel-kinds.json",
  "linear.projectStatus": "read-linear-project-status.json", "linear.views": "read-linear-views.json", "linear.queue": "read-linear-queue.json",
  "teams.roles": "read-teams-roles.json", "teams.members": "read-teams-members.json", "dispatcher.status": "read-dispatcher-status.json",
  "account.devices": "read-account-devices.json", "account.invites": "read-account-invites.json", "account.users": "read-account-users.json",
};

function answers(over: Record<string, () => unknown> = {}): Record<string, () => unknown> {
  const out: Record<string, () => unknown> = {};
  for (const [m, f] of Object.entries(ALL)) out[m] = () => clone(fx(f));
  return { ...out, ...over };
}

const HOSTILE = "<img src=x onerror=alert(1)>";

async function mount(view: (ctx: ReturnType<typeof ctxFor>) => void, over: Record<string, () => unknown> = {}, hash = "") {
  document.body.replaceChildren();
  location.hash = hash;
  const root = document.createElement("div");
  document.body.append(root);
  const server = fakeServer(answers(over));
  const ctx = ctxFor(apiFor(server), root);
  view(ctx);
  await settle();
  return { root, server, ctx };
}

const noInjected = (root: HTMLElement): void => {
  expect(root.querySelector("img, script, iframe, svg[onload]")).toBeNull();
  for (const el of root.querySelectorAll("*")) for (const a of el.getAttributeNames()) expect(a.startsWith("on")).toBe(false);
};

beforeEach(() => {
  document.body.replaceChildren();
  location.hash = "";
});

describe("every fixture parses to something the pages can show", () => {
  it("projects and overviews", () => {
    const r = parseProjects(fx("read-projects-list.json"));
    expect(r.rows.map((p) => [p.slug, p.role, p.overview?.active])).toEqual([["atlas-api", "owner", 2], ["harbor-web", "member", 1]]);
    expect(r.rows[0]?.overview?.linear.teams.map((t) => t.name)).toEqual(["Atlas", "Harbor"]);
    expect(r.rows[0]?.overview?.linear.cancelKinds).toEqual(["duplicate"]);
  });
  it("binding, roles and dispatcher", () => {
    const b = parseBinding(fx("read-linear-binding.json"));
    expect(b.binding?.teamIds).toEqual(["team_atl", "team_hrb"]);
    expect(b.names.get("team_atl")).toBe("Atlas");
    expect(parseRoles(fx("read-teams-roles.json")).people).toHaveLength(4);
    const d = parseDispatcher(fx("read-dispatcher-status.json"));
    expect(d.mode).toBe("model");
    expect(d.budget).toEqual({ used: 143, limit: 600, resetsAt: "2026-10-31T10:00:00Z" });
  });
});

describe("Projects", () => {
  it("lists the projects with role, board and sessions, and no write control", async () => {
    const { root, server } = await mount(projectsView);
    expect(root.querySelector("h1")?.textContent).toBe("Projects");
    const rows = [...root.querySelectorAll("tbody tr")].map((r) => r.textContent);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toContain("Atlas API");
    expect(rows[0]).toContain("Linear: Atlas, Harbor");
    expect(rows[1]).toContain("No Linear board");
    expect(server.calls.map((c) => c.method)).toEqual(["hello", "projects.list"].filter((m) => m !== "hello"));
  });
  it("shows the empty and error states", async () => {
    let { root } = await mount(projectsView, { "projects.list": () => ({ source: "s", data: { projects: [] }, overviews: {} }) });
    expect(root.textContent).toContain("No projects yet.");
    ({ root } = await mount(projectsView, { "projects.list": () => { throw new CallError("the server said no"); } }));
    expect(root.querySelector("[role=alert]")?.textContent).toContain("Couldn't load the projects.");
    expect(root.textContent).toContain("the server said no");
  });
  it("renders hostile names inert", async () => {
    const hostile = clone(fx("read-projects-list.json")) as { data: { projects: Record<string, unknown>[] } };
    hostile.data.projects[0]!.name = HOSTILE;
    const { root } = await mount(projectsView, { "projects.list": () => hostile });
    noInjected(root);
    expect(root.textContent).toContain(HOSTILE);
  });
  it("opens a project page: Overview, and Threads for an owner", async () => {
    const { root, server } = await mount(projectsView, {}, "#/projects/atlas-api");
    expect(root.querySelector("h1")?.textContent).toBe("Atlas API");
    expect(root.textContent).toContain("lead-atlas");
    const threadsTab = [...root.querySelectorAll<HTMLElement>("[role=tab]")].find((t) => t.textContent === "Threads");
    threadsTab?.click();
    await settle();
    expect(server.calls.some((c) => c.method === "project.threads" && (c.params as { project: string }).project === "atlas-api")).toBe(true);
    expect(root.textContent).toContain("thr_1");
    expect(root.textContent).toContain("Opening a thread is recorded in the project's history.");
    root.querySelector<HTMLElement>("tbody tr")?.click();
    await settle();
    expect(root.textContent).toContain("May I take ATL-212?");
    expect(server.calls.some((c) => c.method === "project.thread")).toBe(true);
  });
  it("says only an owner can read threads, without asking the server, for a member", async () => {
    const { root, server } = await mount(projectsView, {}, "#/projects/harbor-web");
    [...root.querySelectorAll<HTMLElement>("[role=tab]")].find((t) => t.textContent === "Threads")?.click();
    await settle();
    expect(root.textContent).toContain(THREADS_REFUSED);
    expect(server.calls.some((c) => c.method === "project.threads")).toBe(false);
  });
  it("shows the same sentence when the server refuses", async () => {
    const { root } = await mount(projectsView, { "project.threads": () => { throw new CallError("not allowed"); } }, "#/projects/atlas-api");
    [...root.querySelectorAll<HTMLElement>("[role=tab]")].find((t) => t.textContent === "Threads")?.click();
    await settle();
    expect(root.textContent).toContain(THREADS_REFUSED);
    expect(root.textContent).toContain("not allowed");
  });
  it("shows the server's rate limit as friendly text, for the list and for a thread", async () => {
    const limited = () => { throw new CallError("thread reads are limited to 30 a minute; try again shortly"); };
    let { root } = await mount(projectsView, { "project.threads": limited }, "#/projects/atlas-api");
    [...root.querySelectorAll<HTMLElement>("[role=tab]")].find((t) => t.textContent === "Threads")?.click();
    await settle();
    expect(root.textContent).toContain(THREADS_RATE_LIMITED);
    expect(root.textContent).not.toContain(THREADS_REFUSED);
    ({ root } = await mount(projectsView, { "project.thread": limited }, "#/projects/atlas-api"));
    [...root.querySelectorAll<HTMLElement>("[role=tab]")].find((t) => t.textContent === "Threads")?.click();
    await settle();
    root.querySelector<HTMLElement>("tbody tr")?.click();
    await settle();
    expect(root.textContent).toContain(THREADS_RATE_LIMITED);
  });
  it("reads threads only on a click: never on a timer, an event, a re-render or a focus change", async () => {
    document.body.replaceChildren();
    location.hash = "#/projects/atlas-api";
    const host = document.createElement("div");
    document.body.append(host);
    const server = fakeServer(answers());
    mountApp(host, apiFor(server));
    await settle();
    const threadCalls = () => server.calls.filter((c) => c.method === "project.threads" || c.method === "project.thread").length;
    expect(threadCalls()).toBe(0);
    [...host.querySelectorAll<HTMLElement>("[role=tab]")].find((t) => t.textContent === "Threads")?.click();
    await settle();
    host.querySelector<HTMLElement>("tbody tr")?.click();
    await settle();
    const after = threadCalls();
    expect(after).toBe(2);
    vi.useFakeTimers();
    try {
      await vi.advanceTimersByTimeAsync(30 * 60_000);
      server.emit("outbox.count", { waiting: 2 });
      server.emit("needs.changed", {});
      server.emit("messages.changed", {});
      server.setState("lost");
      server.setState("ok");
      window.dispatchEvent(new HashChangeEvent("hashchange"));
      window.dispatchEvent(new Event("focus"));
      window.dispatchEvent(new Event("online"));
      document.dispatchEvent(new Event("visibilitychange"));
      await vi.advanceTimersByTimeAsync(30 * 60_000);
    } finally {
      vi.useRealTimers();
    }
    await settle();
    expect(threadCalls()).toBe(after);
  });
  it("renders a hostile thread message as text", async () => {
    const t = clone(fx("read-project-thread.json")) as { data: { messages: Record<string, unknown>[] } };
    t.data.messages[0]!.body = `${HOSTILE}‮\u001b[31mred`;
    const { root } = await mount(projectsView, { "project.thread": () => t }, "#/projects/atlas-api");
    [...root.querySelectorAll<HTMLElement>("[role=tab]")].find((x) => x.textContent === "Threads")?.click();
    await settle();
    root.querySelector<HTMLElement>("tbody tr")?.click();
    await settle();
    noInjected(root);
    expect(root.textContent).toContain(HOSTILE);
    expect(root.textContent).toContain("‹U+202E›");
    expect(root.textContent).not.toContain("\u001b");
  });
});

describe("Linear", () => {
  const tab = (name: string) => mount(linearView, {}, `#/linear/${name}`);
  it("has the sub-nav and every write disabled with the reason", async () => {
    const { root } = await tab("connection");
    expect([...root.querySelectorAll("[role=tab]")].map((t) => t.textContent)).toEqual(["Connection", "Binding", "Tasks", "Intake", "Who may cancel", "Project status", "Views", "Queued writes"]);
    expect(root.textContent).toContain("Mara Lindqvist");
    expect(root.textContent).toContain("Read and write");
    const writes = [...root.querySelectorAll("button")].filter((b) => b.disabled);
    expect(writes.length).toBeGreaterThan(0);
    for (const b of writes) expect(b.title).toBe("Not in this build yet.");
    expect(root.textContent).toContain("Not in this build yet.");
  });
  it("shows the binding with stored names", async () => {
    const { root } = await tab("binding");
    expect(root.textContent).toContain("Atlas (default), Harbor");
    expect(root.textContent).toContain("Billing retry (closes: agent)");
    expect(root.textContent).toContain("backend");
  });
  it("shows tasks, opens an issue and pages", async () => {
    const { root, server } = await tab("tasks");
    expect(root.textContent).toContain("ATL-212");
    expect(root.textContent).toContain("Retry failed invoices");
    root.querySelector<HTMLElement>("tbody tr")?.click();
    await settle();
    expect(server.calls.find((c) => c.method === "linear.issue")?.params).toEqual({ profile: "lp_mara", issue_id: "iss_1" });
    expect(root.textContent).toContain("Back off, then retry up to 3 times.");
    [...root.querySelectorAll("button")].find((b) => b.textContent === "Load more")?.click();
    await settle();
    expect(server.calls.filter((c) => c.method === "linear.tasks").map((c) => (c.params as { cursor?: string }).cursor)).toEqual([undefined, "cur_2"]);
  });
  it("renders hostile Linear titles and descriptions inert", async () => {
    const page = clone(fx("read-linear-tasks.json")) as { page: { items: Record<string, unknown>[] } };
    page.page.items[0]!.title = HOSTILE;
    const issue = clone(fx("read-linear-issue.json")) as { issue: Record<string, unknown> };
    issue.issue.description = `${HOSTILE}\n<script>alert(2)</script>`;
    const { root } = await mount(linearView, { "linear.tasks": () => page, "linear.issue": () => issue }, "#/linear/tasks");
    root.querySelector<HTMLElement>("tbody tr")?.click();
    await settle();
    noInjected(root);
    expect(root.textContent).toContain(HOSTILE);
    expect(root.textContent).toContain("<script>alert(2)</script>");
  });
  it("shows intake, who may cancel, project status, views and the queue", async () => {
    let r = await tab("intake");
    expect(r.root.textContent).toContain("the needs-agent label");
    expect(r.root.textContent).toContain("Mara Lindqvist, Kofi Mensah");
    expect(r.root.textContent).toContain("HRB-91");
    expect(r.root.textContent).not.toContain("HRB-70");
    r = await tab("cancel");
    expect(r.root.textContent).toContain("duplicate");
    r = await tab("status");
    expect(r.root.textContent).toContain("Billing retry");
    expect(r.root.textContent).toContain("Suggested status: completed");
    r = await tab("views");
    expect(r.root.textContent).toContain("Billing board");
    expect(r.root.textContent).toContain("Linear refused the read");
    r = await tab("queue");
    expect(r.root.textContent).toContain("143 of 600");
    expect(r.root.querySelector("[role=meter]")?.getAttribute("aria-valuenow")).toBe("143");
    expect(r.root.textContent).toContain("ATL-212");
  });
  it("says there is no connection, and shows an error with Retry", async () => {
    let { root } = await mount(linearView, { "linear.connection": () => ({ profiles: [] }) }, "#/linear/tasks");
    expect(root.textContent).toContain("No Linear connection.");
    expect(root.textContent).toContain("clawbridge linear connect --name NAME --access read");
    ({ root } = await mount(linearView, { "linear.binding": () => { throw new CallError("Linear is not connected"); } }, "#/linear/binding"));
    expect(root.querySelector("[role=alert]")?.textContent).toContain("Linear is not connected");
    expect([...root.querySelectorAll("button")].some((b) => b.textContent === "Retry")).toBe(true);
  });
  it("shows the queue as unavailable in local mode", async () => {
    const { root } = await mount(linearView, { "linear.queue": () => fx("read-linear-queue-local.json") }, "#/linear/queue");
    expect(root.textContent).toContain("The write queue is for hosted workspaces.");
  });
  it("shows the empty states", async () => {
    let { root } = await mount(linearView, { "linear.tasks": () => ({ page: { items: [], complete: true } }) }, "#/linear/tasks");
    expect(root.textContent).toContain("No issues on the board.");
    ({ root } = await mount(linearView, { "linear.binding": () => ({ binding: { configured: false } }) }, "#/linear/binding"));
    expect(root.textContent).toContain("No Linear board is linked to this project.");
  });
});

describe("Teams & People", () => {
  it("derives roles, links and trust from the reads", () => {
    const d: TeamsData = {
      roles: parseRoles(fx("read-teams-roles.json")),
      rolesError: "",
      members: [],
      trusted: ["lin_mara"],
      membersError: "",
      binding: null,
      pendingInvites: new Set(["tomas@example.test"]),
      users: new Set(["kofi@example.test", "mara@example.test"]),
      invitesKnown: true,
    };
    expect(roleOn(d.roles, "team_atl", "lin_mara")).toBe("Lead");
    expect(roleOn(d.roles, "team_atl", "lin_ines")).toBe("Reviewer");
    expect(roleOn(d.roles, "team_hrb", "lin_ines")).toBe("Lead");
    expect(roleOn(d.roles, "team_hrb", "lin_mara")).toBe("None");
    expect(linkOf(d, "lin_kofi").state).toBe("linked");
    // Mapped, but neither a user nor invited: not linked, never Linked.
    expect(linkOf(d, "lin_ines").state).toBe("not-linked");
    // The owner cannot be read: a mapping alone is only "mapped".
    expect(linkOf({ ...d, users: null }, "lin_kofi").state).toBe("mapped");
    expect(linkOf({ ...d, users: null }, "lin_tomas").state).toBe("pending");
    expect(linkOf(d, "lin_tomas").state).toBe("pending");
    expect(linkOf(d, "lin_priya").state).toBe("not-linked");
    expect(peopleOf(d).find((p) => p.id === "lin_mara")?.trusted).toBe(true);
  });
  it("shows a tab per bound team plus Everyone, with members from Linear", async () => {
    const { root } = await mount(teamsView);
    const tabs = [...root.querySelectorAll("[role=tab]")].map((t) => t.textContent);
    expect(tabs).toEqual(["Atlas · 4", "Harbor · 3", "Everyone · 5"]);
    expect(root.querySelector(".finding")?.textContent).toBe("2 bound teams, 5 people; 1 not linked to ClawBridge.");
    const rows = [...root.querySelectorAll("tbody tr")].map((r) => r.textContent ?? "");
    expect(rows).toHaveLength(4);
    expect(rows.find((r) => r.includes("Mara"))).toContain("Lead");
    expect(rows.find((r) => r.includes("Mara"))).toContain("Trusted for intake");
    expect(rows.find((r) => r.includes("Priya"))).toContain("Not linked");
    expect(rows.find((r) => r.includes("Kofi"))).toContain("mara@example.test".replace("mara", "kofi"));
    [...root.querySelectorAll<HTMLElement>("[role=tab]")][1]?.click();
    const harbor = [...root.querySelectorAll("tbody tr")].map((r) => r.textContent ?? "");
    expect(harbor.find((r) => r.includes("Tomás"))).toContain("Invite pending");
    expect(harbor.find((r) => r.includes("Ines"))).toContain("Lead");
    expect(root.textContent).toContain("Only the project owner or the team's lead can change roles");
    expect(root.textContent).toContain("not available yet");
    expect(root.querySelectorAll("select, input[type=checkbox]")).toHaveLength(0);
  });
  it("never says Linked for a mapped principal who is neither a user nor invited", async () => {
    const onlyMara = { hosted: true, main_owner: true, users: [{ principal: "mara@example.test", roles: [], devices: [] }] };
    const { root } = await mount(teamsView, { "account.users": () => onlyMara });
    const cell = (name: string) => [...root.querySelectorAll("tbody tr")].find((r) => r.textContent?.includes(name))?.textContent ?? "";
    expect(cell("Mara")).toContain("Linked");
    expect(cell("Kofi")).toContain("Not linked");
    expect(cell("Kofi")).not.toMatch(/(?<!Not )Linked/);
  });
  it("shows a neutral Mapped pill when the viewer cannot read the users", async () => {
    const { root } = await mount(teamsView, { "account.users": () => ({ hosted: true, main_owner: false, users: [] }) });
    const kofi = [...root.querySelectorAll("tbody tr")].find((r) => r.textContent?.includes("Kofi"))?.textContent ?? "";
    expect(kofi).toContain("Mapped to kofi@example.test");
    expect(kofi).not.toMatch(/(?<!Not )Linked/);
    expect(root.textContent).toContain("Whether that person has joined is shown to the workspace owner");
  });
  it("still shows what could be read when a piece fails, and an error when all fail", async () => {
    let { root } = await mount(teamsView, { "teams.roles": () => { throw new CallError("roles unavailable"); }, "account.invites": () => { throw new CallError("local"); } });
    expect(root.textContent).toContain("roles unavailable");
    expect(root.querySelectorAll("tbody tr").length).toBeGreaterThan(0);
    const fail = () => { throw new CallError("down"); };
    ({ root } = await mount(teamsView, { "linear.connection": fail, "teams.roles": fail, "teams.members": fail, "linear.binding": fail, "account.invites": fail }));
    expect(root.querySelector("[role=alert]")).not.toBeNull();
  });
  it("finds nothing bound", () => {
    expect(teamsFinding({ roles: null, rolesError: "", members: [], trusted: [], membersError: "", binding: null, pendingInvites: new Set(), invitesKnown: false, users: null })).toBe("No Linear teams are bound to this project.");
  });
  it("renders hostile member names inert", async () => {
    const m = clone(fx("read-teams-members.json")) as { members: Record<string, unknown>[] };
    m.members[0]!.name = HOSTILE;
    const { root } = await mount(teamsView, { "teams.members": () => m });
    noInjected(root);
    expect(root.textContent).toContain(HOSTILE);
  });
});

describe("Dispatcher", () => {
  it("shows the mode, the last test from real fields, and budget numbers", async () => {
    const { root } = await mount(dispatcherView);
    expect(root.querySelector(".finding")?.textContent).toContain("Model mode");
    expect(root.textContent).toContain("The key of revision 2 is active; its setup test passed when it was set.");
    expect(root.textContent).toContain("example-ai / example-model-1");
    expect(root.querySelector("[role=meter]")?.getAttribute("aria-valuenow")).toBe("143");
    expect(root.textContent).toContain("Not available yet.");
    expect(root.textContent).toContain("Shared by every Linear call in this workspace; the dispatcher's issue reads count against it.");
    expect([...root.querySelectorAll("button")].filter((b) => b.disabled).map((b) => b.textContent)).toEqual(["Set up model", "Rotate key"]);
  });
  it("draws no bar and no test for numbers the server did not send", async () => {
    const s = clone(fx("read-dispatcher-status.json")) as Record<string, unknown>;
    s.budget = null;
    s.doctor = null;
    s.denied = "Only a project owner sees and sets up the dispatcher model.";
    const { root } = await mount(dispatcherView, { "dispatcher.status": () => s });
    expect(root.querySelector("[role=meter]")).toBeNull();
    expect(root.textContent).toContain("Only a project owner sees and sets up the dispatcher model.");
    expect(root.textContent).toContain("Only a project owner sees the model setup test.");
  });
  it("words the mode and a failed test", () => {
    const base = parseDispatcher(fx("read-dispatcher-status.json"));
    expect(modeWords({ ...base, enabled: false })).toContain("Off");
    const failed = { ...base, holders: [{ ...base.holders[0]!, testFailure: "bad key", testRevision: 3 }] };
    expect(lastTest(failed)).toBe("The setup test of revision 3 failed: bad key");
  });
  it("shows the error state", async () => {
    const { root } = await mount(dispatcherView, { "dispatcher.status": () => { throw new CallError("boom"); } });
    expect(root.querySelector("[role=alert]")?.textContent).toContain("Couldn't load the dispatcher.");
  });
});

describe("Account", () => {
  it("shows devices with this device marked and the unseen alert", async () => {
    const { root } = await mount(accountView);
    expect([...root.querySelectorAll("[role=tab]")].map((t) => t.textContent)).toEqual(["Devices", "Invites", "People", "Ownership"]);
    expect(root.textContent).toContain("1 device alert you haven't seen: added mara-air");
    expect(root.textContent).toContain("This device");
    expect(root.textContent).toContain("ci-runner");
    expect([...root.querySelectorAll("button")].filter((b) => b.disabled)).toHaveLength(1);
  });
  it("shows invites, people and ownership", async () => {
    const { root } = await mount(accountView);
    const click = async (name: string) => {
      [...root.querySelectorAll<HTMLElement>("[role=tab]")].find((t) => t.textContent === name)?.click();
      await settle();
    };
    await click("Invites");
    expect(root.textContent).toContain("tomas@example.test");
    expect(root.textContent).toContain("harbor-web=member");
    expect(root.textContent).toContain("in ");
    await click("People");
    expect(root.textContent).toContain("kofi@example.test");
    await click("Ownership");
    expect(root.textContent).toContain("No ownership offer is pending.");
  });
  it("says Account is for hosted workspaces in local mode, and when the server says so", async () => {
    document.body.replaceChildren();
    const root = document.createElement("div");
    document.body.append(root);
    const server = fakeServer(answers());
    const ctx = ctxFor(apiFor(server), root);
    accountView({ ...ctx, hello: { ...HELLO, mode: "local" } });
    await settle();
    expect(root.textContent).toContain(LOCAL_ACCOUNT);
    expect(server.calls.map((c) => c.method)).not.toContain("account.devices");
    const { root: r2 } = await mount(accountView, { "account.devices": () => fx("read-account-local.json") });
    expect(r2.textContent).toContain(LOCAL_ACCOUNT);
  });
  it("shows the refusal text for a non-owner's People tab and an error", async () => {
    const { root } = await mount(accountView, { "account.users": () => ({ hosted: true, main_owner: false, users: [] }), "account.devices": () => { throw new RpcError("devices unavailable", -32000); } });
    expect(root.querySelector("[role=alert]")?.textContent).toContain("Couldn't load the devices.");
    [...root.querySelectorAll<HTMLElement>("[role=tab]")].find((t) => t.textContent === "People")?.click();
    await settle();
    expect(root.textContent).toContain("Only the workspace owner sees every member.");
  });
  it("renders a hostile device label inert", async () => {
    const d = clone(fx("read-account-devices.json")) as { devices: Record<string, unknown>[] };
    d.devices[0]!.label = HOSTILE;
    const { root } = await mount(accountView, { "account.devices": () => d });
    noInjected(root);
    expect(root.textContent).toContain(HOSTILE);
  });
});
