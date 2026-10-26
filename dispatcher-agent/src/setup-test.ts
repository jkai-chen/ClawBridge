// The setup test (design §11.3): one request with one no-op tool, proving the key, the model, tool use and the
// provider host from this container. The result is a fixed code; provider text never leaves this function.
import { Agent, type StreamFn } from "@earendil-works/pi-agent-core";
import type { AssistantMessage, Model } from "@earendil-works/pi-ai";
import { Type } from "typebox";
import { providerFailure, type ProviderFailure } from "./log.ts";
import type { SetupTestFailure } from "./schema.ts";

export type SetupTestResult = { result: "active" } | { result: "failed"; reason: SetupTestFailure };

const TIMEOUT_MS = 30_000;

const REASONS: Record<ProviderFailure["error_type"], SetupTestFailure> = {
	auth: "invalid_key",
	forbidden: "no_model_access",
	not_found: "no_model_access",
	quota: "quota",
	rate_limited: "rate_limited",
	server: "provider_error",
	timeout: "timeout",
	aborted: "timeout",
	network: "egress_blocked",
	other: "provider_error",
};

export async function runSetupTest(
	streamFn: StreamFn,
	model: Model<string>,
	timeoutMs = TIMEOUT_MS,
): Promise<SetupTestResult> {
	let called = false;
	let timedOut = false;
	let httpStatus: number | null = null;
	const agent = new Agent({
		initialState: {
			systemPrompt: "This is a connection test. Call the ping tool once, with no arguments, then stop.",
			model,
			tools: [
				{
					name: "ping",
					label: "ping",
					description: "Answers the connection test.",
					parameters: Type.Object({}, { additionalProperties: false }),
					execute: async () => {
						called = true;
						return { content: [{ type: "text", text: "pong" }], details: undefined, terminate: true };
					},
				},
			],
		},
		streamFn: (m, context, options) => streamFn(m, context, { ...options, maxTokens: 32, maxRetries: 0, timeoutMs }),
		toolExecution: "sequential",
		onResponse: (response) => {
			httpStatus = response.status;
		},
	});
	let last: AssistantMessage | undefined;
	agent.subscribe((event) => {
		if (event.type === "message_end" && event.message.role === "assistant") last = event.message as AssistantMessage;
	});
	let release: () => void = () => {};
	const released = new Promise<void>((r) => {
		release = r;
	});
	const timer = setTimeout(() => {
		timedOut = true;
		agent.abort();
		setTimeout(release, 250);
	}, timeoutMs);
	await Promise.race([agent.prompt("ping").catch(() => {}), released]);
	clearTimeout(timer);

	if (called) return { result: "active" };
	if (timedOut) return { result: "failed", reason: "timeout" };
	if (last?.stopReason !== "error") return { result: "failed", reason: "no_tool_use" };
	const f = providerFailure("error", last.errorMessage, httpStatus);
	return { result: "failed", reason: REASONS[f.error_type] };
}
