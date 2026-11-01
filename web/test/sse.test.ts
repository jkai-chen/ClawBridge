import { describe, expect, it } from "vitest";
import { SseParser } from "../src/sse";

describe("SseParser", () => {
  it("reads events across chunk boundaries and ignores heartbeat comments", () => {
    const p = new SseParser();
    expect(p.push(": connected\n\nevent: sessions.chan")).toEqual([]);
    expect(p.push('ged\ndata: {"a":1}\n\n: heartbeat\n\n')).toEqual([{ event: "sessions.changed", data: '{"a":1}' }]);
  });
  it("joins multi-line data, defaults the name and handles CRLF", () => {
    const p = new SseParser();
    expect(p.push("data: one\r\ndata: two\r\n\r\n")).toEqual([{ event: "message", data: "one\ntwo" }]);
  });
  it("waits for a lone CR that may be half of a CRLF", () => {
    const p = new SseParser();
    expect(p.push("data: x\r")).toEqual([]);
    expect(p.push("\n\r\n")).toEqual([{ event: "message", data: "x" }]);
  });
  it("drops an event with no data", () => {
    expect(new SseParser().push("event: x\n\n")).toEqual([]);
  });
});
