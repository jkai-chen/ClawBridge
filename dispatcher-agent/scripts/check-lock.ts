// Supply-chain checks on package-lock.json (design §4.3, §4.4, AG-L2):
// 1. pi comes only from the pinned fork release: its tag, the commit the tag must still point at, and each tarball's
//    version and integrity (G9-11). Moving to a new fork release is a change to this file, reviewed as such;
// 2. every registry package was published at least 14 days before now;
// 3. every production package carries an allowed licence.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

const MIN_DAYS = Number(process.env.MIN_RELEASE_AGE_DAYS ?? 14);
const FORK = {
	repo: "https://github.com/jkai-chen/clawbridge-pi",
	tag: "cb-v1.1.0-1",
	commit: "96f054afedb82c66d11d68773f277c58b38810e1",
	tarballs: {
		"@earendil-works/pi-agent-core": {
			version: "1.1.0",
			integrity: "sha512-QDPJR9ryExe78Le54v51/NzTKvMVNgDFQvpX4G5IqTfC7hxu7DJz5OfchgN+jDCfMxpF5LxwM1ojgS/KDR+1Lg==",
		},
		"@earendil-works/pi-ai": {
			version: "1.1.0",
			integrity: "sha512-tkrIA7oqUul02zZzz6+vII5Z8K+WfOxzK6IxQ9NSFh75T8Z9zGxPh5rJcmNRfcQEy0cICmLvaUcIJzTNZtZo2Q==",
		},
	} as Record<string, { version: string; integrity: string }>,
};
/** The release asset URL of a pinned tarball: @scope/name@version -> scope-name-version.tgz. */
const forkUrl = (name: string, version: string) =>
	`${FORK.repo}/releases/download/${FORK.tag}/${name.slice(1).replace("/", "-")}-${version}.tgz`;
const LICENCES = new Set(["MIT", "Apache-2.0", "BSD-3-Clause", "Unlicense", "0BSD"]);

interface Entry {
	version?: string;
	resolved?: string;
	integrity?: string;
	license?: string;
	dev?: boolean;
	link?: boolean;
	dependencies?: Record<string, string>;
}
const lock = JSON.parse(readFileSync(new URL("../package-lock.json", import.meta.url), "utf8")) as {
	packages: Record<string, Entry>;
};
const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")) as {
	dependencies?: Record<string, string>;
};
const errors: string[] = [];
const forkSeen = new Set<string>();
const registry = new Map<string, { name: string; version: string }>();
let prod = 0;
for (const [path, entry] of Object.entries(lock.packages)) {
	if (!path.includes("node_modules/") || entry.link) continue;
	const name = path.slice(path.lastIndexOf("node_modules/") + "node_modules/".length);
	if (!entry.integrity) errors.push(`${name}: no integrity in the lockfile`);
	if (name.startsWith("@earendil-works/")) {
		const pin = FORK.tarballs[name];
		if (!pin) errors.push(`${name}: not a pinned clawbridge-pi tarball`);
		else if (entry.version !== pin.version || entry.resolved !== forkUrl(name, pin.version) || entry.integrity !== pin.integrity) {
			errors.push(`${name}: must be the pinned ${FORK.tag} tarball, version and integrity`);
		} else forkSeen.add(name);
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

for (const [name, pin] of Object.entries(FORK.tarballs)) {
	if (!forkSeen.has(name)) errors.push(`${name}: the pinned ${FORK.tag} tarball is missing from the lockfile`);
	if (pkg.dependencies?.[name] !== forkUrl(name, pin.version)) errors.push(`${name}: package.json must name the pinned tarball`);
	if (lock.packages[""]?.dependencies?.[name] !== forkUrl(name, pin.version)) {
		errors.push(`${name}: the lockfile's root must name the pinned tarball`);
	}
}
// The tag must still point at the pinned commit: a moved tag is a different release, even with the same tarballs.
try {
	const peeled = execFileSync("git", ["ls-remote", FORK.repo, `refs/tags/${FORK.tag}^{}`], { encoding: "utf8", timeout: 30_000 });
	const commit = peeled.split(/\s/)[0];
	if (commit !== FORK.commit) errors.push(`${FORK.tag}: points at ${commit || "nothing"}, not the pinned ${FORK.commit}`);
} catch {
	errors.push(`${FORK.tag}: could not be read from ${FORK.repo}`);
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
	`lock check passed: ${registry.size} registry packages at least ${MIN_DAYS} days old, pi from ${FORK.tag} at ${FORK.commit.slice(0, 7)}, ${prod} production licences allowed`,
);
