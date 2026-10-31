// SPIKE: the main window. Untrusted text goes into the DOM only as
// textContent, never as HTML.
import { Call } from "/wails/runtime.js";

const bridge = (name, ...args) => Call.ByName("main.Bridge." + name, ...args);
const log = (s) => { document.getElementById("log").textContent += s + "\n"; };
const reply = (raw) => {
  const m = typeof raw === "string" ? JSON.parse(raw) : raw;
  if (m.error) throw new Error(m.error.message);
  return m.result;
};
const item = (list, text, cls) => {
  const li = document.createElement("li");
  li.textContent = text;
  if (cls) li.className = cls;
  document.getElementById(list).appendChild(li);
};

async function load() {
  for (const l of await bridge("Logs")) log(l);
  try {
    const h = reply(await bridge("Call", "hello", {}));
    document.getElementById("hello").textContent = `${h.platform} · ${h.mode || "no profile"} · client ${h.client_version}`;
    const s = reply(await bridge("Call", "sessions.list", {}));
    if (!s.sessions.length) item("sessions", "No sessions yet.");
    for (const x of s.sessions) item("sessions", `${x.name} ${x.state || ""}`);
    const d = reply(await bridge("Call", "doctor.checks", {}));
    for (const c of d.checks) item("doctor", `${c.status.toUpperCase()}  ${c.title}: ${c.message}`, c.status);
  } catch (e) {
    log("error: " + e.message);
  }
}

document.getElementById("term").onclick = () => bridge("OpenTerminal").catch((e) => log(e.message));
document.getElementById("bad").onclick = () =>
  bridge("Call", "pty.open", { kind: "claude", dir: "." })
    .then(() => log("UNEXPECTED: pty.open from main was allowed"))
    .catch((e) => log("refused as designed: " + e.message));
load();
