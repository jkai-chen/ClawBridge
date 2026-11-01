// Account (GUI-D-SPEC §5.7): Devices, Invites, People, Ownership. Hosted
// workspaces only; every write is disabled until P3.
import { h } from "../dom";
import { parseAccountDevices, parseAccountUsers, parseInvites } from "../pages";
import { emptyBlock, safe, titleRow, type View, type ViewCtx } from "./common";
import { ago, buttonTabs, call, dash, dataTable, disabledWrite, loadInto, mono, note, pill } from "./pagekit";

export const LOCAL_ACCOUNT = "Account is for hosted workspaces";

const TABS: [string, string][] = [["devices", "Devices"], ["invites", "Invites"], ["people", "People"], ["ownership", "Ownership"]];

export const accountView: View = (ctx: ViewCtx) => {
  const { root } = ctx;
  if (ctx.hello && ctx.hello.mode !== "cloud") {
    root.replaceChildren(titleRow("Account"), emptyBlock(LOCAL_ACCOUNT, "Local mode has no devices, invites or people to manage."));
    return;
  }
  let tab = "devices";
  const body = h("div", null);
  const tabs = h("div", null);
  const hosted = (flag: boolean, reason: string): HTMLElement | null => (flag ? null : emptyBlock(LOCAL_ACCOUNT, safe(reason) || undefined));

  const devices = (): void => loadInto(ctx, body, "the devices", call(ctx, "account.devices", {}, parseAccountDevices), (a) => {
    const no = hosted(a.hosted, a.reason);
    if (no) return no;
    const unseen = a.events.filter((e) => !e.acked);
    return h("div", { class: "stack" },
      unseen.length ? h("div", { class: "notice", role: "status" }, h("span", { class: "grow", text: `${unseen.length} device ${unseen.length === 1 ? "alert" : "alerts"} you haven't seen: ${unseen.map((e) => `${safe(e.kind)} ${safe(e.label)}`).join(", ")}.` })) : null,
      h("div", { class: "panel" }, a.devices.length === 0 ? emptyBlock("No devices.") : dataTable(
        [{ head: "Device" }, { head: "Person" }, { head: "Kind" }, { head: "Status" }, { head: "Last used", right: true }],
        a.devices.map((d) => [
          h("span", null, mono(d.label || d.id), d.label && d.label === ctx.hello?.device ? " " : null, d.label && d.label === ctx.hello?.device ? pill("This device", "acc") : null),
          dash(d.principal), d.kind === "ci" ? "CI" : "Device", pill(d.status || "unknown", d.status === "active" ? "pos" : d.status === "dormant" ? "warn" : "line"), ago(d.lastUsedAt)]))),
      h("div", { class: "toolbar" }, disabledWrite("Revoke a device")));
  });
  const invites = (): void => loadInto(ctx, body, "the invites", call(ctx, "account.invites", {}, parseInvites), (a) => {
    const no = hosted(a.hosted, a.reason);
    if (no) return no;
    return h("div", { class: "stack" },
      h("div", { class: "panel" }, a.invites.length === 0 ? emptyBlock("No invites.") : dataTable(
        [{ head: "Invited" }, { head: "Status" }, { head: "Grants" }, { head: "Expires", right: true }],
        a.invites.map((i) => [mono(i.email), pill(i.status || "unknown", i.status === "pending" ? "warn" : "line"), i.grants.map(safe).join(", ") || "-", ago(i.expiresAt)]))),
      h("div", { class: "toolbar" }, disabledWrite("Invite someone")));
  });
  const people = (): void => loadInto(ctx, body, "the people", call(ctx, "account.users", {}, parseAccountUsers), (a) => {
    const no = hosted(a.hosted, a.reason);
    if (no) return no;
    if (!a.mainOwner) return emptyBlock("Only the workspace owner sees every member.", "People and roles are in Teams & People.");
    return h("div", { class: "stack" },
      h("div", { class: "panel" }, a.users.length === 0 ? emptyBlock("No members.") : dataTable(
        [{ head: "Person" }, { head: "Roles" }, { head: "Devices", right: true }, { head: "Invited by" }],
        a.users.map((u) => [mono(u.principal), u.roles.map(safe).join(", ") || "No roles", String(u.devices), dash(u.invitedBy)]))),
      note("People and roles are in Teams & People."));
  });
  const ownership = (): void => loadInto(ctx, body, "ownership", call(ctx, "account.devices", {}, parseAccountDevices), (a) => {
    const no = hosted(a.hosted, a.reason);
    if (no) return no;
    const t = a.transfer;
    return h("div", { class: "stack" },
      h("div", { class: "panel pad" }, h("dl", { class: "kv" },
        h("dt", { text: "You" }), h("dd", { text: dash(a.principal) }),
        h("dt", { text: "Workspace owner" }), h("dd", { text: a.mainOwner ? "You" : "Someone else" }),
        h("dt", { text: "Ownership offer" }), h("dd", { text: t ? `Pending${typeof t.to === "string" ? ` to ${safe(t.to)}` : ""}` : "No ownership offer is pending." }))),
      h("div", { class: "toolbar" }, disabledWrite("Offer ownership")));
  });
  const LOADERS: Record<string, () => void> = { devices, invites, people, ownership };
  const paint = (): void => {
    tabs.replaceChildren(buttonTabs(TABS, tab, (id) => { tab = id; paint(); }, "Account"));
    (LOADERS[tab] ?? devices)();
  };
  root.replaceChildren(titleRow("Account", "Devices and invites for this workspace. People and roles are in Teams & People."), tabs, body);
  paint();
};
