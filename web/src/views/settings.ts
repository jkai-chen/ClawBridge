import { h, visible } from "../dom";
import { titleRow, type View } from "./common";

/** Settings opens to Updates first; those arrive later. */
export const settingsView: View = ({ root, hello }) => {
  const info = h("dl", { class: "kv" },
    h("dt", { text: "Client version" }), h("dd", null, h("span", { class: "keyid", text: visible(hello?.clientVersion || "-") })),
    h("dt", { text: "Mode" }), h("dd", { text: visible(hello?.mode || "-") }),
  );
  root.replaceChildren(
    titleRow("Settings", "Updates, and this client."),
    h("div", { class: "set" },
      h("div", { class: "lab" }, h("h3", { text: "Updates" }), h("p", { text: "Versions, Check now and automatic updates." })),
      h("div", { class: "panel pad" }, h("p", { class: "muted", text: "Not in this build yet." })),
    ),
    h("div", { class: "set" },
      h("div", { class: "lab" }, h("h3", { text: "Client" }), h("p", { text: "The ClawBridge client this page is served by." })),
      h("div", { class: "panel pad" }, info),
    ),
  );
};
