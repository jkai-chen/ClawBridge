// The whole agent against a fake ClawBridge and a fake provider over real HTTP, with the provider built from the
// server's config exactly as in production.
import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { runAgent } from "../src/agent.ts";
import { ClawBridgeClient } from "../src/client.ts";
import { Logger } from "../src/log.ts";
import {
	BINDING_SECRET,
	CREDENTIAL,
	FakeClawBridge,
	ISSUE,
	PROJECT_TOKEN,
	sampleCase,
} from "./fake-clawbridge.ts";
import { FakeProvider } from "./fake-provider.ts";

const KEY = "fake-Zq3Xw8Rt5Vb2Nm7Kp4Lj9Hg6Df1Ys0";

let cb: FakeClawBridge;
let provider: FakeProvider;
let lines: string[];
beforeEach(async () => {
	cb = await new FakeClawBridge().start();
	provider = await new FakeProvider().start();
	cb.providerKey = KEY;
	cb.providerBaseUrl = `${provider.url}/v1`;
	provider.plan = [
		["read_issue_facts", { issue_id: ISSUE }],
		["decide", { action: "escalate", reason_code: "untrusted", explanation: "Not on the trusted list." }],
	];
	lines = [];
});
afterEach(async () => {
	assert.deepEqual(cb.violations, [], "every request and response matches v1.schema.json");
	await cb.stop();
	await provider.stop();
});

/** Runs the agent until `until` holds (or 10 s), then stops it. */
async function runUntil(until: () => boolean, over: Partial<Parameters<typeof runAgent>[0]> = {}) {
	const ctrl = new AbortController();
	const running = runAgent({
		client: new ClawBridgeClient(new URL(cb.url)),
		logger: new Logger((l) => lines.push(l)),
		credential: CREDENTIAL,
		instanceId: "inst-test",
		signal: ctrl.signal,
		pollSeconds: 0,
		idleRetryMs: 50,
		...over,
	});
	const t0 = Date.now();
	while (!until() && Date.now() - t0 < 10_000) {
		const settled = await Promise.race([running.then(() => true), new Promise((r) => setTimeout(() => r(false), 20))]);
		if (settled) break;
	}
	ctrl.abort();
	return running;
}

/** True when any 8-character run of `secret` appears in `haystack` (the d4 redaction test). */
function leaks(secret: string, haystack: string): boolean {
	for (let i = 0; i + 8 <= secret.length; i++) if (haystack.includes(secret.slice(i, i + 8))) return true;
	return false;
}

for (const [api, path] of [["openai-completions", "/v1/chat/completions"], ["anthropic-messages", "/v1/messages"]] as const) {
	test(`${api}: config, hello, one case decided end to end`, async () => {
		cb.providerApi = api;
		if (api === "anthropic-messages") cb.providerBaseUrl = provider.url;
		cb.cases.push(sampleCase());
		await runUntil(() => cb.outcomes.length > 0);
		assert.equal(cb.outcomes[0]?.tool, "decide");
		assert.deepEqual(
			cb.requests.map((r) => r.path.replace(/cases\/(?!next)[^/]+/, "cases/:id")).slice(0, 6),
			[
				"/v1/dispatcher-model/agent/config",
				"/v1/dispatcher-model/agent/status",
				"/v1/dispatcher-model/hello",
				"/v1/dispatcher-model/cases/next",
				"/v1/dispatcher-model/cases/:id/tools/read_issue_facts",
				"/v1/dispatcher-model/cases/:id/tools/decide",
			],
		);
		assert.ok(provider.requests.length >= 2 && provider.requests.every((r) => r.path.startsWith(path)));
		const usage = cb.outcomes[0]?.body.usage as Record<string, unknown>;
		assert.deepEqual([usage.provider, usage.model], ["openai", "test-model"]);
	});
}

test("decoy keys in env are never sent: the explicit key from config is", async () => {
	const decoys = { OPENAI_API_KEY: "fake-decoy-openai-0000000000000000", ANTHROPIC_API_KEY: "fake-decoy-anthropic-000000000000" };
	Object.assign(process.env, decoys);
	try {
		for (const api of ["openai-completions", "anthropic-messages"] as const) {
			cb.providerApi = api;
			cb.providerBaseUrl = api === "anthropic-messages" ? provider.url : `${provider.url}/v1`;
			provider.requests.length = 0;
			cb.outcomes.length = 0;
			cb.cases.push(sampleCase({ case_id: `case_${api}` }));
			await runUntil(() => cb.outcomes.length > 0);
			assert.ok(provider.requests.length > 0, api);
			for (const r of provider.requests) {
				const sent = r.apiKey ?? r.authorization?.replace(/^Bearer /, "");
				assert.equal(sent, KEY, `${api} sent the configured key`);
			}
		}
	} finally {
		for (const k of Object.keys(decoys)) delete process.env[k];
	}
});

