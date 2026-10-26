// The dispatcher-model API, v1 (live contract d4): every request, response and error code, defined once.
// scripts/write-schema.ts writes api/dispatcher-model/v1.schema.json from this file, and the server embeds and
// validates against that JSON, so a contract change is made here and reviewed as a diff of that file.
// Requests are closed (the server refuses unknown fields). Responses are open, so additive server changes keep v1.
import { Type, type Static, type TSchema } from "typebox";

export const API_VERSION = 1;

const Closed = { additionalProperties: false } as const;
const Id = Type.String({ minLength: 1, maxLength: 128 });
const Uuid = Type.String({ format: "uuid" });
const Time = Type.String({ format: "date-time" });
const Code = Type.String({ pattern: "^[a-z][a-z0-9_]{0,63}$" });

export const ERROR_CODES = [
	"unauthorized",
	"unsupported_version",
	"agent_cap",
	"case_not_found",
	"case_closed",
	"case_leased",
	"deadline_passed",
	"tool_refused",
	"invalid_argument",
	"not_in_case",
	"cap_reached",
	"guardrail",
	"server_busy",
	"conflict",
] as const;
export type ErrorCode = (typeof ERROR_CODES)[number];

export const ErrorResponse = Type.Object({
	error: Type.Enum(ERROR_CODES),
	tool: Type.Optional(Id),
	supported: Type.Optional(Type.Array(Type.Integer())),
	retry_after: Type.Optional(Type.Integer({ minimum: 0 })),
});

// The design's engine actions (DISPATCHER-DESIGN §3.2); a case's permitted_actions is a subset.
export const ACTIONS = ["confirm", "escalate", "wait", "decline", "route", "release_pending", "release"] as const;
export type Action = (typeof ACTIONS)[number];
const ActionSchema = Type.Enum(ACTIONS);

export const OPT_IN_TOOLS = ["route_notice", "ask_session"] as const;
export type OptInTool = (typeof OPT_IN_TOOLS)[number];

// --- §1b agent configuration ---

const ProjectToken = Type.Object({ project_id: Uuid, token: Type.String({ minLength: 1 }), expires_at: Time });

export const AgentConfigRequest = Type.Object({ instance_id: Id }, Closed);
export const AgentConfigResponse = Type.Object({
	config_revision: Type.Integer({ minimum: 1 }),
	provider: Type.Object({
		id: Id,
		api: Id,
		base_url: Type.String({ format: "uri" }),
		model: Id,
		key: Type.String({ minLength: 1 }),
	}),
	projects: Type.Array(ProjectToken),
	limits: Type.Object({
		max_input_tokens_per_run: Type.Integer({ minimum: 1 }),
		max_output_tokens_per_run: Type.Integer({ minimum: 1 }),
	}),
	binding_secret: Type.String({ minLength: 43 }),
	/** pending_test: run the setup test (design §11.3) and post agent/test-result before any case (d4.1). */
	status: Type.Enum(["pending_test", "active"]),
});

export const AgentRenewRequest = Type.Object({ binding_secret: Type.String({ minLength: 43 }) }, Closed);
export const AgentRenewResponse = Type.Object({ projects: Type.Array(ProjectToken) });

// d4.1: the setup test's result, and the agent's status for doctor. Fixed codes only, never provider text.
export const SETUP_TEST_FAILURES = [
	"invalid_key",
	"no_model_access",
	"quota",
	"rate_limited",
	"no_tool_use",
	"egress_blocked",
	"provider_error",
	"timeout",
] as const;
export type SetupTestFailure = (typeof SETUP_TEST_FAILURES)[number];
const BindingSecret = Type.String({ minLength: 43 });
export const AgentTestResultRequest = Type.Union([
	Type.Object(
		{ binding_secret: BindingSecret, config_revision: Type.Integer({ minimum: 1 }), result: Type.Literal("active") },
		Closed,
	),
	Type.Object(
		{
			binding_secret: BindingSecret,
			config_revision: Type.Integer({ minimum: 1 }),
			result: Type.Literal("failed"),
			reason: Type.Enum(SETUP_TEST_FAILURES),
		},
		Closed,
	),
]);
export const AgentStatusRequest = Type.Object(
	{ binding_secret: BindingSecret, status: Type.Enum(["ok", "provider_auth"]) },
	Closed,
);
export const OkResponse = Type.Object({ ok: Type.Literal(true) });

// --- §2 hello and cases ---

export const HelloResponse = Type.Object({
	project_id: Uuid,
	enabled: Type.Boolean(),
	provider: Id,
	model: Id,
	caps: Type.Object({
		runs_per_hour: Type.Integer({ minimum: 0 }),
		runs_per_day: Type.Integer({ minimum: 0 }),
	}),
	opt_in_tools: Type.Array(Type.Enum(OPT_IN_TOOLS)),
	deadline_seconds: Type.Integer({ minimum: 30, maximum: 300 }),
	server_time: Time,
});

export const CASE_KINDS = ["advise", "decide", "chat"] as const;
export type CaseKind = (typeof CASE_KINDS)[number];

const Principal = Type.String({ pattern: "^p_[A-Za-z0-9_-]{1,64}$" });
const Facts = Type.Object({
	issue: Type.Optional(
		Type.Object({
			id: Uuid,
			identifier: Type.String({ maxLength: 32 }),
			state_type: Code,
			project_id: Type.Union([Uuid, Type.Null()]),
			labels: Type.Array(Type.String({ maxLength: 128 })),
		}),
	),
	session: Type.Optional(
		Type.Object({
			id: Id,
			name: Type.String({ maxLength: 128 }),
			principal: Principal,
			presence: Type.Union([Type.Literal("active"), Type.Literal("stale")]),
			status: Type.Union([Type.Literal("active"), Type.Literal("retired"), Type.Literal("moved")]),
		}),
	),
	domain_matches: Type.Optional(
		Type.Array(
			Type.Object({ domain: Id, by: Type.Union([Type.Literal("project"), Type.Literal("label")]) }),
		),
	),
	holders: Type.Optional(Type.Array(Type.Object({ session_id: Id, since: Time }))),
});

