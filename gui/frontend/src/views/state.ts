// What the sheet shows instead of a screen while the backend is not
// running (GUI-D-SPEC §7): loading, backend stopped, setup needed, update
// needed. A restart never replays anything: it starts a fresh client.
import type { Api, BackendStatus } from "../api";
import { h, visible } from "../dom";
import { codebox } from "./common";

/** The one-line installer from docs/installation.md. */
export const INSTALL_COMMAND =
  "curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.sh | sh";

const isWindows = (s: BackendStatus) => s.os === "windows";

/** Where the person runs commands for this client. */
export function whereWord(s: BackendStatus): string {
  return s.distro ? `${s.distro} (WSL)` : isWindows(s) ? "Windows" : "your terminal";
}

function panel(...kids: (Node | string | null)[]): HTMLElement {
  return h("div", { class: "state-panel" }, h("div", null, ...kids));
}

function run(api: Api, p: Promise<unknown>, note: HTMLElement): void {
  p.then(
    () => { note.textContent = ""; },
    (e: unknown) => { note.textContent = visible(e instanceof Error ? e.message : String(e)); },
  );
  void api;
}

/** Picker for the WSL distro (Windows) and Choose client (everywhere else). */
export function clientPicker(api: Api, status: BackendStatus, onDone?: () => void): HTMLElement {
  const box = h("div", { class: "picker" });
  const note = h("p", { class: "reason", role: "status" });
  const wrap = h("div", { style: "display:grid;gap:8px;justify-items:center;width:100%" }, box, note);
  if (isWindows(status)) {
    const select = h("select", { class: "input", "aria-label": "WSL distro" });
    select.append(h("option", { value: "", text: "Loading distros…" }));
    const use = h("button", { class: "btn primary", type: "button", text: "Use this distro" });
    const refresh = h("button", { class: "btn", type: "button", text: "Refresh list" });
    const fill = () => {
      api.wslDistros().then((names) => {
        select.replaceChildren();
        if (names.length === 0) select.append(h("option", { value: "", text: "No WSL distro found" }));
        for (const n of names) {
          const o = h("option", { value: n, text: visible(n) });
          if (n === status.distro) o.selected = true;
          select.append(o);
        }
        use.disabled = names.length === 0;
      }, (e: unknown) => {
        select.replaceChildren(h("option", { value: "", text: "wsl.exe is not available" }));
        use.disabled = true;
        note.textContent = visible(e instanceof Error ? e.message : String(e));
      });
    };
    use.addEventListener("click", () => {
      run(api, api.setWslDistro(select.value).then(() => onDone?.()), note);
    });
    refresh.addEventListener("click", fill);
    box.append(select, use, refresh);
    fill();
  }
  const choose = h("button", { class: "btn", type: "button", text: isWindows(status) ? "Choose clawbridge.exe…" : "Choose the clawbridge file…" });
  choose.addEventListener("click", () => run(api, api.chooseClient().then(() => onDone?.()), note));
  box.append(choose);
  return wrap;
}

export function statePanel(api: Api, s: BackendStatus): HTMLElement {
  switch (s.state) {
    case "starting":
      return h("div", { class: "state-panel", "aria-busy": "true" },
        h("div", null, h("h1", { text: "Starting the ClawBridge client" }), h("p", { text: "This takes a moment. In WSL it can take longer the first time." })));
    case "needs-setup":
      return panel(
        h("h1", { text: isWindows(s) ? "Choose where ClawBridge lives" : "ClawBridge client not found" }),
        h("p", { text: visible(s.reason ?? "") }),
        h("p", { text: isWindows(s) ? "ClawBridge usually lives inside WSL. In the distro, install it with:" : "To install it, run in your terminal:" }),
        codebox(INSTALL_COMMAND),
        h("p", { text: "The app never installs the client for you." }),
        clientPicker(api, s),
      );
    case "update-needed":
      return panel(
        h("h1", { text: "Update ClawBridge" }),
        h("p", { text: `This app needs a newer ClawBridge client${s.client_version ? `; the client in ${whereWord(s)} is ${visible(s.client_version)}` : ""}.` }),
        h("p", { text: `In ${whereWord(s)}, run:` }),
        codebox("clawbridge self-update"),
        s.reason ? h("p", { class: "reason", text: visible(s.reason) }) : null,
        h("button", { class: "btn", type: "button", on: { click: () => void api.restart() }, text: "Restart the client" }),
      );
    case "stopped":
    default: {
      const where = s.distro ? ` in ${whereWord(s)}` : "";
      return panel(
        h("h1", { text: `The ClawBridge client stopped${where}.` }),
        s.reason && s.reason !== "The ClawBridge client stopped." ? h("p", { class: "reason", text: visible(s.reason) }) : null,
        h("p", { text: "Restarting starts a fresh client. Nothing that was in flight is sent again." }),
        h("button", { class: "btn primary", type: "button", on: { click: () => void api.restart() }, text: "Restart" }),
      );
    }
  }
}
