// One case, one fresh Agent (design §7). The model may read through the case's tools and must end with one outcome;
// on any deadline, cap, refusal or provider failure the agent itself sends abstain(uncertain), so the server falls
// back to rules in time. Nothing from the run outlives it.
import { Agent, type AgentTool, type StreamFn } from "@earendil-works/pi-agent-core";
import type { AssistantMessage, Model } from "@earendil-works/pi-ai";
import type { ClawBridgeClient } from "./client.ts";
import { type Logger, providerFailure, type ProviderFailure } from "./log.ts";
import { renderCase, SYSTEM_PROMPT } from "./prompt.ts";
import { type CaseT, type OptInTool, type ToolName, ToolResponse, type UsageT } from "./schema.ts";
import { MAX_READ_CALLS, toolsForCase } from "./tools.ts";

export interface RunCaseDeps {
	client: ClawBridgeClient;
	/** The project's current token; read at each call, so a renewal mid-case is picked up. */
	token: () => string;
	streamFn: StreamFn;
	model: Model<string>;
	limits: { max_input_tokens_per_run: number; max_output_tokens_per_run: number };
	logger: Logger;
	projectId: string;
	/** Stop the model this long before the deadline, leaving time for the outcome call (d4 §2: about 5 s). */
	marginMs?: number;
	now?: () => number;
}

export interface CaseResult {
	/** The outcome tool the server recorded, or "none" when the case closed without one from this run. */
	outcome: ToolName | "none";
	/** Why the agent abstained or stopped, as a code; absent when the model's own outcome was accepted. */
	code?: string;
	/** The provider refused the key (401/403): stop taking cases for this key-holder (design §7). */
	providerAuth: boolean;
}

const RETRYABLE = new Set(["network", "timeout", "server_busy"]);
const CLOSED = new Set(["case_closed", "deadline_passed", "case_not_found", "case_leased"]);

