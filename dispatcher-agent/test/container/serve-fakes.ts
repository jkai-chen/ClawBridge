// The fakes for the container test (scripts/container-test.sh): a fake ClawBridge with N decide cases, each with a
// 4 KiB untrusted excerpt, a fake provider that answers each case with two reads and a decide, and a fake egress
// proxy the agent must reach both through. Prints one JSON line when every case has an outcome, then exits.
import { FakeClawBridge, ISSUE, sampleCase } from "../fake-clawbridge.ts";
import { FakeProvider } from "../fake-provider.ts";
import { FakeProxy } from "../fake-proxy.ts";

const cases = Number(process.argv[2] ?? 1000);
const provider = await new FakeProvider().start(18081);
provider.plan = [
	["read_issue_facts", { issue_id: ISSUE }],
	["list_domain", { domain: "payments" }],
	["decide", { action: "escalate", reason_code: "untrusted", explanation: "Matched by label only." }],
];
const cb = await new FakeClawBridge().start(18080);
const proxy = await new FakeProxy().start(18082);
cb.providerBaseUrl = `${provider.url}/v1`;
const excerpt = "Untrusted description text. ".repeat(160).slice(0, 4096);
for (let i = 0; i < cases; i++) {
	cb.cases.push(sampleCase({ case_id: `case_${i}`, untrusted: { title: `Issue ${i}`, description_excerpt: excerpt, truncated: true } }));
}
console.log(JSON.stringify({ ready: true, cases }));
const t0 = Date.now();
const timer = setInterval(() => {
	if (cb.outcomes.length < cases) return;
	clearInterval(timer);
	const tools = cb.outcomes.map((o) => o.tool);
	console.log(
		JSON.stringify({
			done: true,
			outcomes: cb.outcomes.length,
			decide: tools.filter((t) => t === "decide").length,
			violations: cb.violations.length,
			tunnels: [...new Set(proxy.targets)].sort(),
			plain: proxy.plain.length,
			seconds: Math.round((Date.now() - t0) / 1000),
		}),
	);
	void Promise.all([cb.stop(), provider.stop(), proxy.stop()]).then(() => process.exit(0));
}, 200);
