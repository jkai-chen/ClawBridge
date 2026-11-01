// Teams & People (GUI-D-SPEC §11): who works on each bound team, read only.
// Members come from Linear; the link status is derived from the roles
// mapping and the invites; roles and intake trust are shown as text.
import { h } from "../dom";
import { plural } from "../format";
import { parseAccountUsers, parseBinding, parseInvites, parseRoles, parseTeamMembers, type BindingPage, type RolesView } from "../pages";
import { emptyBlock, safe, titleRow, type View, type ViewCtx } from "./common";
import { buttonTabs, call, dataTable, loadInto, mono, note, pill } from "./pagekit";

export type LinkState = "linked" | "pending" | "mapped" | "not-linked";

export interface Person {
  id: string;
  name: string;
  teams: string[];
  role: string; // per team, filled when shown
  principal: string;
  link: LinkState;
  trusted: boolean;
}

export interface TeamsData {
  roles: RolesView | null;
  rolesError: string;
  members: { id: string; name: string; teamId: string }[];
  trusted: string[];
  membersError: string;
  binding: BindingPage | null;
  pendingInvites: Set<string>;
  invitesKnown: boolean;
  /** The workspace's users (lower-case), when the viewer is the workspace owner and may read them; null otherwise. */
  users: Set<string> | null;
}

const cap = (s: string): string => (s ? s[0]!.toUpperCase() + s.slice(1) : s);

/** The role a person has on a team: a team lead, else the mapping's one role if the team is among theirs. */
export function roleOn(roles: RolesView | null, team: string, id: string): string {
  if (!roles) return "";
  if (roles.teams.find((t) => t.teamId === team)?.leads.includes(id)) return "Lead";
  const p = roles.people.find((x) => x.id === id);
  return p && p.teams.includes(team) ? cap(p.role) : "None";
}

export function linkOf(d: TeamsData, id: string): { state: LinkState; principal: string } {
  const person = d.roles?.people.find((x) => x.id === id);
  if (!person?.principal) return { state: "not-linked", principal: "" };
  const who = person.principal.toLowerCase();
  // "Linked" needs data: the principal is a user the owner can see. A mapping alone is only an assertion.
  if (d.pendingInvites.has(who)) return { state: "pending", principal: person.principal };
  if (d.users === null) return { state: "mapped", principal: person.principal };
  return { state: d.users.has(who) ? "linked" : "not-linked", principal: person.principal };
}

/** Every person the page knows: Linear members plus the mapping's people. */
export function peopleOf(d: TeamsData): Person[] {
  const byId = new Map<string, Person>();
  const add = (id: string, name: string, team: string): void => {
    const p = byId.get(id) ?? { id, name: name || id, teams: [], role: "", principal: "", link: "not-linked" as LinkState, trusted: false };
    if (name && p.name === id) p.name = name;
    if (team && !p.teams.includes(team)) p.teams.push(team);
    byId.set(id, p);
  };
  for (const m of d.members) add(m.id, m.name, m.teamId);
  for (const x of d.roles?.people ?? []) add(x.id, d.roles?.users.get(x.id) ?? "", "");
  for (const p of byId.values()) {
    const l = linkOf(d, p.id);
    p.link = l.state;
    p.principal = l.principal;
    p.trusted = d.trusted.includes(p.id);
  }
  return [...byId.values()];
}

export function teamsFinding(d: TeamsData): string {
  const teams = d.binding?.binding?.teamIds ?? [];
  const people = peopleOf(d);
  const unlinked = people.filter((p) => p.link === "not-linked").length;
  if (teams.length === 0) return "No Linear teams are bound to this project.";
  return `${plural(teams.length, "bound team")}, ${plural(people.length, "person", "people")}; ${unlinked} not linked to ClawBridge.`;
}

function linkCell(p: Person): HTMLElement {
  if (p.link === "linked") return h("span", null, h("span", { class: "keyid m", text: safe(p.principal) }), " ", pill("Linked", "pos"));
  if (p.link === "pending") return h("span", null, h("span", { class: "keyid m", text: safe(p.principal) }), " ", pill("Invite pending", "warn"));
  if (p.link === "mapped") return h("span", null, pill(`Mapped to ${p.principal}`, "line"));
  return pill("Not linked", "line");
}

