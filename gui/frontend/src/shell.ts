// The framed-sheet shell (GUI-D-SPEC §4): a tinted L-frame (sidebar and top
// bar) around one white sheet that holds the current screen.
import { Api, INITIAL_STATUS, type BackendStatus } from "./api";
import { h, setChildren } from "./dom";
import { icon, mark } from "./icons";
import { go, parseRoute, type RouteId } from "./router";
import { doctorView } from "./views/doctor";
import { logsView } from "./views/logs";
import { placeholder } from "./views/placeholder";
import { sessionsView } from "./views/sessions";
import { settingsView } from "./views/settings";
import { statePanel, whereWord } from "./views/state";
import type { View } from "./views/common";

interface NavItem { id: RouteId; label: string; icon: string }

const NAV: NavItem[] = [
  { id: "overview", label: "Overview", icon: "overview" },
  { id: "needs-you", label: "Needs you", icon: "needs" },
  { id: "sessions", label: "Sessions", icon: "sessions" },
  { id: "projects", label: "Projects", icon: "projects" },
  { id: "teams", label: "Teams & People", icon: "teams" },
  { id: "linear", label: "Linear", icon: "linear" },
  { id: "dispatcher", label: "Dispatcher", icon: "dispatcher" },
  { id: "account", label: "Account", icon: "account" },
  { id: "doctor", label: "Doctor", icon: "doctor" },
];
const SETTINGS: NavItem = { id: "settings", label: "Settings", icon: "settings" };

const VIEWS: Record<RouteId, View> = {
  overview: placeholder({ title: "Overview", question: "Which agents need me now?" }),
  "needs-you": placeholder({ title: "Needs you", question: "What is waiting on me, and what do I do about it?" }),
  sessions: sessionsView,
  projects: placeholder({ title: "Projects", question: "Which projects am I in and how are they bound?" }),
  teams: placeholder({ title: "Teams & People", question: "Who works on each bound team, and what may each person and their agents do?" }),
  linear: placeholder({ title: "Linear", question: "Is the board connected and are agent writes flowing?" }),
  dispatcher: placeholder({ title: "Dispatcher", question: "Is routing working, and is model mode safe to use?" }),
  account: placeholder({ title: "Account", question: "Which devices and invites does this account have?" }),
  doctor: doctorView,
  settings: settingsView,
  logs: logsView,
};

/** Screens that work while the client is down, because they are how to fix it. */
const ALWAYS: ReadonlySet<RouteId> = new Set(["settings", "logs"]);

export function connectionText(s: BackendStatus): { dot: string; text: string } {
  switch (s.state) {
    case "running": {
      const v = s.client_version ? ` · client v${s.client_version}` : "";
      return { dot: "dot", text: s.distro ? `${whereWord(s)}${v}` : `Connected${v}` };
    }
    case "starting": return { dot: "dot grey", text: "Connecting…" };
    case "needs-setup": return { dot: "dot off", text: "Setup needed" };
    case "update-needed": return { dot: "dot off", text: "Update needed" };
    default: return { dot: "dot stop", text: "Client stopped" };
  }
}

export function mountApp(host: HTMLElement, api: Api): void {
  let status: BackendStatus = INITIAL_STATUS;
  let generation = 0;
  const badges = new Map<string, number>();

  const sidebar = h("aside", { class: "sidebar" });
  const nav = h("nav", { class: "nav", "aria-label": "Main" });
  const bottomNav = h("nav", { class: "nav", "aria-label": "Settings" });
  const connDot = h("span", { class: "dot" });
  const connText = h("span", { text: "Connecting…" });
  const view = h("div", { class: "canvas", id: "view" });
  const sheet = h("main", { class: "sheet" }, view);

  const search = h("div", { class: "search", "aria-disabled": "true" },
    icon("search"),
    h("input", { type: "search", placeholder: "Search sessions, issues, messages", "aria-label": "Search (not available yet)", disabled: true }),
    h("kbd", { text: "⌘K" }),
  );
  const topbar = h("header", { class: "topbar" }, search, h("div", { class: "conn", role: "status" }, connDot, connText));

  const link = (item: NavItem): HTMLAnchorElement => {
    const a = h("a", { class: "nav-item", href: `#/${item.id}`, "data-id": item.id },
      icon(item.icon), h("span", { text: item.label }), h("span", { class: "count", hidden: true }));
    return a;
  };
  for (const item of NAV) nav.append(link(item));
  bottomNav.append(link(SETTINGS));
  sidebar.append(
    h("div", { class: "brand" }, mark(), h("span", { text: "ClawBridge" })),
    h("div", { class: "ws" }, h("div", { class: "ws-btn" }, h("div", { class: "t" }, h("b", { text: "ClawBridge" }), h("span", { id: "ws-sub", text: "" })))),
    nav,
    h("div", { class: "bottom" }, bottomNav),
  );

  setChildren(host, h("div", { class: "app" }, sidebar, topbar, sheet),
    h("div", { class: "narrow-note" }, h("div", null, mark(28), h("p", { class: "t", text: "ClawBridge needs a wider window" }), h("p", { text: "Make the window at least 1024 px wide." }))));

  const setBadge = (id: string, n: number) => {
    badges.set(id, n);
    const el = sidebar.querySelector(`a[data-id="${id}"] .count`);
    if (el instanceof HTMLElement) {
      el.hidden = n <= 0;
      el.textContent = String(n);
    }
  };

  const render = () => {
    const route = parseRoute(location.hash);
    generation += 1;
    const mine = generation;
    for (const a of sidebar.querySelectorAll("a.nav-item")) {
      if (a.getAttribute("data-id") === route) a.setAttribute("aria-current", "page");
      else a.removeAttribute("aria-current");
    }
    const c = connectionText(status);
    connDot.className = c.dot;
    connText.textContent = c.text;
    const sub = sidebar.querySelector("#ws-sub");
    if (sub) sub.textContent = status.state === "running" ? (status.target ?? "") : "";
    if (status.state !== "running" && !ALWAYS.has(route)) {
      view.replaceChildren(statePanel(api, status));
      return;
    }
    view.replaceChildren();
    VIEWS[route]({
      root: view, api, status,
      isCurrent: () => mine === generation,
      setBadge,
      go: (p) => go(p as RouteId),
    });
  };

  window.addEventListener("hashchange", render);
  api.onState((s) => {
    const was = status.state;
    status = s;
    // A change of state, not each repeat of it, redraws the sheet.
    if (was !== s.state || s.state !== "running") render();
  });
  api.onOpenLogs(() => go("logs"));
  // The window may have been created after the first state event.
  api.status().then((s) => { status = s; render(); }, () => render());
  render();
}
