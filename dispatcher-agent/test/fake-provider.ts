// A fake model provider over real HTTP: OpenAI chat completions and Anthropic messages, both SSE. It records the
// auth header of every request. In `fail` mode it answers with that status and echoes the key it received in the
// body and a header, as some providers do, so the redaction tests have something to leak.
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";

export interface ProviderRequest {
	path: string;
	authorization: string | undefined;
	apiKey: string | undefined;
}

export class FakeProvider {
	readonly requests: ProviderRequest[] = [];
	/** The tool calls to make, in order, before ending with text. */
	plan: [string, Record<string, unknown>][] = [];
	fail: number | null = null;
	private server: Server | undefined;
	url = "";

	/** Listens on 127.0.0.1, on `port` or any free one. */
	async start(port = 0): Promise<this> {
		this.server = createServer((req, res) => {
			let raw = "";
			req.on("data", (d) => (raw += d));
			req.on("end", () => {
				const body = JSON.parse(raw || "{}");
				const authorization = req.headers.authorization;
				const apiKey = req.headers["x-api-key"] as string | undefined;
				this.requests.push({ path: req.url ?? "", authorization, apiKey });
				if (this.fail !== null) {
					const echoed = apiKey ?? authorization ?? "";
					res.writeHead(this.fail, { "content-type": "application/json", "x-echo-key": echoed });
					res.end(JSON.stringify({ error: { type: "authentication_error", message: `Incorrect API key provided: ${echoed}` } }));
					return;
				}
				if (req.url?.startsWith("/v1/chat/completions")) return this.openai(res, body.messages ?? []);
				if (req.url?.startsWith("/v1/messages")) return this.anthropic(res, body.messages ?? []);
				res.writeHead(404);
				res.end();
			});
		});
		await new Promise<void>((r) => this.server?.listen(port, "127.0.0.1", r));
		this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
		return this;
	}

	async stop(): Promise<void> {
		this.server?.closeAllConnections();
		await new Promise<void>((r) => this.server?.close(() => r()));
	}

	private openai(res: import("node:http").ServerResponse, messages: { role: string }[]) {
		const step = messages.filter((m) => m.role === "tool").length;
		const chunk = (delta: object, finish?: string) => ({
			id: "x", object: "chat.completion.chunk", created: 0, model: "fake",
			choices: [{ index: 0, delta, finish_reason: finish ?? null }],
		});
		const call = this.plan[step];
		const chunks = call
			? [
					chunk({ role: "assistant", tool_calls: [{ index: 0, id: `call_${step}`, type: "function", function: { name: call[0], arguments: "" } }] }),
					chunk({ tool_calls: [{ index: 0, function: { arguments: JSON.stringify(call[1]) } }] }),
					chunk({}, "tool_calls"),
				]
			: [chunk({ role: "assistant", content: "done" }), chunk({}, "stop")];
		res.writeHead(200, { "content-type": "text/event-stream" });
		for (const c of chunks) res.write(`data: ${JSON.stringify(c)}\n\n`);
		res.end("data: [DONE]\n\n");
	}

	private anthropic(res: import("node:http").ServerResponse, messages: { content: unknown }[]) {
		const step = messages.filter(
			(m) => Array.isArray(m.content) && m.content.some((c: { type?: string }) => c.type === "tool_result"),
		).length;
		const call = this.plan[step];
		const ev: [string, object][] = [
			["message_start", { type: "message_start", message: { id: `msg_${step}`, type: "message", role: "assistant", model: "fake", content: [], stop_reason: null, usage: { input_tokens: 900, output_tokens: 1 } } }],
		];
		if (call) {
			ev.push(["content_block_start", { type: "content_block_start", index: 0, content_block: { type: "tool_use", id: `toolu_${step}`, name: call[0], input: {} } }]);
			ev.push(["content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "input_json_delta", partial_json: JSON.stringify(call[1]) } }]);
			ev.push(["content_block_stop", { type: "content_block_stop", index: 0 }]);
			ev.push(["message_delta", { type: "message_delta", delta: { stop_reason: "tool_use" }, usage: { output_tokens: 40 } }]);
		} else {
			ev.push(["content_block_start", { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } }]);
			ev.push(["content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "done" } }]);
			ev.push(["content_block_stop", { type: "content_block_stop", index: 0 }]);
			ev.push(["message_delta", { type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 2 } }]);
		}
		ev.push(["message_stop", { type: "message_stop" }]);
		res.writeHead(200, { "content-type": "text/event-stream" });
		for (const [e, d] of ev) res.write(`event: ${e}\ndata: ${JSON.stringify(d)}\n\n`);
		res.end();
	}
}
