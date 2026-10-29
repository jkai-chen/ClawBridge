// All of the agent's network traffic goes through the egress proxy the launcher configures (design §5.3), with no
// direct-dial fallback:
// - Node's global fetch, which the ClawBridge client and the provider SDKs use, becomes undici's fetch on a
//   ProxyAgent. Every request, http or https, opens a CONNECT tunnel through the proxy.
// - Every other path out of the process is fenced: a TCP connect or a DNS lookup for anything but the proxy fails
//   at once with EgressRefused. A library that used node:http, node:https or node:net directly fails the same way,
//   instead of dialling out.
// The proxy URL comes from CLAWBRIDGE_EGRESS_PROXY, which agentd sets; the ambient HTTP(S)_PROXY variables stay
// refused (env-guard.ts).
import dns from "node:dns";
import net from "node:net";
import { FormData, Headers, ProxyAgent, Request, Response, setGlobalDispatcher, fetch as undiciFetch } from "undici";

/** The proxy URL: http://HOST:PORT and nothing else. */
export function parseEgressProxy(raw: string | undefined): URL {
	let url: URL;
	try {
		url = new URL(raw ?? "");
	} catch {
		throw new Error("CLAWBRIDGE_EGRESS_PROXY must be set to the egress proxy, http://HOST:PORT");
	}
	if (url.protocol !== "http:" || !url.port || url.username || url.password || url.pathname !== "/" || url.search || url.hash) {
		throw new Error("CLAWBRIDGE_EGRESS_PROXY must be http://HOST:PORT, with nothing else");
	}
	return url;
}

const bare = (host: string) => host.replace(/^\[(.*)\]$/, "$1");

/** Whether the proxy accepts a TCP connection within timeoutMs. */
export function proxyReachable(proxy: URL, timeoutMs = 5_000): Promise<boolean> {
	return new Promise((resolve) => {
		const socket = net.connect({ host: bare(proxy.hostname), port: Number(proxy.port) });
		const done = (ok: boolean) => {
			socket.destroy();
			resolve(ok);
		};
		socket.setTimeout(timeoutMs, () => done(false));
		socket.once("connect", () => done(true));
		socket.once("error", () => done(false));
	});
}

/** The error every connection or lookup outside the proxy fails with. */
export class EgressRefused extends Error {
	readonly code = "EGRESS_REFUSED";
	constructor() {
		super("egress refused: the agent connects only through its egress proxy");
	}
}

let installed: string | undefined;

/** Routes fetch through the proxy and fences every other connection. Once per process. */
export function installEgress(proxy: URL): void {
	if (installed !== undefined) {
		if (installed !== proxy.href) throw new Error("the egress proxy is already set");
		return;
	}
	const host = bare(proxy.hostname);
	const port = Number(proxy.port);
	const agent = new ProxyAgent({ uri: proxy.href, proxyTunnel: true });
	// Nothing above can be half done: set only once the agent exists, so a failed install can be tried again.
	installed = proxy.href;
	// fetch is undici's own, on the proxy: the same undici as the ProxyAgent, whichever undici this Node bundles.
	// The global dispatcher covers a fetch reference taken earlier, on a Node whose bundled undici reads it.
	setGlobalDispatcher(agent);
	globalThis.fetch = ((input: Parameters<typeof undiciFetch>[0], init?: Parameters<typeof undiciFetch>[1]) =>
		undiciFetch(input, { ...init, dispatcher: agent })) as unknown as typeof fetch;
	// The fetch classes come from the same undici, or `instanceof Headers` checks (pi's retry policy among them)
	// would fail on what this fetch returns.
	Object.assign(globalThis, { Headers, Request, Response, FormData });

	const connect = net.Socket.prototype.connect;
	net.Socket.prototype.connect = function (this: net.Socket, ...args: unknown[]) {
		const first = Array.isArray(args[0]) ? args[0][0] : args[0];
		const o = (typeof first === "object" && first !== null ? first : { port: first, host: args[1] }) as {
			host?: string;
			port?: number | string;
			path?: string;
		};
		if (o.path === undefined && bare(o.host ?? "localhost") === host && Number(o.port) === port) {
			return (connect as (...a: unknown[]) => net.Socket).apply(this, args);
		}
		// Failed like any connection error, on the socket, so callers see an ordinary network error.
		process.nextTick(() => this.destroy(new EgressRefused()));
		return this;
	} as typeof net.Socket.prototype.connect;

	const lookup = dns.lookup;
	dns.lookup = function (this: unknown, hostname: string, ...rest: unknown[]) {
		if (hostname === host) return (lookup as (...a: unknown[]) => void).call(this, hostname, ...rest);
		const callback = rest.at(-1) as (err: Error) => void;
		process.nextTick(() => callback(new EgressRefused()));
	} as typeof dns.lookup;
	const promisesLookup = dns.promises.lookup;
	dns.promises.lookup = (async (hostname: string, ...rest: unknown[]) => {
		if (hostname === host) return (promisesLookup as (...a: unknown[]) => unknown)(hostname, ...rest);
		throw new EgressRefused();
	}) as typeof dns.promises.lookup;
	// The resolver functions query DNS servers directly; the agent needs none of them.
	for (const name of Object.keys(dns.promises) as (keyof typeof dns.promises)[]) {
		if (name.startsWith("resolve") || name === "reverse") {
			(dns.promises as Record<string, unknown>)[name] = async () => {
				throw new EgressRefused();
			};
		}
	}
	// So do Resolver objects, which talk to DNS servers without node:net.
	for (const proto of [dns.Resolver.prototype, dns.promises.Resolver.prototype] as unknown as Record<string, unknown>[]) {
		for (const name of Object.getOwnPropertyNames(proto)) {
			if (name.startsWith("resolve") || name === "reverse") {
				proto[name] = function (...args: unknown[]) {
					const callback = args.at(-1);
					if (typeof callback !== "function") return Promise.reject(new EgressRefused());
					process.nextTick(() => (callback as (err: Error) => void)(new EgressRefused()));
				};
			}
		}
	}
	for (const name of Object.keys(dns) as (keyof typeof dns)[]) {
		if (name.startsWith("resolve") || name === "reverse") {
			(dns as Record<string, unknown>)[name] = (...args: unknown[]) => {
				const callback = args.at(-1) as (err: Error) => void;
				process.nextTick(() => callback(new EgressRefused()));
			};
		}
	}
}

// Every connection is to the proxy, so one that can't be opened means the proxy is unreachable: refused, no route,
// its name not resolving, or the connect timing out. A reset or a timeout on an open tunnel is not one of these: the
// request may have reached the server, and it stays an ordinary network failure.
const UNREACHABLE = new Set([
	"ECONNREFUSED", "EHOSTUNREACH", "ENETUNREACH", "ENOTFOUND", "EAI_AGAIN", "UND_ERR_CONNECT_TIMEOUT",
]);

/** Whether a fetch failure came from the egress proxy: unreachable before any tunnel, or refusing the destination. */
export function egressFailure(err: unknown): "egress_unavailable" | "egress_refused" | undefined {
	for (let e = err as { message?: string; code?: string; cause?: unknown } | undefined; e; e = e.cause as typeof e) {
		if (typeof e.message === "string" && e.message.startsWith("Proxy response (")) return "egress_refused";
		if (e.code === "EGRESS_REFUSED") return "egress_refused";
		if (e.code && UNREACHABLE.has(e.code)) return "egress_unavailable";
	}
	return undefined;
}
