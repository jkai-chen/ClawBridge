// The only file that talks to the server: POST /rpc and a fetch-streamed
// GET /events, both with the session secret in X-ClawBridge-Session. There is
// no EventSource (it cannot send the header; C13) and no cookie.
import { forgetSecret } from "./session";
import { SseParser, type SseEvent } from "./sse";

export const SESSION_HEADER = "X-ClawBridge-Session";

/** Why the server cannot be used: no secret, a refused secret, or no answer. */
export type ConnKind = "no-secret" | "unauthorized" | "lost";

export class ConnectionError extends Error {
  constructor(readonly kind: ConnKind) {
    super(kind);
    this.name = "ConnectionError";
  }
}

/** A JSON-RPC error answer. */
export class RpcError extends Error {
  constructor(message: string, readonly code: number) {
    super(message);
    this.name = "RpcError";
  }
}

export type ConnState = "connecting" | "ok" | ConnKind;

export interface Transport {
  /** Resolves to the JSON-RPC result. Rejects with RpcError or ConnectionError. */
  call(method: string, params?: Record<string, unknown>): Promise<unknown>;
  /** Streams events until the returned function is called. Reconnects with backoff. */
  events(onEvent: (e: SseEvent) => void, onState: (s: ConnState) => void): () => void;
}

type Fetch = typeof fetch;

export interface HttpOptions {
  secret: string | null;
  fetch?: Fetch;
  /** Backoff steps in ms for the event stream. */
  backoff?: readonly number[];
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
}

const BACKOFF = [1000, 2000, 4000, 8000, 15000] as const;

function sleepFor(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => { clearTimeout(t); resolve(); }, { once: true });
  });
}

export function httpTransport(opts: HttpOptions): Transport {
  const doFetch: Fetch = opts.fetch ?? ((...a) => fetch(...a));
  const backoff = opts.backoff ?? BACKOFF;
  const sleep = opts.sleep ?? sleepFor;
  const secret = opts.secret;
  let nextId = 1;

  const fail401 = (): ConnectionError => {
    forgetSecret();
    return new ConnectionError("unauthorized");
  };

  return {
    async call(method, params = {}) {
      if (!secret) throw new ConnectionError("no-secret");
      let res: Response;
      try {
        res = await doFetch("/rpc", {
          method: "POST",
          headers: { "Content-Type": "application/json", [SESSION_HEADER]: secret },
          body: JSON.stringify({ jsonrpc: "2.0", id: nextId++, method, params }),
        });
      } catch {
        throw new ConnectionError("lost");
      }
      if (res.status === 401) throw fail401();
      if (!res.ok) throw new RpcError(`The ClawBridge UI answered ${res.status}.`, -res.status);
      let body: unknown;
      try {
        body = await res.json();
      } catch {
        throw new RpcError("The ClawBridge UI sent an answer this page cannot read.", -32700);
      }
      const r = (body && typeof body === "object" ? body : {}) as { result?: unknown; error?: { code?: number; message?: string } };
      if (r.error) throw new RpcError(String(r.error.message ?? "error"), Number(r.error.code ?? -32603));
      return r.result;
    },

    events(onEvent, onState) {
      const stop = new AbortController();
      void (async () => {
        if (!secret) {
          onState("no-secret");
          return;
        }
        let attempt = 0;
        while (!stop.signal.aborted) {
          try {
            const res = await doFetch("/events", { headers: { [SESSION_HEADER]: secret }, signal: stop.signal, cache: "no-store" });
            if (res.status === 401) {
              forgetSecret();
              onState("unauthorized");
              return;
            }
            if (!res.ok || !res.body) throw new Error(`events ${res.status}`);
            attempt = 0;
            onState("ok");
            const reader = res.body.getReader();
            const dec = new TextDecoder();
            const parser = new SseParser();
            for (;;) {
              const { done, value } = await reader.read();
              if (done) break;
              for (const ev of parser.push(dec.decode(value, { stream: true }))) onEvent(ev);
            }
          } catch {
            if (stop.signal.aborted) return;
          }
          if (stop.signal.aborted) return;
          onState("lost");
          await sleep(backoff[Math.min(attempt, backoff.length - 1)] ?? 15000, stop.signal);
          attempt += 1;
        }
      })();
      return () => stop.abort();
    },
  };
}
