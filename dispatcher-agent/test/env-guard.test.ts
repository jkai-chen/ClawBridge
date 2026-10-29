import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { forbiddenEnv, PI_ENV_VARS } from "../src/env-guard.ts";

const root = fileURLToPath(new URL("..", import.meta.url));
const piAi = join(root, "node_modules/@earendil-works/pi-ai/dist");

function* jsFiles(dir: string): Generator<string> {
	for (const name of readdirSync(dir)) {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) yield* jsFiles(p);
		else if (name.endsWith(".js")) yield p;
	}
}

test("the list covers every variable the pinned pi-ai reads", () => {
	const read = new Set<string>();
	for (const m of readFileSync(join(piAi, "env-api-keys.js"), "utf8").matchAll(/"([A-Z][A-Z0-9_]{2,})"/g)) read.add(m[1] as string);
	for (const f of jsFiles(piAi)) {
		const text = readFileSync(f, "utf8");
		for (const m of text.matchAll(/(?:getProviderEnvValue|getProxyEnv)\("([A-Za-z0-9_]+)"/g)) read.add(m[1] as string);
		for (const m of text.matchAll(/process\.env\.([A-Z][A-Z0-9_]+)/g)) read.add(m[1] as string);
	}
	assert.ok(read.size > 40, "the scan found pi-ai's variables");
	const listed = new Set(PI_ENV_VARS.map((n) => n.toUpperCase()));
	const missing = [...read].filter((n) => !listed.has(n.toUpperCase())).sort();
	assert.deepEqual(missing, [], "add these to src/env-guard.ts");
});

test("forbiddenEnv names each set variable, ignores empty ones", () => {
	for (const name of PI_ENV_VARS) assert.deepEqual(forbiddenEnv({ [name]: "x" }), [name]);
	assert.deepEqual(forbiddenEnv({ OPENAI_API_KEY: "" }), []);
	assert.deepEqual(forbiddenEnv({ PATH: "/bin", CLAWBRIDGE_SERVER_URL: "https://cb.example" }), []);
});

test("the agent refuses to start with a provider key in env, and never prints its value", () => {
	const decoy = "fake-decoy-value-must-not-print-0123456789";
	const r = spawnSync(process.execPath, [join(root, "src/main.ts")], {
		env: { PATH: process.env.PATH, OPENAI_API_KEY: decoy, CLAWBRIDGE_SERVER_URL: "https://cb.example" },
		input: "",
		encoding: "utf8",
		timeout: 20_000,
	});
	assert.equal(r.status, 2);
	assert.match(r.stderr, /refusing to start: .*OPENAI_API_KEY/);
	assert.ok(!(r.stdout + r.stderr).includes(decoy.slice(0, 8)));
});

test("Node's TLS, CA and preload variables are refused too", () => {
	for (const [name, value] of [
		["NODE_TLS_REJECT_UNAUTHORIZED", "0"],
		["NODE_EXTRA_CA_CERTS", "/tmp/evil-ca.pem"],
		["NODE_OPTIONS", "--import=/tmp/evil.mjs"],
	]) {
		assert.deepEqual(forbiddenEnv({ [name as string]: value }), [name]);
		const r = spawnSync(process.execPath, [join(root, "src/main.ts")], {
			// NODE_OPTIONS is set to a harmless flag here, so the child still starts and the guard is what refuses it.
			env: { PATH: process.env.PATH, CLAWBRIDGE_SERVER_URL: "https://cb.example", [name as string]: name === "NODE_OPTIONS" ? "--no-warnings" : value },
			input: "",
			encoding: "utf8",
			timeout: 20_000,
		});
		assert.equal(r.status, 2, name);
		assert.match(r.stderr, new RegExp(`refusing to start: .*${name}`));
	}
});

test("the agent refuses a plain-http server URL other than localhost [A8]", () => {
	const r = spawnSync(process.execPath, [join(root, "src/main.ts")], {
		env: { PATH: process.env.PATH, CLAWBRIDGE_SERVER_URL: "http://cb.example" },
		input: "cba_abcdefghijklmnopqrstuvwxyz\n",
		encoding: "utf8",
		timeout: 20_000,
	});
	assert.equal(r.status, 2);
	assert.match(r.stderr, /must use https/);
});

test("the agent refuses to start without a reachable egress proxy, and says why", async () => {
	const { createServer } = await import("node:net");
	// A port that was free a moment ago: nothing listens on it.
	const closed = await new Promise<number>((resolve) => {
		const s = createServer().listen(0, "127.0.0.1", () => {
			const port = (s.address() as { port: number }).port;
			s.close(() => resolve(port));
		});
	});
	for (const [proxy, message] of [
		[undefined, /CLAWBRIDGE_EGRESS_PROXY must be set/],
		["", /CLAWBRIDGE_EGRESS_PROXY must be set/],
		["https://clawbridge-egress:3128", /must be http:\/\/HOST:PORT/],
		["http://clawbridge-egress", /must be http:\/\/HOST:PORT/],
		["http://user:pw@clawbridge-egress:3128", /must be http:\/\/HOST:PORT/],
		["http://clawbridge-egress:3128/path", /must be http:\/\/HOST:PORT/],
		[`http://127.0.0.1:${closed}`, /egress proxy CLAWBRIDGE_EGRESS_PROXY is unreachable/],
	] as const) {
		const env: Record<string, string | undefined> = { PATH: process.env.PATH, CLAWBRIDGE_SERVER_URL: "https://cb.example" };
		if (proxy !== undefined) env.CLAWBRIDGE_EGRESS_PROXY = proxy;
		const r = spawnSync(process.execPath, [join(root, "src/main.ts")], {
			env,
			input: "cba_abcdefghijklmnopqrstuvwxyz\n",
			encoding: "utf8",
			timeout: 20_000,
		});
		assert.equal(r.status, 2, String(proxy));
		assert.match(r.stderr, message);
	}
});
