import { visible, h } from "../dom";
import { titleRow, type View } from "./common";
import { clientPicker, whereWord } from "./state";

/** Settings opens to Updates first; those arrive later. The client's location is real. */
export const settingsView: View = ({ root, api, status }) => {
  const info = h("dl", { class: "kv" },
    h("dt", { text: "Runs in" }), h("dd", { text: visible(status.target ? whereWord({ ...status }) : "not started") }),
    h("dt", { text: "Client version" }), h("dd", null, h("span", { class: "keyid", text: visible(status.client_version ?? "-") })),
    h("dt", { text: "State" }), h("dd", { text: status.state }),
  );
  root.replaceChildren(
    titleRow("Settings", "Updates, and where the ClawBridge client runs."),
    h("div", { class: "set" },
      h("div", { class: "lab" }, h("h3", { text: "Updates" }), h("p", { text: "Versions, Check now and automatic updates." })),
      h("div", { class: "panel pad" }, h("p", { class: "muted", text: "Not in this build yet." })),
    ),
    h("div", { class: "set" },
      h("div", { class: "lab" }, h("h3", { text: "Client" }), h("p", { text: "The app starts `clawbridge gui-backend --stdio` and stores only this choice." })),
      h("div", { class: "panel pad" }, info,
        h("div", { class: "section" }, clientPicker(api, status)),
        h("div", { class: "section" }, h("button", { class: "btn", type: "button", on: { click: () => void api.restart() }, text: "Restart the client" })),
      ),
    ),
  );
};
