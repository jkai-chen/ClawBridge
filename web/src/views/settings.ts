import { h, visible } from "../dom";
import { routeArg } from "../router";
import { titleRow, type View } from "./common";
import { updatesPanel } from "./updates";

/** Settings opens to Updates first (GUI-D-SPEC §12); General holds this client's facts. */
export const settingsView: View = (ctx) => {
  const { root, hello } = ctx;
  const tab = routeArg(location.hash) === "general" ? "general" : "updates";
  const tabs = h("div", { class: "tabs", role: "tablist" },
    h("a", { href: "#/settings/updates", role: "tab", "aria-selected": String(tab === "updates"), text: "Updates" }),
    h("a", { href: "#/settings/general", role: "tab", "aria-selected": String(tab === "general"), text: "General" }),
  );
  const body = tab === "updates"
    ? updatesPanel(ctx)
    : h("div", { class: "panel pad" }, h("dl", { class: "kv" },
      h("dt", { text: "Client version" }), h("dd", null, h("span", { class: "keyid", text: visible(hello?.clientVersion || "-") })),
      h("dt", { text: "Mode" }), h("dd", { text: visible(hello?.mode || "-") }),
    ));
  root.replaceChildren(titleRow("Settings", tab === "updates" ? "Versions and updates." : "This client."), tabs, body);
};