function table(d: TeamsData, people: Person[], team: string | null): HTMLElement {
  if (people.length === 0) return emptyBlock("No members read from Linear for this team.", d.membersError || undefined);
  const cols = [{ head: "Person" }, { head: "ClawBridge link" }, { head: team ? "Role on this team" : "Role" }, { head: "Intake trust" }];
  return dataTable(cols, people.map((p) => [
    h("div", null, h("div", { text: safe(p.name) }), mono(p.id)),
    linkCell(p),
    team ? roleOn(d.roles, team, p.id) || "-" : p.teams.map((t) => `${safe(d.binding?.names.get(t) || t)}: ${roleOn(d.roles, t, p.id)}`).join(", ") || "-",
    p.trusted ? "Trusted for intake" : "Not trusted",
  ]));
}

export const teamsView: View = (ctx: ViewCtx) => {
  const host = h("div", null);
  ctx.root.replaceChildren(titleRow("Teams & People", "Loading teams."), host);
  loadInto<TeamsData>(ctx, host, "teams and people", async () => {
    const profiles = await ctx.api.rpc("linear.connection");
    const profile = (profiles as { profiles?: { id?: string }[] }).profiles?.[0]?.id ?? "";
    const settled = await Promise.allSettled([
      call(ctx, "teams.roles", { profile }, parseRoles)(),
      call(ctx, "teams.members", { profile }, parseTeamMembers)(),
      call(ctx, "linear.binding", {}, parseBinding)(),
      call(ctx, "account.invites", {}, parseInvites)(),
      call(ctx, "account.users", {}, parseAccountUsers)(),
    ]);
    const [roles, members, binding, invites, users] = settled;
    // If nothing at all could be read, it is an error; a piece may be missing.
    if (settled.every((s) => s.status === "rejected")) throw (roles as PromiseRejectedResult).reason;
    const why = (s: PromiseSettledResult<unknown>): string => (s.status === "rejected" ? String((s.reason as Error)?.message ?? s.reason) : "");
    return {
      roles: roles.status === "fulfilled" ? roles.value : null, rolesError: why(roles),
      members: members.status === "fulfilled" ? members.value.members.filter((m) => m.teamId) : [],
      trusted: members.status === "fulfilled" ? members.value.trusted : [],
      membersError: members.status === "fulfilled" ? members.value.namesNote : why(members),
      binding: binding.status === "fulfilled" ? binding.value : null,
      pendingInvites: new Set(invites.status === "fulfilled" ? invites.value.invites.filter((i) => i.status === "pending").map((i) => i.email.toLowerCase()) : []),
      invitesKnown: invites.status === "fulfilled" && invites.value.hosted,
      users: users.status === "fulfilled" && users.value.hosted && users.value.mainOwner ? new Set(users.value.users.map((u) => u.principal.toLowerCase())) : null,
    };
  }, (d) => {
    const teamIds = d.binding?.binding?.teamIds ?? d.roles?.teams.map((t) => t.teamId) ?? [];
    const people = peopleOf(d);
    let tab = teamIds[0] ?? "everyone";
    const body = h("div", null);
    const tabs = h("div", null);
    const paint = (): void => {
      const items: [string, string][] = [...teamIds.map((t) => [t, `${d.binding?.names.get(t) || t} · ${people.filter((p) => p.teams.includes(t)).length}`] as [string, string]), ["everyone", `Everyone · ${people.length}`]];
      tabs.replaceChildren(buttonTabs(items.map(([id, text]) => [id, safe(text)] as [string, string]), tab, (id) => { tab = id; paint(); }, "Teams"));
      const shown = tab === "everyone" ? people : people.filter((p) => p.teams.includes(tab));
      body.replaceChildren(
        h("div", { class: "panel" }, table(d, shown, tab === "everyone" ? null : tab)),
        h("div", { class: "stack", style: "margin-top:12px" },
          note("Only the project owner or the team's lead can change roles. Role changes arrive in a later build."),
          note("A person has one role across their teams today. Linking a person without a role, and lane pins per person, are not available yet."),
          d.roles?.namesErr ? note(`Names: ${d.roles.namesErr}`) : null,
          d.rolesError ? note(`Roles couldn't be read: ${d.rolesError}`) : null,
          d.membersError ? note(d.membersError) : null,
          d.users === null && people.some((p) => p.link === "mapped") ? note("Whether that person has joined is shown to the workspace owner") : null,
          d.invitesKnown ? null : note("Invite status needs a hosted workspace; people without a mapped person show as Not linked."),
          d.roles ? note(`Roles revision ${d.roles.revision}. Intake trust applies to the whole project.`) : null,
          d.roles?.owner ? note("You own this project.") : d.roles?.youLead.length ? note("You lead " + d.roles.youLead.map((t) => d.binding?.names.get(t) || t).join(", ") + ".") : null));
    };
    paint();
    ctx.root.firstElementChild?.replaceWith(titleRow("Teams & People", teamsFinding(d)));
    return [tabs, body];
  });
};
