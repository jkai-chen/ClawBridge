// Egress (design §5.3, §8): every request the agent makes goes through its egress proxy, and nothing reaches the
// network any other way. A recorder under the agent's fence sees every TCP connect that gets through; it must see
// only the proxy. The fake proxy sees the CONNECT targets: only the ClawBridge server and the provider.
import assert from "node:assert/strict";
import dns from "node:dns";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import { after, before, test } from "node:test";
import { createModels } from "@earendil-works/pi-ai";
import { runAgent } from "../src/agent.ts";
import { ClawBridgeClient } from "../src/client.ts";
import { EgressRefused, egressFailure, installEgress, parseEgressProxy } from "../src/egress.ts";
import { Logger, providerFailure } from "../src/log.ts";
import { buildProvider, SUPPORTED_APIS } from "../src/provider.ts";
import { AgentConfigResponse } from "../src/schema.ts";
import { runSetupTest } from "../src/setup-test.ts";
import { CREDENTIAL, FakeClawBridge, ISSUE, sampleCase } from "./fake-clawbridge.ts";
import { FakeProvider } from "./fake-provider.ts";
import { FakeProxy } from "./fake-proxy.ts";

// The recorder sits under the fence: it sees only the connects the fence lets through.
const connects: string[] = [];
const origConnect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function (this: net.Socket, ...args: unknown[]) {
	const first = Array.isArray(args[0]) ? args[0][0] : args[0];
	const o = (typeof first === "object" && first !== null ? first : { port: first, host: args[1] }) as { host?: string; port?: number; path?: string };
	connects.push(o.path ?? `${o.host ?? "localhost"}:${o.port}`);
	return (origConnect as (...a: unknown[]) => net.Socket).apply(this, args);
} as typeof net.Socket.prototype.connect;

let cb: FakeClawBridge;
let provider: FakeProvider;
let proxy: FakeProxy;
let proxyHost: string;
before(async () => {
	cb = await new FakeClawBridge().start();
	provider = await new FakeProvider().start();
	proxy = await new FakeProxy().start();
	proxyHost = new URL(proxy.url).host;
	installEgress(parseEgressProxy(proxy.url));
});
after(async () => {
	await cb.stop();
	await provider.stop();
	await proxy.stop();
});

const host = (url: string) => new URL(url).host;
const onlyProxy = () => assert.deepEqual([...new Set(connects)].filter((c) => c !== proxyHost), [], "a connect bypassed the proxy");

test("a full case loop goes through the proxy, to the server and the provider only", async () => {
	cb.providerBaseUrl = `${provider.url}/v1`;
	provider.plan = [
		["read_issue_facts", { issue_id: ISSUE }],
		["list_sessions", { active_only: true }],
		["decide", { action: "wait", reason_code: "overlap", explanation: "Holder still active." }],
	];
	cb.cases.push(sampleCase({ case_id: "case_a" }), sampleCase({ case_id: "case_b" }));
	const ctrl = new AbortController();
	const running = runAgent({
		client: new ClawBridgeClient(new URL(cb.url)),
		logger: new Logger(() => {}),
		credential: CREDENTIAL,
		instanceId: "inst-egress",
		signal: ctrl.signal,
		pollSeconds: 0,
	});
	const t0 = Date.now();
	while (cb.outcomes.length < 2 && Date.now() - t0 < 10_000) await new Promise((r) => setTimeout(r, 20));
	ctrl.abort();
	await running;

	assert.equal(cb.outcomes.length, 2);
	assert.ok(connects.includes(proxyHost), "the recorder saw the proxy connections");
	onlyProxy();
	assert.deepEqual([...new Set(proxy.targets)].sort(), [host(cb.url), host(provider.url)].sort());
	assert.deepEqual(proxy.plain, [], "plain http is tunnelled too, never forwarded");
});

test("every provider API this agent offers sends its requests through the proxy", async () => {
	for (const api of SUPPORTED_APIS) {
		const before = provider.requests.length;
		await proxy.dropTunnels();
		proxy.targets.length = 0;
		const built = await buildProvider(
			{ id: "fake", api, base_url: `${provider.url}/v1`, model: "fake-model", key: "sk-test-egress-0123456789" },
			{ max_input_tokens_per_run: 2000, max_output_tokens_per_run: 200 },
		);
		const models = createModels();
		models.setProvider(built.provider);
		const result = await runSetupTest(models.streamSimple.bind(models), built.model, 10_000);
		// Whatever the fake answered, the request reached it, through the proxy.
		assert.ok(provider.requests.length > before, `${api}: no request reached the provider`);
		assert.ok(result.result === "active" || result.reason !== "egress_blocked", `${api}: ${JSON.stringify(result)}`);
		assert.ok(proxy.targets.includes(host(provider.url)), `${api}: no tunnel to the provider`);
		assert.deepEqual(proxy.targets.filter((t) => t !== host(provider.url) && t !== host(cb.url)), [], api);
	}
	onlyProxy();
});

