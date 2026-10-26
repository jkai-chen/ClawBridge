// The agent's life: redeem the one-time credential for the key-holder's config (§11.4), keep the project tokens
// renewed, and for each project check hello and pull cases one at a time. Projects are independent: a failure in
// one doesn't stop the others. A provider auth failure stops them all, since they share the one key (design §7).
import type { StreamFn } from "@earendil-works/pi-agent-core";
import { createModels } from "@earendil-works/pi-ai";
import type { Static } from "typebox";
import type { ClawBridgeClient } from "./client.ts";
import type { Logger } from "./log.ts";
import { buildProvider, type BuiltProvider } from "./provider.ts";
import { runCase } from "./run-case.ts";
import {
	AgentConfigResponse,
	AgentRenewResponse,
	CasesNextResponse,
	HelloResponse,
} from "./schema.ts";

type Config = Static<typeof AgentConfigResponse>;
type ProjectTokens = Config["projects"];

export interface AgentOptions {
	client: ClawBridgeClient;
	logger: Logger;
	credential: string;
	instanceId: string;
	signal: AbortSignal;
	/** For tests: a provider other than the configured one (the faux provider). */
	provider?: BuiltProvider;
	pollSeconds?: number;
	idleRetryMs?: number;
	marginMs?: number;
	/** The shortest wait between renewals (tests shorten it). */
	renewFloorMs?: number;
}

export class ConfigError extends Error {
	readonly code: string;
	constructor(code: string) {
		super(`agent config failed: ${code}`);
		this.code = code;
	}
}

export async function runAgent(o: AgentOptions): Promise<{ stopped: string }> {
	const cfg = await o.client.call("POST", "/v1/dispatcher-model/agent/config", o.credential, AgentConfigResponse, {
		instance_id: o.instanceId,
	});
	if (!cfg.ok) {
		o.logger.log("config_failed", { code: cfg.code, status: cfg.status });
		throw new ConfigError(cfg.code);
	}
	const config = cfg.body;
	const built = o.provider ?? (await buildProvider(config.provider, config.limits));
	const models = createModels();
	models.setProvider(built.provider);
	const streamFn: StreamFn = models.streamSimple.bind(models);
	o.logger.log("configured", {
		config_revision: config.config_revision,
		provider: config.provider.id,
		model: config.provider.model,
		projects: config.projects.length,
	});

	const tokens = new Map<string, string>();
	const setTokens = (list: ProjectTokens) => {
		for (const p of list) tokens.set(p.project_id, p.token);
	};
	setTokens(config.projects);
	let expiries = config.projects.map((p) => Date.parse(p.expires_at));

	const stop = new AbortController();
	const signal = AbortSignal.any([o.signal, stop.signal]);
	let stopped = "signal";
	const stopAll = (why: string) => {
		stopped = why;
		stop.abort();
	};

	const renewLoop = async () => {
		let failures = 0;
		while (!signal.aborted) {
			// Renew at half the remaining life of the soonest token (1 h tokens: about every 30 min).
			const soonest = Math.min(...expiries, Date.now() + 3_600_000);
			const wait = failures > 0 ? Math.min(60_000 * failures, 300_000) : Math.max(o.renewFloorMs ?? 10_000, (soonest - Date.now()) / 2);
			await sleep(wait, signal);
			if (signal.aborted) return;
			const r = await o.client.call("POST", "/v1/dispatcher-model/agent/renew", o.credential, AgentRenewResponse, {
				binding_secret: config.binding_secret,
			});
			if (r.ok) {
				failures = 0;
				setTokens(r.body.projects);
				expiries = r.body.projects.map((p) => Date.parse(p.expires_at));
				o.logger.log("renewed", { projects: r.body.projects.length });
			} else {
				failures++;
				o.logger.log("renew_failed", { code: r.code, status: r.status, attempt: failures });
				if (r.code === "unauthorized") return stopAll("credential_revoked");
			}
		}
	};

	const projectLoop = async (projectId: string) => {
		const token = () => tokens.get(projectId) ?? "";
		const log = (event: string, fields: Parameters<Logger["log"]>[1] = {}) =>
			o.logger.log(event, { project_id: projectId, ...fields });
		const idle = o.idleRetryMs ?? 60_000;
		while (!signal.aborted) {
			const hello = await o.client.call("GET", "/v1/dispatcher-model/hello", token(), HelloResponse);
			if (!hello.ok) {
				log("hello_failed", { code: hello.code, status: hello.status });
				if (hello.code === "unauthorized" && !tokens.has(projectId)) return;
				await sleep(hello.code === "server_busy" ? (hello.retryAfter ?? 5) * 1000 : idle, signal);
				continue;
			}
			const h = hello.body;
			// The owner's settings and this agent's config must agree (d4 §6); a mismatch is never served.
			if (h.provider !== config.provider.id || h.model !== config.provider.model) {
				log("provider_mismatch", { code: "provider_mismatch" });
				return;
			}
			if (!h.enabled) {
				await sleep(idle, signal);
				continue;
			}
			const helloAt = Date.now();
			while (!signal.aborted && Date.now() - helloAt < 300_000) {
				const next = await o.client.call(
					"POST",
					"/v1/dispatcher-model/cases/next",
					token(),
					CasesNextResponse,
					{ wait_seconds: o.pollSeconds ?? 30 },
					{ timeoutMs: ((o.pollSeconds ?? 30) + 10) * 1000, signal },
				);
				if (!next.ok) {
					log("next_failed", { code: next.code, status: next.status });
					await sleep(next.code === "server_busy" ? (next.retryAfter ?? 5) * 1000 : 5_000, signal);
					break; // back to hello: the project may have been turned off
				}
				const c = next.body.case;
				if (!c) continue;
				log("case", { case_id: c.case_id, kind: c.kind });
				const result = await runCase(c, h.opt_in_tools, {
					client: o.client,
					token,
					streamFn,
					model: built.model,
					limits: config.limits,
					logger: o.logger,
					projectId,
					marginMs: o.marginMs,
				}).catch(() => {
					// The server falls back to rules at the deadline; the loop goes on with the next case.
					log("case_failed", { case_id: c.case_id });
					return { providerAuth: false };
				});
				if (result.providerAuth) {
					log("provider_auth", { code: "provider_auth" });
					return stopAll("provider_auth");
				}
			}
		}
	};

	const renewing = renewLoop();
	await Promise.all(
		config.projects.map((p) =>
			projectLoop(p.project_id).catch(() => o.logger.log("project_loop_failed", { project_id: p.project_id })),
		),
	);
	// Every project loop ended without a shutdown: nothing left to serve (e.g. all mismatched).
	if (stopped === "signal" && !o.signal.aborted) stopped = "no_projects";
	stop.abort();
	await renewing;
	o.logger.log("stopped", { code: stopped });
	return { stopped };
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
	return new Promise((resolve) => {
		if (signal.aborted) return resolve();
		const t = setTimeout(done, ms);
		function done() {
			clearTimeout(t);
			signal.removeEventListener("abort", done);
			resolve();
		}
		signal.addEventListener("abort", done, { once: true });
	});
}
