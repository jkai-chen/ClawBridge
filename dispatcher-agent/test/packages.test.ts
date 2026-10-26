// The fork's strip-down holds in what is actually installed (design §8).
import assert from "node:assert/strict";
import { existsSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const nm = fileURLToPath(new URL("../node_modules", import.meta.url));

test("only pi-ai and pi-agent-core are installed from pi", () => {
	assert.deepEqual(readdirSync(join(nm, "@earendil-works")).sort(), ["pi-agent-core", "pi-ai"]);
});

test("removed fork paths are absent from the installed packages", () => {
	const absent = [
		"pi-ai/dist/cli.js", "pi-ai/dist/oauth.js", "pi-ai/dist/auth/oauth", "pi-agent-core/dist/proxy.js",
		"pi-ai/dist/providers/github-copilot.js", "pi-ai/dist/providers/openai-codex.js", "pi-ai/dist/providers/radius.js",
		"pi-ai/dist/providers/typesafe.js", "pi-ai/dist/api/pi-messages.js", "pi-ai/dist/api/openai-codex-responses.js",
	];
	for (const p of absent) assert.ok(!existsSync(join(nm, "@earendil-works", p)), p);
	for (const pkg of ["pi-coding-agent", "pi-tui", "pi-telemetry", "pi-mcp"]) {
		assert.ok(!existsSync(join(nm, "@earendil-works", pkg)), pkg);
	}
});
