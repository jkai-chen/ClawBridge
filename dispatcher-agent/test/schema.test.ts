import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { schemaDocument, SCHEMA_PATH } from "../scripts/write-schema.ts";
import { ToolRequests } from "../src/schema.ts";

test("api/dispatcher-model/v1.schema.json is generated from src/schema.ts (run npm run schema)", () => {
	assert.equal(readFileSync(SCHEMA_PATH, "utf8"), schemaDocument());
});

test("every operation names $defs that exist, and every d4 tool has an operation", () => {
	const doc = JSON.parse(schemaDocument());
	assert.equal(doc.$schema, "https://json-schema.org/draft/2020-12/schema");
	for (const [name, op] of Object.entries(doc["x-operations"]) as [string, { request?: string; response: string }][]) {
		for (const ref of [op.request, op.response].filter(Boolean) as string[]) {
			assert.ok(doc.$defs[ref.replace("#/$defs/", "")], `${name}: ${ref}`);
		}
	}
	for (const tool of Object.keys(ToolRequests)) assert.ok(doc["x-operations"][`tool_${tool}`], tool);
	assert.deepEqual(Object.keys(ToolRequests).sort(), [
		"abstain", "ask_session", "decide", "list_decisions", "list_domain", "list_sessions", "read_issue_facts",
		"recommend", "reply", "route_notice", "session_activity",
	]);
});

test("responses are open: an unknown field, top-level or nested, still validates (requests stay closed)", async () => {
	const { Value } = await import("typebox/value");
	const { AgentRenewResponse, CasesNextResponse, HelloResponse, ToolRequests } = await import("../src/schema.ts");
	const later = { x_future_field: 1 };
	const hello = {
		project_id: "prj_0b9f6c5e3a4d4e8b9c1f2d3e4f5a6b7c", enabled: true, provider: "openai", model: "m",
		caps: { runs_per_hour: 1, runs_per_day: 1, ...later }, opt_in_tools: [], deadline_seconds: 90,
		server_time: "2026-10-26T00:00:00Z", ...later,
	};
	assert.ok(Value.Check(HelloResponse, hello));
	assert.ok(Value.Check(AgentRenewResponse, { projects: [], ...later }));
	assert.ok(Value.Check(CasesNextResponse, { case: null, ...later }));
	assert.ok(!Value.Check(ToolRequests.list_sessions, { active_only: true, ...later }), "requests are closed");
});