test("a provider 401 that echoes the key: no 8-character run of a secret in logs or in anything sent to ClawBridge", async () => {
	provider.fail = 401;
	cb.cases.push(sampleCase());
	const out: string[] = [];
	const write = process.stderr.write.bind(process.stderr);
	process.stderr.write = ((chunk: string | Uint8Array) => (out.push(String(chunk)), true)) as typeof process.stderr.write;
	let stopped: string;
	try {
		({ stopped } = await runUntil(() => cb.statuses.includes("provider_auth")));
	} finally {
		process.stderr.write = write;
	}
	assert.equal(stopped, "provider_auth", "a rejected key stops the key-holder's agent");
	assert.deepEqual(cb.statuses, ["ok", "provider_auth"], "doctor hears why it stopped");
	const nextCalls = cb.requests.filter((r) => r.path.endsWith("/cases/next")).length;
	assert.equal(nextCalls, 1, "no more cases after provider_auth");
	assert.equal(cb.outcomes[0]?.tool, "abstain");
	assert.ok(lines.some((l) => l.includes('"error_type":"auth"') && l.includes('"status":401')));
	const sentToClawBridge = cb.requests
		.filter((r) => !r.path.endsWith("/agent/config") && !r.path.endsWith("/agent/renew"))
		.map((r) => JSON.stringify(r.body ?? ""))
		.join("\n");
	for (const secret of [KEY, PROJECT_TOKEN, CREDENTIAL, BINDING_SECRET]) {
		assert.ok(!leaks(secret, lines.join("\n")), "logs");
		assert.ok(!leaks(secret, out.join("")), "stderr");
	}
	assert.ok(!leaks(KEY, sentToClawBridge), "outcome bodies");
});

test("a provider or model that differs from hello's is never served", async () => {
	cb.hello = { model: "another-model" };
	cb.cases.push(sampleCase());
	const { stopped } = await runUntil(() => false);
	assert.equal(stopped, "no_projects", "with no project left to serve, the agent exits");
	assert.ok(lines.some((l) => l.includes('"event":"provider_mismatch"')));
	assert.ok(!cb.requests.some((r) => r.path.endsWith("/cases/next")));
	assert.equal(provider.requests.length, 0);
});

test("project tokens are renewed with the binding secret, never the key", async () => {
	cb.tokenLifetimeMs = 200;
	await runUntil(() => cb.requests.some((r) => r.path.endsWith("/agent/renew")), { renewFloorMs: 50 });
	const renew = cb.requests.find((r) => r.path.endsWith("/agent/renew"));
	assert.equal(renew?.auth, CREDENTIAL);
	assert.deepEqual(renew?.body, { binding_secret: BINDING_SECRET });
	assert.equal(cb.requests.filter((r) => r.path.endsWith("/agent/config")).length, 1, "config is redeemed once");
});

test("a pending_test config runs the setup test and reports active before any case", async () => {
	cb.configStatus = "pending_test";
	provider.plan = [["ping", {}]];
	await runUntil(() => cb.requests.some((r) => r.path.endsWith("/cases/next")));
	assert.deepEqual(cb.testResults, [{ binding_secret: BINDING_SECRET, config_revision: 7, result: "active" }]);
	const paths = cb.requests.map((r) => r.path);
	assert.ok(paths.indexOf("/v1/dispatcher-model/agent/test-result") < paths.indexOf("/v1/dispatcher-model/cases/next"));
	assert.equal(provider.requests.length, 1, "one request to the provider");
});

for (const [status, reason] of [
	[401, "invalid_key"],
	[403, "no_model_access"],
	[404, "no_model_access"],
	[402, "quota"],
	[429, "rate_limited"],
	[500, "provider_error"],
] as const) {
	test(`a setup test that gets ${status} reports ${reason}, and the agent takes no case`, async () => {
		cb.configStatus = "pending_test";
		provider.fail = status;
		const { stopped } = await runUntil(() => false);
		assert.equal(stopped, "setup_test_failed");
		assert.deepEqual(cb.testResults, [{ binding_secret: BINDING_SECRET, config_revision: 7, result: "failed", reason }]);
		assert.ok(!cb.requests.some((r) => r.path.endsWith("/hello") || r.path.endsWith("/cases/next")));
		assert.ok(!leaks(KEY, lines.join("\n")), "the echoed key is not logged");
	});
}

test("a model that answers without calling the tool fails the setup test with no_tool_use", async () => {
	cb.configStatus = "pending_test";
	provider.plan = [];
	const { stopped } = await runUntil(() => false);
	assert.equal(stopped, "setup_test_failed");
	assert.equal((cb.testResults[0] as { reason?: string }).reason, "no_tool_use");
});

test("an unreachable provider host fails the setup test with egress_blocked", async () => {
	cb.configStatus = "pending_test";
	cb.providerBaseUrl = "http://127.0.0.1:9/v1";
	await runUntil(() => false);
	assert.equal((cb.testResults[0] as { reason?: string }).reason, "egress_blocked");
});

test("a chat case is answered end to end with reply, from read-tool facts", async () => {
	provider.plan = [
		["list_decisions", { since_hours: 24, limit: 10 }],
		["reply", { text: "ABC-12 matched its domain by label only, so it was escalated." }],
	];
	cb.cases.push(
		sampleCase({
			case_id: "case_chat",
			kind: "chat",
			permitted_actions: undefined,
			subject: { type: "message", id: "msg_1" },
			message: { from: { session: "lead", principal: "p_lead" }, text: "Why was ABC-12 escalated?" },
		}),
	);
	await runUntil(() => cb.outcomes.length > 0);
	assert.equal(cb.outcomes[0]?.tool, "reply");
	assert.deepEqual(
		cb.requests.filter((r) => r.path.includes("/tools/")).map((r) => r.path.split("/").pop()),
		["list_decisions", "reply"],
	);
});
