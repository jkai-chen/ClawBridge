// The agent's only connection to ClawBridge: JSON over HTTPS under /v1/dispatcher-model/ (d4 §1).
// Results carry codes, never response text, and a failure never includes the bearer token or a request body.
import type { Static, TSchema } from "typebox";
import { Value } from "typebox/value";
import { API_VERSION, ErrorResponse } from "./schema.ts";

export type ClientResult<T> =
	| { ok: true; status: number; body: T }
	| { ok: false; status: number | null; code: string; retryAfter?: number };

const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]"]);

/** The server URL, refused unless HTTPS or loopback [A8]. */
export function parseServerUrl(raw: string | undefined): URL {
	let url: URL;
	try {
		url = new URL(raw ?? "");
	} catch {
		throw new Error("CLAWBRIDGE_SERVER_URL must be an absolute URL");
	}
	if (url.protocol !== "https:" && !(url.protocol === "http:" && LOOPBACK.has(url.hostname))) {
		throw new Error("CLAWBRIDGE_SERVER_URL must use https (plain http only for localhost)");
	}
	if (url.username || url.password || url.search || url.hash) {
		throw new Error("CLAWBRIDGE_SERVER_URL must not carry credentials, a query or a fragment");
	}
	return url;
}

export class ClawBridgeClient {
	private readonly base: URL;
	private readonly fetchImpl: typeof fetch;

	constructor(base: URL, fetchImpl: typeof fetch = fetch) {
		this.base = base;
		this.fetchImpl = fetchImpl;
	}

	async call<S extends TSchema>(
		method: "GET" | "POST",
		path: string,
		bearer: string,
		responseSchema: S,
		body?: unknown,
		options: { timeoutMs?: number; signal?: AbortSignal } = {},
	): Promise<ClientResult<Static<S>>> {
		const url = new URL(path.replace(/^\//, ""), this.base.href.endsWith("/") ? this.base : `${this.base.href}/`);
		const signals = [AbortSignal.timeout(options.timeoutMs ?? 15_000)];
		if (options.signal) signals.push(options.signal);
		let res: Response;
		try {
			res = await this.fetchImpl(url, {
				method,
				headers: {
					authorization: `Bearer ${bearer}`,
					"clawbridge-model-api": String(API_VERSION),
					...(body === undefined ? {} : { "content-type": "application/json" }),
				},
				body: body === undefined ? undefined : JSON.stringify(body),
				signal: AbortSignal.any(signals),
				redirect: "error",
			});
		} catch (err) {
			const name = (err as { name?: string }).name;
			return { ok: false, status: null, code: name === "TimeoutError" || name === "AbortError" ? "timeout" : "network" };
		}
		let parsed: unknown;
		try {
			parsed = await res.json();
		} catch {
			parsed = undefined;
		}
		if (res.ok) {
			if (!Value.Check(responseSchema, parsed)) return { ok: false, status: res.status, code: "bad_response" };
			return { ok: true, status: res.status, body: parsed as Static<S> };
		}
		if (Value.Check(ErrorResponse, parsed)) {
			const e = parsed as Static<typeof ErrorResponse>;
			return { ok: false, status: res.status, code: e.error, retryAfter: e.retry_after };
		}
		return { ok: false, status: res.status, code: `http_${res.status}` };
	}
}

