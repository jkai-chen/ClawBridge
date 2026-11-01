// A parser for a server-sent event stream read with fetch() (C13): the page
// cannot use EventSource, which cannot send the session header.

export interface SseEvent {
  event: string;
  data: string;
}

/** Feed it decoded text chunks; it returns the events completed so far. */
export class SseParser {
  private buf = "";
  private event = "";
  private data: string[] = [];

  push(chunk: string): SseEvent[] {
    this.buf += chunk;
    const out: SseEvent[] = [];
    for (;;) {
      const m = /\r\n|\n|\r/.exec(this.buf);
      if (!m) break;
      // A lone "\r" at the very end may be the first half of "\r\n".
      if (m[0] === "\r" && m.index === this.buf.length - 1) break;
      const line = this.buf.slice(0, m.index);
      this.buf = this.buf.slice(m.index + m[0].length);
      if (line === "") {
        if (this.data.length) out.push({ event: this.event || "message", data: this.data.join("\n") });
        this.event = "";
        this.data = [];
      } else if (line.startsWith(":")) {
        // a comment: the server's heartbeat
      } else {
        const i = line.indexOf(":");
        const field = i < 0 ? line : line.slice(0, i);
        let value = i < 0 ? "" : line.slice(i + 1);
        if (value.startsWith(" ")) value = value.slice(1);
        if (field === "event") this.event = value;
        else if (field === "data") this.data.push(value);
      }
    }
    return out;
  }
}
