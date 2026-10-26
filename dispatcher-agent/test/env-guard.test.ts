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
