// The framed-sheet shell (GUI-D-SPEC §4): a tinted L-frame (sidebar and top
// bar) around one white sheet that holds the current screen.
import { Api, PROTOCOL, type Hello } from "./api";
import { h, setChildren, visible } from "./dom";
import { icon, mark } from "./icons";
import { go, parseRoute, type RouteId } from "./router";
import { ConnectionError, type ConnState } from "./transport";
import { accountView } from "./views/account";
import { dispatcherView } from "./views/dispatcher";
import { doctorView } from "./views/doctor";
import { linearView } from "./views/linear";
import { projectsView } from "./views/projects";
import { teamsView } from "./views/teams";
import { overviewView } from "./views/overview";
import { sessionsView } from "./views/sessions";
import { settingsView } from "./views/settings";
import { needsView } from "./views/needs";
import { createNeedsSignal } from "./needsSignal";
import { parseNeeds } from "./needs";
import { statePanel, type PanelKind } from "./views/state";
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
  overview: overviewView,
  "needs-you": needsView,
  sessions: sessionsView,
  projects: projectsView,
  teams: teamsView,
  linear: linearView,
  dispatcher: dispatcherView,
  account: accountView,
  doctor: doctorView,
  settings: settingsView,
};

export interface Shown {
  dot: string;
  text: string;
}

/** The top bar's connection area. `offlineAt` is the time of the copy being shown. */
export function connectionText(conn: ConnState, hello: Hello | null, offlineAt: string | null): Shown {
  switch (conn) {
    case "ok": {
      if (offlineAt !== null) return { dot: "dot off", text: offlineAt ? `Offline, showing the copy from ${offlineAt}` : "Offline, showing a saved copy" };
      const v = hello?.clientVersion ? ` · client v${visible(hello.clientVersion)}` : "";
      return { dot: "dot", text: `${hello?.platform === "wsl" ? (hello.distro ? `${visible(hello.distro)} (WSL)` : "WSL") : "Connected"}${v}` };
    }
    case "connecting": return { dot: "dot grey", text: "Connecting…" };
    case "lost": return { dot: "dot stop", text: "Connection lost" };
    default: return { dot: "dot off", text: "Not connected" };
  }
}

/** The first letter of the person's name or address, upper-cased. */
export function initial(principal: string): string {
  const first = Array.from(principal.trim())[0] ?? "";
  return first.toUpperCase();
}

