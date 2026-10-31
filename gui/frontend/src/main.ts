// Vendored through npm and bundled: Vite copies the woff2 files into dist, so
// nothing loads from the network (CSP font-src 'self'). Latin subset only.
import "@fontsource/ibm-plex-sans/latin-400.css";
import "@fontsource/ibm-plex-sans/latin-500.css";
import "@fontsource/ibm-plex-sans/latin-600.css";
import "@fontsource/ibm-plex-mono/latin-400.css";
import "@fontsource/ibm-plex-mono/latin-500.css";
import "./styles.css";
import { Api } from "./api";
import { watchViolations } from "./violations";
import { applyTheme } from "./theme";
import { mountApp } from "./shell";
import { defaultTransport } from "./transport";

// Before anything else runs: nothing the webview blocks may go unseen.
const violations = watchViolations();

async function boot(): Promise<void> {
  applyTheme();
  const host = document.getElementById("app");
  if (!host) return;
  try {
    const api = new Api(await defaultTransport());
    violations.connect((text) => { void api.reportViolation(text).catch(() => {}); });
    mountApp(host, api);
    // First render done: tell the shell (the self-check waits for this).
    await api.status().catch(() => undefined);
    void api.ready(host.querySelector(".sidebar a.nav-item") !== null).catch(() => {});
  } catch (e) {
    host.textContent = `ClawBridge could not start its window bridge: ${e instanceof Error ? e.message : String(e)}`;
  }
}

void boot();
