// Supply-chain checks on package-lock.json (design §4.3, §4.4, AG-L2):
// 1. pi comes only from the fork's release tarballs, each with an integrity hash;
// 2. every registry package was published at least 14 days before now;
// 3. every production package carries an allowed licence.
import { readFileSync } from "node:fs";

const MIN_DAYS = Number(process.env.MIN_RELEASE_AGE_DAYS ?? 14);
const FORK_RELEASE = /^https:\/\/github\.com\/jkai-chen\/clawbridge-pi\/releases\/download\/cb-v[0-9.]+-\d+\/[a-z0-9.-]+\.tgz$/;
const LICENCES = new Set(["MIT", "Apache-2.0", "BSD-3-Clause", "Unlicense", "0BSD"]);

interface Entry {
	version?: string;
	resolved?: string;
	integrity?: string;
	license?: string;
	dev?: boolean;
	link?: boolean;
}
const lock = JSON.parse(readFileSync(new URL("../package-lock.json", import.meta.url), "utf8")) as {
	packages: Record<string, Entry>;
};
const errors: string[] = [];
const registry = new Map<string, { name: string; version: string }>();
let prod = 0;
for (const [path, entry] of Object.entries(lock.packages)) {
	if (!path.includes("node_modules/") || entry.link) continue;
	const name = path.slice(path.lastIndexOf("node_modules/") + "node_modules/".length);
	if (!entry.integrity) errors.push(`${name}: no integrity in the lockfile`);
	if (name.startsWith("@earendil-works/")) {
		if (!FORK_RELEASE.test(entry.resolved ?? "")) errors.push(`${name}: must resolve to a clawbridge-pi release tarball`);
	} else if (entry.resolved?.startsWith("https://registry.npmjs.org/") && entry.version) {
		registry.set(`${name}@${entry.version}`, { name, version: entry.version });
	} else {
		errors.push(`${name}: resolved from neither the npm registry nor a fork release`);
	}
	if (!entry.dev) {
		prod++;
		if (!LICENCES.has(entry.license ?? "")) errors.push(`${name}: licence ${entry.license ?? "unknown"} is not allowed`);
	}
}

const cutoff = Date.now() - MIN_DAYS * 86_400_000;
const queue = [...registry.values()];
async function worker() {
	for (let item = queue.shift(); item; item = queue.shift()) {
		const res = await fetch(`https://registry.npmjs.org/${item.name.replace("/", "%2f")}`, {
			headers: { accept: "application/json" },
		});
		if (!res.ok) {
			errors.push(`${item.name}@${item.version}: registry ${res.status}`);
			continue;
		}
		const published = ((await res.json()) as { time?: Record<string, string> }).time?.[item.version];
		if (!published) errors.push(`${item.name}@${item.version}: no publish time`);
		else if (Date.parse(published) > cutoff) {
			errors.push(`${item.name}@${item.version}: published ${published}, younger than ${MIN_DAYS} days`);
		}
	}
}
await Promise.all(Array.from({ length: 8 }, worker));
if (errors.length) {
	console.error(errors.sort().join("\n"));
	process.exit(1);
}
console.log(
	`lock check passed: ${registry.size} registry packages at least ${MIN_DAYS} days old, pi from the fork release, ${prod} production licences allowed`,
);
