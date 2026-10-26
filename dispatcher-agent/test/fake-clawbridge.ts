// A fake ClawBridge speaking d4. Every request it receives and every response it sends is checked against the
// committed api/dispatcher-model/v1.schema.json definitions; mismatches are collected in `violations`.
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import type { Static, TSchema } from "typebox";
import { Value } from "typebox/value";
import {
	AgentConfigRequest,
	AgentConfigResponse,
	AgentRenewRequest,
	AgentRenewResponse,
	type CaseT,
	CasesNextRequest,
	CasesNextResponse,
	ErrorResponse,
	HelloResponse,
	type ToolName,
	ToolRequests,
	ToolResponse,
	toolWireRequest,
} from "../src/schema.ts";

export const PROJECT = "0b9f6c5e-3a4d-4e8b-9c1f-2d3e4f5a6b7c";
export const ISSUE = "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b";
export const CREDENTIAL = "cba_Q7x2Lk9PzR4mWv8NcT3jYb";
export const PROJECT_TOKEN = "cbm_H5tY8uJ2nB6vC9xZ4qWeRd";
export const BINDING_SECRET = "Xk3Pq9Wm2Zt7Rv5Ln8Hb4Jc6Fd1Sg0Ay3Ue7Io2Ew5Tq9";

export interface Recorded {
	method: string;
	path: string;
	auth: string | undefined;
	body: unknown;
}

export interface Outcome {
	case_id: string;
	tool: ToolName;
	body: Record<string, unknown>;
}

type ToolHandler = (caseId: string, body: Record<string, unknown>) => { status: number; body: unknown } | undefined;

export function sampleCase(over: Partial<CaseT> = {}): CaseT {
	return {
		case_id: "case_1",
		kind: "decide",
		deadline: new Date(Date.now() + 60_000).toISOString(),
		deadline_ms: 60_000,
		subject: { type: "proposal", id: "prp_1" },
		permitted_actions: ["escalate", "wait"],
		reason_codes: ["untrusted", "overlap", "insufficient_trust"],
		facts: {
			issue: { id: ISSUE, identifier: "ABC-12", state_type: "started", project_id: null, labels: ["payments"] },
			session: { id: "ses_1", name: "eng-1", principal: "p_abc", presence: "active", status: "active" },
			domain_matches: [{ domain: "payments", by: "label" }],
			holders: [],
		},
		untrusted: { title: "Fix the ledger", description_excerpt: "Ledger rounding is off.", truncated: false },
		...over,
	};
}

export class FakeClawBridge {
	readonly requests: Recorded[] = [];
	readonly violations: string[] = [];
	readonly outcomes: Outcome[] = [];
	readonly cases: CaseT[] = [];
	providerKey = "fake-test-provider-key-0000000000000000";
	providerBaseUrl = "http://127.0.0.1:9";
	providerApi = "openai-completions";
	hello: Partial<Static<typeof HelloResponse>> = {};
	tokenLifetimeMs = 3_600_000;
	/** Overrides a tool's response; return undefined for the default. */
	onTool: ToolHandler = () => undefined;
	private server: Server | undefined;
	private open = new Map<string, CaseT>();
	url = "";

	async start(): Promise<this> {
		this.server = createServer((req, res) => void this.handle(req, res));
		await new Promise<void>((r) => this.server?.listen(0, "127.0.0.1", r));
		this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
		return this;
	}

	async stop(): Promise<void> {
		this.server?.closeAllConnections();
		await new Promise<void>((r) => this.server?.close(() => r()));
	}

	private check(what: string, schema: TSchema, value: unknown) {
		if (!Value.Check(schema, value)) {
			const first = [...Value.Errors(schema, value)][0];
			this.violations.push(`${what}: ${first ? `${first.instancePath} ${first.message}` : "invalid"}`);
		}
	}

	private send(res: ServerResponse, status: number, schema: TSchema, body: unknown, what: string) {
		this.check(`${what} response`, status < 400 ? schema : ErrorResponse, body);
		res.writeHead(status, { "content-type": "application/json" });
		res.end(JSON.stringify(body));
	}

