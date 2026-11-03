import { Api, parseHello } from "../src/api";
import { ConnectionError, type ConnState, type Transport } from "../src/transport";
import type { ViewCtx } from "../src/views/common";
import { fixtures, fullHello } from "./fixtures";

export interface FakeServer {
  transport: Transport;
  calls: { method: string; params: unknown }[];
  /** Pushes a connection state to the page, as the event stream does. */
  setState(s: ConnState): void;
}

/** A transport whose answers come from `answers[rpcMethod]`; hello defaults to the full one. */
export function fakeServer(answers: Record<string, () => unknown | Promise<unknown>>): FakeServer {
  const calls: FakeServer["calls"] = [];
  let onState: (s: ConnState) => void = () => {};
  const all: Record<string, () => unknown | Promise<unknown>> = { hello: () => fullHello, ...answers };
  const transport: Transport = {
    async call(method, params) {
      calls.push({ method, params });
      const fn = all[method];
      if (!fn) throw new Error(`method not found: ${method}`);
      return fn();
    },
    events(_onEvent, state) {
      onState = state;
      queueMicrotask(() => state("ok"));
      return () => {};
    },
  };
  return { transport, calls, setState: (s) => onState(s) };
}

export const apiFor = (s: FakeServer) => new Api(s.transport);

export const HELLO = parseHello(fullHello);
export const lost = () => { throw new ConnectionError("lost"); };

export function ctxFor(api: Api, root: HTMLElement): ViewCtx & { badges: Record<string, number>; seen: { connection: string[]; offline: (string | null)[] } } {
  const badges: Record<string, number> = {};
  const seen = { connection: [] as string[], offline: [] as (string | null)[] };
  return {
    root, api, hello: HELLO, isCurrent: () => true, go: () => {}, badges, seen,
    setBadge: (n, v) => { badges[n] = v; },
    connection: (k) => { seen.connection.push(k); },
    setOffline: (at) => { seen.offline.push(at); },
  };
}

export { fixtures };

/** Lets pending promises settle. */
export const settle = async () => {
  for (let i = 0; i < 8; i++) await new Promise((r) => setTimeout(r, 0));
};
