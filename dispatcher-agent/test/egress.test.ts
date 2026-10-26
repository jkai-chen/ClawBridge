// Egress (design §8): over a full case loop the agent connects to the ClawBridge server and the provider host, and
// nothing else. Every TCP connect and DNS lookup in this process is recorded; anything else is refused.
// (The container's egress proxy is the boundary in production; this proves the code asks for nothing more.)
import assert from "node:assert/strict";
import dns from "node:dns";
import net from "node:net";
import { after, before, test } from "node:test";
import { runAgent } from "../src/agent.ts";
import { ClawBridgeClient } from "../src/client.ts";
import { Logger } from "../src/log.ts";
import { CREDENTIAL, FakeClawBridge, ISSUE, sampleCase } from "./fake-clawbridge.ts";
import { FakeProvider } from "./fake-provider.ts";

const connects: string[] = [];
const lookups: string[] = [];
const origConnect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function (this: net.Socket, ...args: unknown[]) {
	const first = Array.isArray(args[0]) ? args[0][0] : args[0];
	const o = (typeof first === "object" && first !== null ? first : { port: first, host: args[1] }) as {
		host?: string;
		port?: number;
		path?: string;
	};
	if (o.path === undefined) {
		const host = o.host ?? "localhost";
		connects.push(`${host}:${o.port}`);
		if (host !== "127.0.0.1") throw new Error(`egress refused: ${host}`);
	}
	return (origConnect as (...a: unknown[]) => net.Socket).apply(this, args);
} as typeof net.Socket.prototype.connect;
const origLookup = dns.lookup;
const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);
dns.lookup = function (this: unknown, host: string, ...rest: unknown[]) {
	if (LOOPBACK.has(host)) return (origLookup as (...a: unknown[]) => void).call(this, host, ...rest);
	lookups.push(host);
	// Refused without a real query, so the negative control sends nothing either.
	const callback = rest.at(-1) as (err: Error) => void;
	process.nextTick(() => callback(Object.assign(new Error(`egress refused: ${host}`), { code: "ENOTFOUND" })));
} as typeof dns.lookup;

let cb: FakeClawBridge;
let provider: FakeProvider;
before(async () => {
	cb = await new FakeClawBridge().start();
	provider = await new FakeProvider().start();
});
after(async () => {
	await cb.stop();
	await provider.stop();
});

test("a full case loop connects only to the server and the provider", async () => {
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
	const allowed = new Set([new URL(cb.url).host, new URL(provider.url).host]);
	assert.ok(connects.length > 0, "the hook saw the connections");
	assert.deepEqual([...new Set(connects)].filter((c) => !allowed.has(c)), []);
	assert.deepEqual(lookups, [], "no DNS lookups beyond loopback");
});

test("negative control: the hook records and refuses any other host", async () => {
	await assert.rejects(fetch("https://example.com"));
	assert.ok(lookups.includes("example.com") || connects.some((c) => c.startsWith("example.com")));
});