test("a provider's retries go through the proxy too", async () => {
	provider.fail = 503;
	try {
		const before = provider.requests.length;
		const built = await buildProvider(
			{ id: "fake", api: "anthropic-messages", base_url: provider.url, model: "fake-model", key: "sk-test-egress-0123456789" },
			{ max_input_tokens_per_run: 2000, max_output_tokens_per_run: 200 },
		);
		const models = createModels();
		models.setProvider(built.provider);
		// The case path's own setting (run-case.ts): pi retries once with backoff; the SDKs' retries are off.
		const stream = models.streamSimple(built.model, { messages: [{ role: "user", content: "hi", timestamp: 0 }] }, { maxRetries: 1, timeoutMs: 20_000 });
		for await (const _ of stream) {
			// drained
		}
		assert.equal(provider.requests.length - before, 2, "one request and one retry");
	} finally {
		provider.fail = null;
	}
	onlyProxy();
});

test("Node's global fetch goes through the proxy", async () => {
	await proxy.dropTunnels();
	proxy.targets.length = 0;
	const res = await fetch(`${provider.url}/nothing`);
	assert.equal(res.status, 404);
	assert.deepEqual(proxy.targets, [host(provider.url)]);
	onlyProxy();
});

test("every other way out is refused: node:http, node:https, node:net, DNS", async () => {
	const failed = (start: (fail: (e: Error) => void) => void) =>
		new Promise<Error>((resolve, reject) => {
			start(resolve);
			setTimeout(() => reject(new Error("no failure within 2 s")), 2_000);
		});
	const errors = [
		await failed((fail) => http.get(provider.url, () => fail(new Error("answered"))).on("error", fail)),
		await failed((fail) => https.get("https://127.0.0.1:1/", () => fail(new Error("answered"))).on("error", fail)),
		await failed((fail) => net.connect(Number(new URL(provider.url).port), "127.0.0.1").on("error", fail)),
		await failed((fail) => dns.lookup("example.com", (e) => fail(e ?? new Error("resolved")))),
		await failed((fail) => dns.resolve4("example.com", (e) => fail(e ?? new Error("resolved")))),
		await failed((fail) => new dns.Resolver().resolve4("example.com", (e) => fail(e ?? new Error("resolved")))),
		await dns.promises.lookup("example.com").then(() => new Error("resolved"), (e: Error) => e),
		await dns.promises.resolveTxt("example.com").then(() => new Error("resolved"), (e: Error) => e),
		await new dns.promises.Resolver().resolve4("example.com").then(() => new Error("resolved"), (e: Error) => e),
	];
	for (const e of errors) assert.ok(e instanceof EgressRefused, String(e));
	// A library on node:http, such as node-fetch (a dependency of the Google SDK's auth), fails the same way.
	const { default: nodeFetch } = await import("node-fetch");
	await assert.rejects(nodeFetch(provider.url), (e: { code?: string; message: string }) => /egress refused/.test(e.message));
	onlyProxy();
});

test("a refused destination and an unreachable proxy are named, never retried around", async () => {
	const client = new ClawBridgeClient(new URL(cb.url));
	await proxy.dropTunnels();
	proxy.refuse.add(host(cb.url));
	const refused = await client.call("POST", "/v1/dispatcher-model/agent/config", CREDENTIAL, AgentConfigResponse, {});
	proxy.refuse.clear();
	assert.deepEqual(refused, { ok: false, status: null, code: "egress_refused" });
	assert.equal(providerFailure("error", "Connection error. Proxy response (403) !== 200 when HTTP Tunneling").error_type, "network");

	await proxy.stop();
	const down = await client.call("POST", "/v1/dispatcher-model/agent/config", CREDENTIAL, AgentConfigResponse, {});
	assert.deepEqual(down, { ok: false, status: null, code: "egress_unavailable" });
	onlyProxy();
});

test("only a proxy that can't be reached is egress_unavailable; a reset tunnel is a network failure [R93-M1]", () => {
	const chain = (code: string) => new TypeError("fetch failed", { cause: Object.assign(new Error("x"), { code }) });
	for (const code of ["ECONNREFUSED", "EHOSTUNREACH", "ENETUNREACH", "ENOTFOUND", "EAI_AGAIN", "UND_ERR_CONNECT_TIMEOUT"]) {
		assert.equal(egressFailure(chain(code)), "egress_unavailable", code);
	}
	for (const code of ["ECONNRESET", "ETIMEDOUT", "EPIPE", "UND_ERR_SOCKET"]) assert.equal(egressFailure(chain(code)), undefined, code);
});
