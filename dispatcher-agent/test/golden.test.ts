// The shared contract test: api/dispatcher-model/golden holds the server's answers as it sends them (written by the
// server's Go tests), and each must validate against src/schema.ts. A drift on either side fails CI on that side.
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { Value } from "typebox/value";
import type { TSchema } from "typebox";
import * as schema from "../src/schema.ts";

const DIR = join(import.meta.dirname, "..", "..", "api", "dispatcher-model", "golden");
const index = JSON.parse(readFileSync(join(DIR, "index.json"), "utf8")) as Record<string, string>;
const read = (file: string): unknown => JSON.parse(readFileSync(join(DIR, file), "utf8"));
const defs = schema.definitions();

test("every golden file is in index.json, and every indexed file exists", () => {
	const files = readdirSync(DIR).filter((f) => f.endsWith(".json") && f !== "index.json").sort();
	assert.deepEqual(files, Object.keys(index).sort());
});

test("every golden agent-API answer validates against its schema", () => {
	for (const [file, name] of Object.entries(index)) {
		// The launcher's answers are agentd's (Go), checked in internal/agentd.
		if (name === "agentd") continue;
		const [outer, result] = name.split(":") as [string, string | undefined];
		const s: TSchema | undefined = defs[outer];
		assert.ok(s, `${file}: no schema ${outer}`);
		const body = read(file);
		assert.ok(Value.Check(s, body), `${file} does not match ${outer}: ${JSON.stringify([...Value.Errors(s, body)].slice(0, 3))}`);
		if (result) {
			const r = defs[result];
			assert.ok(r, `${file}: no schema ${result}`);
			assert.ok(Value.Check(r, (body as { result: unknown }).result), `${file}'s result does not match ${result}`);
		}
	}
});

test("every error code has its golden answer", () => {
	for (const code of schema.ERROR_CODES) {
		if (code === "guardrail" || code === "conflict") continue; // not sent by the case API
		assert.equal(index[`error-${code}.json`], "ErrorResponse", code);
	}
});

test("a ClawBridge project ID is prj_ and 32 hex digits, never a UUID [#106 wire review]", () => {
	const renew = read("agent-renew.json") as { projects: { project_id: string }[] };
	assert.ok(Value.Check(schema.AgentRenewResponse, renew));
	const uuid = { projects: [{ ...renew.projects[0], project_id: "0b9f6c5e-3a4d-4e8b-9c1f-2d3e4f5a6b7c" }] };
	assert.ok(!Value.Check(schema.AgentRenewResponse, uuid));
	const hello = read("hello.json") as Record<string, unknown>;
	assert.ok(!Value.Check(schema.HelloResponse, { ...hello, project_id: "prj_short" }));
});