export const Case = Type.Object({
	case_id: Id,
	kind: Type.Enum(CASE_KINDS),
	deadline: Time,
	deadline_ms: Type.Integer({ minimum: 0 }),
	subject: Type.Object({
		type: Type.Union([
			Type.Literal("proposal"),
			Type.Literal("assignment"),
			Type.Literal("notice"),
			Type.Literal("message"),
		]),
		id: Id,
	}),
	permitted_actions: Type.Optional(Type.Array(ActionSchema)),
	reason_codes: Type.Array(Code),
	facts: Facts,
	untrusted: Type.Optional(
		Type.Object({
			title: Type.Optional(Type.String({ maxLength: 1024 })),
			description_excerpt: Type.Optional(Type.String({ maxLength: 4096 })),
			truncated: Type.Optional(Type.Boolean()),
		}),
	),
	message: Type.Optional(
		Type.Object({
			from: Type.Object({ session: Type.String({ maxLength: 128 }), principal: Principal }),
			text: Type.String({ maxLength: 4096 }),
		}),
	),
});
export type CaseT = Static<typeof Case>;

export const CasesNextRequest = Type.Object({ wait_seconds: Type.Integer({ minimum: 0, maximum: 30 }) }, Closed);
export const CasesNextResponse = Type.Object({ case: Type.Union([Case, Type.Null()]) });

// --- §3 tools: each request is exactly what the model may send, so these double as the tools' parameters ---

const Explanation = Type.String({ maxLength: 280, description: "At most 280 characters, shown as the model's words." });

export const ToolRequests = {
	read_issue_facts: Type.Object({ issue_id: Uuid }, Closed),
	list_domain: Type.Object({ domain: Id }, Closed),
	list_sessions: Type.Object({ active_only: Type.Boolean() }, Closed),
	session_activity: Type.Object(
		{ session_id: Id, since_hours: Type.Integer({ minimum: 1, maximum: 168 }) },
		Closed,
	),
	list_decisions: Type.Object(
		{ since_hours: Type.Integer({ minimum: 1, maximum: 168 }), limit: Type.Integer({ minimum: 1, maximum: 50 }) },
		Closed,
	),
	recommend: Type.Object({ action: ActionSchema, reason_code: Code, explanation: Explanation }, Closed),
	decide: Type.Object({ action: ActionSchema, reason_code: Code, explanation: Explanation }, Closed),
	reply: Type.Object({ text: Type.String({ minLength: 1, maxLength: 2000 }) }, Closed),
	abstain: Type.Object(
		{
			reason_code: Type.Union([
				Type.Literal("insufficient_facts"),
				Type.Literal("uncertain"),
				Type.Literal("refused"),
			]),
		},
		Closed,
	),
	route_notice: Type.Object({ notice_id: Id, domain: Id }, Closed),
	ask_session: Type.Object(
		{
			template: Type.Union([
				Type.Literal("which_domain"),
				Type.Literal("still_working"),
				Type.Literal("scope_unclear"),
			]),
			session_id: Id,
			issue_id: Uuid,
		},
		Closed,
	),
} satisfies Record<string, TSchema>;
export type ToolName = keyof typeof ToolRequests;
export const OUTCOME_TOOLS = ["recommend", "decide", "reply", "abstain"] as const satisfies readonly ToolName[];

// Outcome calls carry the run's usage [A10]. The agent adds it; the model never sees this field.
export const Usage = Type.Object(
	{
		input_tokens: Type.Integer({ minimum: 0 }),
		output_tokens: Type.Integer({ minimum: 0 }),
		tool_calls: Type.Integer({ minimum: 0 }),
		duration_ms: Type.Integer({ minimum: 0 }),
		provider: Id,
		model: Id,
	},
	Closed,
);
export type UsageT = Static<typeof Usage>;

export const ToolResponse = Type.Object({ ok: Type.Literal(true), result: Type.Unknown() });
export const DecideResult = Type.Object({
	guardrail: Type.Union([Type.Literal("permitted"), Type.Literal("overridden")]),
	action_taken: ActionSchema,
	reason_code: Code,
});

/** The wire body of a tool call: the model's arguments, plus `usage` on an outcome. */
export function toolWireRequest(name: ToolName): TSchema {
	const req = ToolRequests[name];
	if (!(OUTCOME_TOOLS as readonly string[]).includes(name)) return req;
	return Type.Object({ ...req.properties, usage: Usage }, Closed);
}

/** Every named definition, as written to v1.schema.json's $defs. */
export function definitions(): Record<string, TSchema> {
	const defs: Record<string, TSchema> = {
		ErrorResponse,
		AgentConfigRequest,
		AgentConfigResponse,
		AgentRenewRequest,
		AgentRenewResponse,
		AgentTestResultRequest,
		AgentStatusRequest,
		OkResponse,
		HelloResponse,
		Case,
		CasesNextRequest,
		CasesNextResponse,
		Usage,
		ToolResponse,
		DecideResult,
	};
	for (const name of Object.keys(ToolRequests) as ToolName[]) defs[`Tool_${name}_Request`] = toolWireRequest(name);
	return defs;
}
