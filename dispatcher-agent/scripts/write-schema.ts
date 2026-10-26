// Writes api/dispatcher-model/v1.schema.json from src/schema.ts. `--check` fails if the committed file differs.
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { API_VERSION, definitions, ToolRequests, type ToolName } from "../src/schema.ts";

export const SCHEMA_PATH = fileURLToPath(new URL("../../api/dispatcher-model/v1.schema.json", import.meta.url));

const ref = (name: string) => `#/$defs/${name}`;

/** The schema document, as the server embeds it. */
export function schemaDocument(): string {
	const operations: Record<string, object> = {
		agent_config: { method: "POST", path: "/v1/dispatcher-model/agent/config", auth: "agent_credential", request: ref("AgentConfigRequest"), response: ref("AgentConfigResponse") },
		agent_renew: { method: "POST", path: "/v1/dispatcher-model/agent/renew", auth: "agent_credential", request: ref("AgentRenewRequest"), response: ref("AgentRenewResponse") },
		agent_test_result: { method: "POST", path: "/v1/dispatcher-model/agent/test-result", auth: "agent_credential", request: ref("AgentTestResultRequest"), response: ref("OkResponse") },
		agent_status: { method: "POST", path: "/v1/dispatcher-model/agent/status", auth: "agent_credential", request: ref("AgentStatusRequest"), response: ref("OkResponse") },
		hello: { method: "GET", path: "/v1/dispatcher-model/hello", auth: "project_token", response: ref("HelloResponse") },
		cases_next: { method: "POST", path: "/v1/dispatcher-model/cases/next", auth: "project_token", request: ref("CasesNextRequest"), response: ref("CasesNextResponse") },
	};
	for (const name of Object.keys(ToolRequests) as ToolName[]) {
		operations[`tool_${name}`] = {
			method: "POST",
			path: `/v1/dispatcher-model/cases/{case_id}/tools/${name}`,
			auth: "project_token",
			request: ref(`Tool_${name}_Request`),
			response: ref("ToolResponse"),
		};
	}
	const doc = {
		$schema: "https://json-schema.org/draft/2020-12/schema",
		$id: `urn:clawbridge:dispatcher-model:v${API_VERSION}`,
		title: `ClawBridge dispatcher-model API v${API_VERSION}`,
		description:
			"Generated from dispatcher-agent/src/schema.ts by dispatcher-agent/scripts/write-schema.ts; edit that file, not this one. " +
			"Every request sends the header ClawBridge-Model-API: 1. Errors are 4xx/503 with an ErrorResponse body.",
		"x-operations": operations,
		$defs: definitions(),
	};
	return `${JSON.stringify(doc, null, "\t")}\n`;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	const next = schemaDocument();
	if (process.argv.includes("--check")) {
		let current = "";
		try {
			current = readFileSync(SCHEMA_PATH, "utf8");
		} catch {}
		if (current !== next) {
			console.error("api/dispatcher-model/v1.schema.json is out of date: run npm run schema in dispatcher-agent");
			process.exit(1);
		}
		console.log("v1.schema.json matches src/schema.ts");
	} else {
		writeFileSync(SCHEMA_PATH, next);
		console.log("wrote api/dispatcher-model/v1.schema.json");
	}
}
