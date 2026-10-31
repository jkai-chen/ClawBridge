// SPIKE: the terminal window. It renders only the PTY's bytes.
import { Call, Events } from "/wails/runtime.js";

const bridge = (method, params) => Call.ByName("main.Bridge.Call", method, params).then((raw) => {
  const m = typeof raw === "string" ? JSON.parse(raw) : raw;
  if (m.error) throw new Error(m.error.message);
  return m.result;
});
const enc = (s) => btoa(String.fromCharCode(...new TextEncoder().encode(s)));
const dec = (b) => Uint8Array.from(atob(b), (c) => c.charCodeAt(0));

const term = new Terminal({ cols: 100, rows: 30 });
term.open(document.getElementById("t"));
let id = null;
Events.On("pty.data", (ev) => {
  const p = JSON.parse(ev.data);
  if (p.id === id) term.write(dec(p.data));
});
Events.On("pty.exit", () => term.write("\r\n[agent exited]\r\n"));
bridge("pty.open", { kind: "claude", dir: ".", cols: term.cols, rows: term.rows })
  .then((r) => { id = r.id; })
  .catch((e) => term.write("pty.open failed: " + e.message + "\r\n"));
term.onData((d) => { if (id) bridge("pty.write", { id, data: enc(d) }); });
term.onResize(({ cols, rows }) => { if (id) bridge("pty.resize", { id, cols, rows }); });
