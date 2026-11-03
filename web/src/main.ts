// Vendored through npm and bundled: Vite copies the woff2 files into the
// build, so nothing loads from the network (CSP font-src 'self'). Latin subset only.
import "@fontsource/ibm-plex-sans/latin-400.css";
import "@fontsource/ibm-plex-sans/latin-500.css";
import "@fontsource/ibm-plex-sans/latin-600.css";
import "@fontsource/ibm-plex-mono/latin-400.css";
import "@fontsource/ibm-plex-mono/latin-500.css";
import "./styles.css";
import { Api } from "./api";
import { takeSecret } from "./session";
import { applyTheme } from "./theme";
import { mountApp } from "./shell";
import { httpTransport } from "./transport";

// The secret is taken before anything renders: the meta goes, the address is
// reset to "/" and the secret lives only in sessionStorage.
const secret = takeSecret();
applyTheme();
const host = document.getElementById("app");
if (host) mountApp(host, new Api(httpTransport({ secret })));