export async function runCase(c: CaseT, optIn: readonly OptInTool[], deps: RunCaseDeps): Promise<CaseResult> {
	const now = deps.now ?? Date.now;
	const started = now();
	const deadlineAt = started + c.deadline_ms;
	const stopModelAt = deadlineAt - (deps.marginMs ?? 5_000);
	const path = (name: string) => `/v1/dispatcher-model/cases/${encodeURIComponent(c.case_id)}/tools/${name}`;
	const log = (event: string, fields: Parameters<Logger["log"]>[1] = {}) =>
		deps.logger.log(event, { project_id: deps.projectId, case_id: c.case_id, kind: c.kind, ...fields });

	let inputTokens = 0;
	let outputTokens = 0;
	let serverToolCalls = 0;
	let readCalls = 0;
	let httpStatus: number | null = null;
	let stopCode: string | undefined;
	let done: CaseResult | undefined;
	let finished = false; // set once the run is over: a stream that ignored the abort can't act after it
	let release: () => void = () => {};
	const released = new Promise<void>((r) => {
		release = r;
	});

	const usage = (): UsageT => ({
		input_tokens: inputTokens,
		output_tokens: outputTokens,
		tool_calls: serverToolCalls,
		duration_ms: Math.max(0, Math.round(now() - started)),
		provider: deps.model.provider,
		model: deps.model.id,
	});

	// Outcomes are idempotent [A5]: a retry resends the identical body, usage included.
	const sendOutcome = async (name: ToolName, args: Record<string, unknown>): Promise<{ ok: boolean; code?: string }> => {
		serverToolCalls++;
		const body = { ...args, usage: usage() };
		for (let attempt = 1; attempt <= 3; attempt++) {
			const remaining = deadlineAt - now();
			if (remaining <= 0) return { ok: false, code: "deadline_passed" };
			const r = await deps.client.call("POST", path(name), deps.token(), ToolResponse, body, {
				timeoutMs: Math.min(remaining, 5_000),
			});
			if (r.ok) return { ok: true };
			log("outcome_failed", { tool: name, code: r.code, status: r.status, attempt });
			if (!RETRYABLE.has(r.code) && !/^http_5\d\d$/.test(r.code)) return { ok: false, code: r.code };
			await sleep(Math.min((r.retryAfter ?? 0.5) * 1000, Math.max(0, deadlineAt - now() - 500)));
		}
		return { ok: false, code: "retries_exhausted" };
	};

	const caseTools = toolsForCase(c, optIn);
	const allowed = new Set<string>(caseTools.map((t) => t.name));
	const tools: AgentTool[] = caseTools.map((t) => ({
		name: t.name,
		label: t.name,
		description: t.description,
		parameters: t.parameters,
		execute: async (_id, params) => {
			const args = params as Record<string, unknown>;
			if (finished) return { content: [{ type: "text", text: '{"error":"run_ended"}' }], details: undefined, isError: true };
			if (t.outcome) {
				const r = await sendOutcome(t.name, args);
				if (r.ok) {
					done = { outcome: t.name, providerAuth: false };
					log("outcome", { tool: t.name, ...usageFields(usage()) });
				} else {
					stopCode = CLOSED.has(r.code ?? "") ? r.code : `outcome_${r.code}`;
					if (CLOSED.has(r.code ?? "")) done = { outcome: "none", code: r.code, providerAuth: false };
				}
				return { content: [{ type: "text", text: r.ok ? '{"ok":true}' : JSON.stringify({ error: r.code }) }], details: undefined, terminate: true };
			}
			if (readCalls >= MAX_READ_CALLS) {
				return { content: [{ type: "text", text: '{"error":"cap_reached"}' }], details: undefined, isError: true };
			}
			readCalls++;
			serverToolCalls++;
			const remaining = stopModelAt - now();
			const r = await deps.client.call("POST", path(t.name), deps.token(), ToolResponse, args, {
				timeoutMs: Math.max(1, Math.min(remaining, 10_000)),
			});
			log("tool", { tool: t.name, status: r.status, code: r.ok ? undefined : r.code });
			if (!r.ok && CLOSED.has(r.code ?? "")) {
				// The case is no longer this agent's (a restarted agent took its lease over, d5 Q9) or is over: stop
				// without an outcome.
				stopCode = r.code;
				done = { outcome: "none", code: r.code, providerAuth: false };
				return { content: [{ type: "text", text: JSON.stringify({ error: r.code }) }], details: undefined, terminate: true };
			}
			if (!r.ok) return { content: [{ type: "text", text: JSON.stringify({ error: r.code }) }], details: undefined, isError: true };
			return { content: [{ type: "text", text: JSON.stringify(modelResult(t.name, r.body.result)) }], details: undefined };
		},
	}));

	const { max_input_tokens_per_run: maxIn, max_output_tokens_per_run: maxOut } = deps.limits;
	const streamFn: StreamFn = (model, context, options) => {
		if (inputTokens >= maxIn || outputTokens >= maxOut) {
			stopCode ??= "token_cap";
			throw new Error("token cap reached");
		}
		const timeoutMs = Math.max(1, stopModelAt - now());
		return deps.streamFn(model, context, {
			...options,
			maxTokens: Math.max(1, maxOut - outputTokens),
			maxRetries: 1, // one retry with backoff, inside the deadline (design §7)
			maxRetryDelayMs: Math.min(5_000, timeoutMs),
			timeoutMs,
		});
	};

	const agent = new Agent({
		initialState: { systemPrompt: SYSTEM_PROMPT, model: deps.model, tools },
		streamFn,
		toolExecution: "sequential",
		onResponse: (response) => {
			httpStatus = response.status;
		},
		beforeToolCall: async ({ toolCall }) =>
			done || !allowed.has(toolCall.name) ? { block: true, reason: "tool_refused", terminate: true } : undefined,
	});
	let lastAssistant: AssistantMessage | undefined;
	agent.subscribe((event) => {
		if (event.type !== "message_end" || event.message.role !== "assistant") return;
		const m = event.message as AssistantMessage;
		lastAssistant = m;
		inputTokens += m.usage.input + m.usage.cacheRead + m.usage.cacheWrite;
		outputTokens += m.usage.output;
		// pi answers an unregistered tool with "not found" and lets the model go on; the design ends the run instead.
		const unknown = m.content.some((b) => b.type === "toolCall" && !allowed.has(b.name));
		if (unknown) stopRun("unknown_tool");
	});

	// Abort the model, and stop waiting for it shortly after: the outcome must not depend on a stream honouring abort.
	let graceTimer: NodeJS.Timeout | undefined;
	function stopRun(code: string) {
		stopCode ??= code;
		agent.abort();
		graceTimer ??= setTimeout(release, 250);
	}
	const timer = setTimeout(() => stopRun("deadline"), Math.max(0, stopModelAt - now()));
	const prompting = agent.prompt(renderCase(c)).catch(() => {
		stopCode ??= "agent_error";
	});
	await Promise.race([prompting, released]);
	finished = true;
	clearTimeout(timer);
	clearTimeout(graceTimer);

	let failure: ProviderFailure | undefined;
	if (!done && lastAssistant?.stopReason === "error" && !stopCode) {
		failure = providerFailure("error", lastAssistant.errorMessage, httpStatus);
		stopCode = `provider_${failure.error_type}`;
		log("provider_error", { status: failure.status, error_type: failure.error_type });
	}
	const providerAuth = failure?.error_type === "auth" || failure?.error_type === "forbidden";
	if (done) return { ...done, providerAuth };

	const code = stopCode ?? "no_outcome";
	const r = await sendOutcome("abstain", { reason_code: "uncertain" });
	log(r.ok ? "abstained" : "abstain_failed", { code: r.ok ? code : r.code, ...usageFields(usage()) });
	return { outcome: r.ok ? "abstain" : "none", code: r.ok ? code : r.code, providerAuth };
}

function usageFields(u: UsageT) {
	return { input_tokens: u.input_tokens, output_tokens: u.output_tokens, tool_calls: u.tool_calls, duration_ms: u.duration_ms };
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/**
 * A read tool's result as the model sees it. list_decisions' explanations are earlier model output, which an issue's
 * text could have steered, so each moves under `untrusted`: data, never instructions (d6). A result in any other
 * shape than the agreed array fails closed: all of it goes under `untrusted`.
 */
export function modelResult(tool: string, result: unknown): unknown {
	if (tool !== "list_decisions") return result;
	if (!Array.isArray(result)) return { untrusted: { result } };
	return result.map((item) => {
		if (item === null || typeof item !== "object" || !("explanation" in item)) return item;
		const { explanation, ...rest } = item as Record<string, unknown>;
		return { ...rest, untrusted: { explanation } };
	});
}
