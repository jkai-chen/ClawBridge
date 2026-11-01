import { beforeEach, describe, expect, it } from "vitest";
import { STORAGE_KEY } from "../src/session";
import { ConnectionError, RpcError, SESSION_HEADER, httpTransport, type ConnState } from "../src/transport";
import { settle } from "./helpers";

const S = "cd".repeat(32);
beforeEach(() => sessionStorage.clear());

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function stream(chunks: string[], hold = false): Response {
  const enc = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      for (const x of chunks) c.enqueue(enc.encode(x));
      if (!hold) c.close();
    },
  });
  return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
}

describe("call", () => {
  it("POSTs JSON-RPC to /rpc with the session header and returns the result", async () => {
    let seen: { url: string; init: RequestInit } | undefined;
    const t = httpTransport({ secret: S, fetch: async (url, init) => { seen = { url: String(url), init: init! }; return json({ jsonrpc: "2.0", id: 1, result: { ok: true } }); } });
    expect(await t.call("sessions.list")).toEqual({ ok: true });
    expect(seen?.url).toBe("/rpc");
    expect(seen?.init.method).toBe("POST");
    const headers = seen?.init.headers as Record<string, string>;
    expect(headers["Content-Type"]).toBe("application/json");
    expect(headers[SESSION_HEADER]).toBe(S);
    expect(JSON.parse(String(seen?.init.body))).toMatchObject({ jsonrpc: "2.0", method: "sessions.list" });
  });
  it("sends no secret in the URL and no credentials option", async () => {
    let init: RequestInit | undefined;
    const t = httpTransport({ secret: S, fetch: async (_u, i) => { init = i; return json({ result: 1 }); } });
    await t.call("hello");
    expect(JSON.stringify(init)).not.toContain("credentials");
  });
  it("maps a 401 to unauthorized and forgets the secret", async () => {
    sessionStorage.setItem(STORAGE_KEY, S);
    const t = httpTransport({ secret: S, fetch: async () => new Response("unauthorized", { status: 401 }) });
    await expect(t.call("hello")).rejects.toMatchObject({ kind: "unauthorized" });
    expect(sessionStorage.getItem(STORAGE_KEY)).toBeNull();
  });
  it("maps a network failure to lost, a missing secret to no-secret", async () => {
    const down = httpTransport({ secret: S, fetch: async () => { throw new TypeError("Failed to fetch"); } });
    await expect(down.call("hello")).rejects.toBeInstanceOf(ConnectionError);
    await expect(down.call("hello")).rejects.toMatchObject({ kind: "lost" });
    let called = false;
    const none = httpTransport({ secret: null, fetch: async () => { called = true; return json({}); } });
    await expect(none.call("hello")).rejects.toMatchObject({ kind: "no-secret" });
    expect(called).toBe(false);
  });
  it("turns a JSON-RPC error into an RpcError", async () => {
    const t = httpTransport({ secret: S, fetch: async () => json({ jsonrpc: "2.0", id: 1, error: { code: -32601, message: "method not found" } }) });
    await expect(t.call("x")).rejects.toEqual(new RpcError("method not found", -32601));
  });
});

describe("events", () => {
  it("reads /events with fetch and the header, and parses the stream", async () => {
    const states: ConnState[] = [];
    const got: string[] = [];
    let seen: { url: string; init: RequestInit } | undefined;
    const t = httpTransport({ secret: S, backoff: [1], sleep: async () => {}, fetch: async (url, init) => { seen = { url: String(url), init: init! }; return stream([": connected\n\nevent: n\ndata: 1\n\n"], true); } });
    const stop = t.events((e) => got.push(`${e.event}:${e.data}`), (s) => states.push(s));
    await settle();
    stop();
    expect(seen?.url).toBe("/events");
    expect((seen?.init.headers as Record<string, string>)[SESSION_HEADER]).toBe(S);
    expect(states[0]).toBe("ok");
    expect(got).toEqual(["n:1"]);
  });
  it("reconnects with backoff after a failure, reporting lost and then ok", async () => {
    const states: ConnState[] = [];
    const waits: number[] = [];
    let n = 0;
    const t = httpTransport({
      secret: S, backoff: [10, 20, 40],
      sleep: async (ms) => { waits.push(ms); },
      fetch: async () => { n += 1; if (n < 4) throw new TypeError("down"); return stream([], true); },
    });
    const stop = t.events(() => {}, (s) => states.push(s));
    await settle();
    stop();
    expect(waits.slice(0, 3)).toEqual([10, 20, 40]);
    expect(states.slice(0, 4)).toEqual(["lost", "lost", "lost", "ok"]);
  });
  it("stops on 401 with unauthorized and does not retry", async () => {
    const states: ConnState[] = [];
    let n = 0;
    const t = httpTransport({ secret: S, sleep: async () => {}, fetch: async () => { n += 1; return new Response("unauthorized", { status: 401 }); } });
    t.events(() => {}, (s) => states.push(s));
    await settle();
    expect(states).toEqual(["unauthorized"]);
    expect(n).toBe(1);
  });
  it("reports no-secret without fetching", async () => {
    const states: ConnState[] = [];
    let called = false;
    httpTransport({ secret: null, fetch: async () => { called = true; return stream([]); } }).events(() => {}, (s) => states.push(s));
    await settle();
    expect(states).toEqual(["no-secret"]);
    expect(called).toBe(false);
  });
});
