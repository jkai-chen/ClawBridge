// JSON-line logs of IDs, tool names, codes, counts and durations only (design §5.4). Never prompts, model text,
// untrusted fields, keys, tokens or headers: only the fields below are written, and a string value that isn't
// shaped like an ID or a code is replaced, so free text can't slip into a log line by mistake.
export type LogFields = Partial<Record<(typeof FIELDS)[number], string | number | boolean | null>>;

const FIELDS = [
	"project_id", "case_id", "kind", "tool", "outcome", "code", "reason_code", "status", "error_type", "attempt",
	"input_tokens", "output_tokens", "tool_calls", "duration_ms", "config_revision", "provider", "model", "projects",
	"retry_after",
] as const;
const ALLOWED = new Set<string>(FIELDS);
const SAFE_STRING = /^[A-Za-z0-9_.:-]{0,128}$/;

export type Sink = (line: string) => void;

export class Logger {
	private readonly sink: Sink;
	constructor(sink: Sink = (line) => process.stdout.write(`${line}\n`)) {
		this.sink = sink;
	}

	log(event: string, fields: LogFields = {}): void {
		const out: Record<string, unknown> = { time: new Date().toISOString(), event: safe(event) };
		for (const [k, v] of Object.entries(fields)) {
			if (!ALLOWED.has(k) || v === undefined) continue;
			out[k] = typeof v === "string" ? safe(v) : v;
		}
		this.sink(JSON.stringify(out));
	}
}

function safe(v: string): string {
	return SAFE_STRING.test(v) ? v : "[redacted]";
}

/**
 * A provider failure reduced to what may be recorded: an HTTP status and a fixed error type (DP-L6). The raw error
 * is never kept, since provider bodies and headers can echo the key.
 */
export interface ProviderFailure {
	status: number | null;
	error_type: "auth" | "forbidden" | "not_found" | "rate_limited" | "quota" | "server" | "timeout" | "aborted" | "network" | "other";
}

/** `httpStatus` is the provider response's status when one was seen (pi's onResponse); else it is read from the text. */
export function providerFailure(
	stopReason: string,
	errorMessage: string | undefined,
	httpStatus: number | null = null,
): ProviderFailure {
	if (stopReason === "aborted") return { status: null, error_type: "aborted" };
	const text = errorMessage ?? "";
	const m = /\b([45]\d\d)\b/.exec(text);
	const status = httpStatus !== null && httpStatus >= 400 ? httpStatus : m ? Number(m[1]) : null;
	if (status === 401) return { status, error_type: "auth" };
	if (status === 402) return { status, error_type: "quota" };
	if (status === 403) return { status, error_type: "forbidden" };
	if (status === 404) return { status, error_type: "not_found" };
	if (status === 429) return { status, error_type: /quota|billing|credit/i.test(text) ? "quota" : "rate_limited" };
	if (status !== null && status >= 500) return { status, error_type: "server" };
	if (/timed? ?out|timeout/i.test(text)) return { status, error_type: "timeout" };
	// The provider SDKs report a failed connection as "Connection error." (no status).
	if (/connection error|ECONNREFUSED|ENOTFOUND|ECONNRESET|fetch failed|network/i.test(text)) return { status, error_type: "network" };
	return { status, error_type: "other" };
}