	private projects() {
		const expires_at = new Date(Date.now() + this.tokenLifetimeMs).toISOString();
		return [{ project_id: PROJECT, token: PROJECT_TOKEN, expires_at }];
	}

	private async handle(req: IncomingMessage, res: ServerResponse) {
		let raw = "";
		for await (const chunk of req) raw += chunk;
		const body = raw ? JSON.parse(raw) : undefined;
		const path = req.url ?? "";
		const auth = req.headers.authorization?.replace(/^Bearer /, "");
		this.requests.push({ method: req.method ?? "", path, auth, body });
		if (req.headers["clawbridge-model-api"] !== "1") this.violations.push(`${path}: missing ClawBridge-Model-API: 1`);
		const unauthorized = (what: string) => this.send(res, 401, ErrorResponse, { error: "unauthorized" }, what);

		if (path === "/v1/dispatcher-model/agent/config") {
			this.check("agent_config request", AgentConfigRequest, body);
			if (auth !== CREDENTIAL) return unauthorized("agent_config");
			return this.send(res, 200, AgentConfigResponse, {
				config_revision: 7,
				provider: { id: "openai", api: this.providerApi, base_url: this.providerBaseUrl, model: "test-model", key: this.providerKey },
				projects: this.projects(),
				limits: { max_input_tokens_per_run: 20_000, max_output_tokens_per_run: 2_000 },
				binding_secret: BINDING_SECRET,
			}, "agent_config");
		}
		if (path === "/v1/dispatcher-model/agent/renew") {
			this.check("agent_renew request", AgentRenewRequest, body);
			if (auth !== CREDENTIAL || body?.binding_secret !== BINDING_SECRET) return unauthorized("agent_renew");
			return this.send(res, 200, AgentRenewResponse, { projects: this.projects() }, "agent_renew");
		}
		if (auth !== PROJECT_TOKEN) return unauthorized(path);
		if (path === "/v1/dispatcher-model/hello") {
			return this.send(res, 200, HelloResponse, {
				project_id: PROJECT, enabled: true, provider: "openai", model: "test-model",
				caps: { runs_per_hour: 10, runs_per_day: 50 }, opt_in_tools: [], deadline_seconds: 90,
				server_time: new Date().toISOString(), ...this.hello,
			}, "hello");
		}
		if (path === "/v1/dispatcher-model/cases/next") {
			this.check("cases_next request", CasesNextRequest, body);
			const open = [...this.open.values()][0];
			const c = open ?? this.cases.shift() ?? null;
			if (c) this.open.set(c.case_id, c);
			else await new Promise((r) => setTimeout(r, 20));
			return this.send(res, 200, CasesNextResponse, { case: c }, "cases_next");
		}
		const m = /^\/v1\/dispatcher-model\/cases\/([^/]+)\/tools\/([a-z_]+)$/.exec(path);
		if (m) {
			const [, caseId, name] = m as unknown as [string, string, ToolName];
			if (!(name in ToolRequests)) return this.send(res, 400, ErrorResponse, { error: "tool_refused", tool: "unknown" }, "tool");
			this.check(`tool ${name} request`, toolWireRequest(name), body);
			const custom = this.onTool(caseId, body);
			if (custom) return this.send(res, custom.status, ToolResponse, custom.body, `tool ${name}`);
			const outcome = ["recommend", "decide", "reply", "abstain"].includes(name);
			if (outcome) {
				const prior = this.outcomes.find((o) => o.case_id === caseId);
				if (prior && (prior.tool !== name || JSON.stringify(prior.body) !== JSON.stringify(body))) {
					return this.send(res, 409, ErrorResponse, { error: "case_closed" }, `tool ${name}`);
				}
				if (!prior) this.outcomes.push({ case_id: caseId, tool: name, body });
				this.open.delete(caseId);
				const result = name === "decide" ? { guardrail: "permitted", action_taken: body.action, reason_code: body.reason_code } : {};
				return this.send(res, 200, ToolResponse, { ok: true, result }, `tool ${name}`);
			}
			return this.send(res, 200, ToolResponse, { ok: true, result: { tool: name, note: "fake read" } }, `tool ${name}`);
		}
		return this.send(res, 404, ErrorResponse, { error: "case_not_found" }, path);
	}
}
