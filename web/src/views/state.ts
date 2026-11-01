// What the sheet shows instead of a screen while the page has no working
// connection to `clawbridge ui` (GUI-D-SPEC §7, GUI-DESIGN §12.3).
import { h, rich } from "../dom";
import { codebox } from "./common";

export type PanelKind = "connecting" | "no-secret" | "unauthorized" | "lost" | "update-needed";

function panel(...kids: (Node | string | null)[]): HTMLElement {
  return h("div", { class: "state-panel" }, h("div", null, ...kids));
}

export function statePanel(kind: PanelKind): HTMLElement {
  switch (kind) {
    case "connecting":
      return h("div", { class: "state-panel", "aria-busy": "true" }, h("div", null, h("h1", { text: "Connecting to ClawBridge" })));
    case "no-secret":
    case "unauthorized":
      // The same state, so a missing and a refused secret look alike.
      return panel(
        h("h1", null, rich("Open this from `clawbridge ui`")),
        h("p", null, rich("This tab isn't connected. Run `clawbridge ui` in your terminal to open ClawBridge.")),
        codebox("clawbridge ui"),
      );
    case "lost":
      return panel(
        h("h1", { text: "Connection lost" }),
        h("p", null, rich("ClawBridge UI stopped (it stops a minute after its last tab closes). Run `clawbridge ui` again.")),
        codebox("clawbridge ui"),
      );
    case "update-needed":
      return panel(
        h("h1", { text: "Update ClawBridge" }),
        h("p", { text: "Reload this tab." }),
        h("p", { text: "If it still shows this, this app needs a newer ClawBridge client. In your terminal, run:" }),
        codebox("clawbridge self-update"),
        h("p", null, rich("Then run `clawbridge ui` again.")),
      );
  }
}
