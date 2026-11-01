import { createHash } from "node:crypto";
import { readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { type Plugin } from "vite";
import { defineConfig } from "vitest/config";

// The bundle goes to internal/webui/dist/app/, which git ignores: the
// hand-written "UI not built" page at internal/webui/dist/index.html stays
// committed and is what a plain `go build` embeds. internal/webui serves
// dist/app when it has an index.html, and the placeholder otherwise.
const OUT = fileURLToPath(new URL("../internal/webui/dist/app", import.meta.url));
export const MANIFEST = "manifest.sha256";

function files(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f);
    return statSync(p).isDirectory() ? files(p) : [p];
  });
}

/**
 * Writes manifest.sha256 beside the bundle: one "<sha256>  <path>" line per
 * file, sorted by path (bytes), manifest excluded. The server's bundle id is
 * the sha256 of this same text, computed from the embedded files
 * (internal/webui/bundle.go), so a release can compare the two (R1-T8).
 */
function bundleManifest(): Plugin {
  return {
    name: "bundle-manifest",
    apply: "build",
    closeBundle() {
      const rows = files(OUT)
        .map((p) => relative(OUT, p).split(sep).join("/"))
        .filter((p) => p !== MANIFEST)
        .sort((a, b) => (Buffer.from(a) < Buffer.from(b) ? -1 : 1))
        .map((p) => `${createHash("sha256").update(readFileSync(join(OUT, p))).digest("hex")}  ${p}\n`);
      writeFileSync(join(OUT, MANIFEST), rows.join(""));
    },
  };
}

export default defineConfig({
  base: "/",
  plugins: [bundleManifest()],
  build: {
    outDir: OUT,
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
