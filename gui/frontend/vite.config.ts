import { type Plugin } from "vite";
import { defineConfig } from "vitest/config";

// The page's Content-Security-Policy lives in index.html as a <meta> tag.
// The dev server's hot reload needs inline scripts and a websocket, so the
// tag is dropped there; every build keeps it.
function devWithoutCsp(): Plugin {
  return {
    name: "dev-without-csp",
    apply: "serve",
    transformIndexHtml: (html) => html.replace(/<meta http-equiv="Content-Security-Policy"[^>]*>/, ""),
  };
}

export default defineConfig({
  base: "./",
  plugins: [devWithoutCsp()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // Fonts and the icon mask stay files: the CSP has no data: source.
    assetsInlineLimit: 0,
    // No inline <script>, no module-preload polyfill that injects one.
    modulePreload: { polyfill: false },
    cssCodeSplit: false,
    sourcemap: false,
  },
  test: {
    environment: "jsdom",
    include: ["test/**/*.test.ts"],
  },
});
