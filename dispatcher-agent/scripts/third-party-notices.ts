// Writes THIRD-PARTY-NOTICES to stdout: every production package in package-lock.json, with its licence and the
// licence text it ships. Run in the image build after `npm ci --omit=dev` (design §4.4); a package without a licence
// file gets its SPDX identifier only, which check:lock has already limited to the allowed set.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const lock = JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8")) as {
	packages: Record<string, { version?: string; license?: string; dev?: boolean; link?: boolean }>;
};

const out: string[] = [
	"Third-party software in the ClawBridge dispatcher agent image.",
	"Generated from package-lock.json; each entry gives the package, its version, its licence and the licence text it ships.",
	"",
];
for (const [path, entry] of Object.entries(lock.packages).sort(([a], [b]) => a.localeCompare(b))) {
	if (!path.includes("node_modules/") || entry.dev || entry.link) continue;
	const name = path.slice(path.lastIndexOf("node_modules/") + "node_modules/".length);
	const dir = join(root, path);
	const files = existsSync(dir) ? readdirSync(dir).filter((f) => /^(licen[cs]e|copying|notice)(\.|$)/i.test(f)).sort() : [];
	out.push("=".repeat(78), `${name}@${entry.version ?? "?"} (${entry.license ?? "unknown"})`, "=".repeat(78));
	for (const f of files) out.push(readFileSync(join(dir, f), "utf8").trimEnd(), "");
	if (files.length === 0) out.push(`(no licence file shipped; SPDX: ${entry.license ?? "unknown"})`, "");
}
process.stdout.write(`${out.join("\n")}\n`);