export function mountApp(host: HTMLElement, api: Api): void {
  let conn: ConnState = "connecting";
  let hello: Hello | null = null;
  let offlineAt: string | null = null;
  let generation = 0;
  let loadingHello = false;
  let outboxWaiting: number | null = null;
  const outboxListeners = new Set<(n: number) => void>();
  const outbox = {
    get: () => outboxWaiting,
    subscribe: (fn: (n: number) => void) => {
      outboxListeners.add(fn);
      return () => { outboxListeners.delete(fn); };
    },
  };

  const needsSignal = createNeedsSignal();
  const sidebar = h("aside", { class: "sidebar" });
  const nav = h("nav", { class: "nav", "aria-label": "Main" });
  const bottomNav = h("nav", { class: "nav", "aria-label": "Settings" });
  const connDot = h("span", { class: "dot" });
  const connText = h("span", { text: "Connecting…" });
  const who = h("span", { class: "avatar me", hidden: true, "aria-hidden": "true" });
  const wsName = h("b", { text: "ClawBridge" });
  const wsSub = h("span", { text: "" });
  const device = h("div", { class: "dev", hidden: true });
  const view = h("div", { class: "canvas", id: "view" });
  const sheet = h("main", { class: "sheet" }, view);

  const search = h("div", { class: "search", "aria-disabled": "true" },
    icon("search"),
    h("input", { type: "search", placeholder: "Search sessions, issues, messages", "aria-label": "Search (not available yet)", disabled: true }),
    h("kbd", { text: "⌘K" }),
  );
  const topbar = h("header", { class: "topbar" }, search, h("div", { class: "conn", role: "status" }, connDot, connText), who);

  const link = (item: NavItem): HTMLAnchorElement =>
    h("a", { class: "nav-item", href: `#/${item.id}`, "data-id": item.id },
      icon(item.icon), h("span", { text: item.label }), h("span", { class: "count", hidden: true }));
  for (const item of NAV) nav.append(link(item));
  bottomNav.append(link(SETTINGS));
  sidebar.append(
    h("div", { class: "brand" }, mark(), h("span", { text: "ClawBridge" })),
    h("div", { class: "ws" }, h("div", { class: "ws-btn" }, h("div", { class: "t" }, wsName, wsSub))),
    nav,
    h("div", { class: "bottom" }, device, bottomNav),
  );

  setChildren(host, h("div", { class: "app" }, sidebar, topbar, sheet),
    h("div", { class: "narrow-note" }, h("div", null, mark(28), h("p", { class: "t", text: "ClawBridge needs a wider window" }), h("p", { text: "Make the window at least 1024 px wide." }))));

  const setBadge = (id: string, n: number) => {
    const el = sidebar.querySelector(`a[data-id="${id}"] .count`);
    if (el instanceof HTMLElement) {
      el.hidden = n <= 0;
      el.textContent = String(n);
    }
  };

  const panelKind = (): PanelKind | null => {
    if (conn === "connecting") return "connecting";
    if (conn === "no-secret" || conn === "unauthorized" || conn === "lost") return conn;
    if (hello && hello.protocol !== PROTOCOL) return "update-needed";
    if (!hello) return "connecting";
    return null;
  };

  const chrome = () => {
    const c = connectionText(conn, hello, offlineAt);
    connDot.className = c.dot;
    connText.textContent = c.text;
    wsName.textContent = visible(hello?.workspace || "ClawBridge");
    wsSub.textContent = visible(hello?.project || "");
    const who1 = hello ? initial(hello.principal) : "";
    who.hidden = !who1;
    who.textContent = visible(who1);
    device.hidden = !hello?.device;
    device.replaceChildren(...(hello?.device ? ["This device: ", h("span", { text: visible(hello.device) })] : []));
  };

  const render = () => {
    const route = parseRoute(location.hash);
    generation += 1;
    const mine = generation;
    for (const a of sidebar.querySelectorAll("a.nav-item")) {
      if (a.getAttribute("data-id") === route) a.setAttribute("aria-current", "page");
      else a.removeAttribute("aria-current");
    }
    const kind = panelKind();
    if (kind || conn !== "ok") offlineAt = null;
    chrome();
    if (kind) {
      view.replaceChildren(statePanel(kind));
      return;
    }
    view.replaceChildren();
    VIEWS[route]({
      root: view, api, hello,
      isCurrent: () => mine === generation,
      setBadge, outbox, needsChanged: needsSignal.subscribe,
      go: (p) => go(p as RouteId),
      connection: (k) => { conn = k; render(); },
      setOffline: (at) => {
        if ((at === null) !== (offlineAt === null) || at !== offlineAt) {
          offlineAt = at;
          chrome();
        }
      },
    });
  };

  const loadHello = () => {
    if (loadingHello) return;
    loadingHello = true;
    api.hello().then(
      (x) => { hello = x; refreshNeedsBadge(); },
      (e: unknown) => { hello = null; conn = e instanceof ConnectionError ? e.kind : "lost"; },
    ).finally(() => { loadingHello = false; render(); });
  };

  // The sidebar count is the queue's length; a failed read leaves it as it was.
  const refreshNeedsBadge = () => {
    api.rpc("needs.list").then((raw) => setBadge("needs-you", parseNeeds(raw).items.length), () => {});
  };

  window.addEventListener("hashchange", render);
  render();
  api.events((e) => {
    if (e.event === "needs.changed") {
      // The payload is only a nudge; the queue itself is read again.
      refreshNeedsBadge();
      needsSignal.fire();
      return;
    }
    if (e.event !== "outbox.count") return;
    // Anything but a count is ignored: the page trusts the shape, not the sender.
    let n: unknown;
    try { n = (JSON.parse(e.data) as { waiting?: unknown }).waiting; } catch { return; }
    if (typeof n !== "number" || !Number.isFinite(n) || n < 0) return;
    outboxWaiting = Math.floor(n);
    for (const fn of outboxListeners) fn(outboxWaiting);
  }, (s) => {
    const was = conn;
    conn = s;
    if (s === "ok" && was !== "ok") loadHello();
    else if (was !== s) render();
  });
}
