// Entry point. Inputs: CLAWBRIDGE_SERVER_URL and CLAWBRIDGE_EGRESS_PROXY (env), and the one-time agent credential on
// stdin (design §5.2, §5.3, §11.4). Everything else, the provider key included, comes from the server into memory.
// Nothing leaves the process except through the egress proxy: without one, the agent does not start.
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import { ConfigError, runAgent } from "./agent.ts";
import { ClawBridgeClient, parseServerUrl } from "./client.ts";
import { installEgress, parseEgressProxy, proxyReachable } from "./egress.ts";
import { forbiddenEnv } from "./env-guard.ts";
import { Logger } from "./log.ts";

const CREDENTIAL = /^cba_[A-Za-z0-9_-]{16,256}$/;

/** Reads the credential from stdin once, then closes it. Never from env, argv or a file. */
export async function readCredential(stdin: NodeJS.ReadableStream & { destroy?: () => void }): Promise<string> {
	let raw = "";
	for await (const chunk of stdin) {
		raw += chunk.toString();
		if (raw.length > 1024) break;
	}
	stdin.destroy?.();
	const credential = raw.trim();
	if (!CREDENTIAL.test(credential)) throw new Error("stdin must carry one agent credential (cba_…)");
	return credential;
}

export async function main(env: NodeJS.ProcessEnv = process.env): Promise<number> {
	const logger = new Logger();
	const forbidden = forbiddenEnv(env);
	if (forbidden.length > 0) {
		// Names only, never values.
		process.stderr.write(`refusing to start: provider environment variables are set: ${forbidden.join(", ")}\n`);
		return 2;
	}
	let client: ClawBridgeClient;
	let credential: string;
	try {
		const server = parseServerUrl(env.CLAWBRIDGE_SERVER_URL);
		const proxy = parseEgressProxy(env.CLAWBRIDGE_EGRESS_PROXY);
		if (!(await proxyReachable(proxy))) throw new Error("the egress proxy CLAWBRIDGE_EGRESS_PROXY is unreachable");
		installEgress(proxy);
		client = new ClawBridgeClient(server);
		credential = await readCredential(process.stdin);
	} catch (err) {
		process.stderr.write(`refusing to start: ${(err as Error).message}\n`);
		return 2;
	}
	const shutdown = new AbortController();
	for (const sig of ["SIGTERM", "SIGINT"] as const) process.once(sig, () => shutdown.abort());
	try {
		const { stopped } = await runAgent({ client, logger, credential, instanceId: randomUUID(), signal: shutdown.signal });
		return stopped === "signal" ? 0 : 1;
	} catch (err) {
		if (err instanceof ConfigError) return 1;
		logger.log("crashed");
		return 1;
	}
}

if (process.argv[1] === fileURLToPath(import.meta.url)) process.exitCode = await main();
