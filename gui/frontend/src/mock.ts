// Dev-only stand-in for the Go shell (`npm run dev`, open /?mock=ok).
// It is imported only under import.meta.env.DEV, so it is not in the build.
//
//   mock=ok|empty|error|slow|hostile   what the two reads return
//   state=running|starting|stopped|needs-setup|update-needed
//   os=windows                          the Windows + WSL variant

import { fixtures } from "../test/fixtures";
import type { BackendStatus } from "./api";
import type { Transport } from "./transport";

export function mockTransport(q: URLSearchParams): Transport {
  const mode = q.get("mock") || "ok";
  const windows = q.get("os") === "windows";
  const state = (q.get("state") || "running") as BackendStatus["state"];
  const status: BackendStatus = {
    state,
    os: windows ? "windows" : "darwin",
    target: windows ? "Ubuntu (WSL)" : "this computer",
    distro: windows ? "Ubuntu" : undefined,
    client_version: "1.1.0",
    reason: state === "stopped" ? "The ClawBridge client stopped." : state === "needs-setup" ? "No clawbridge client found on PATH. Install ClawBridge, or choose the client file." : state === "update-needed" ? "The client speaks protocol 0; this app needs 1." : undefined,
    code: state === "needs-setup" ? (windows ? "needs-distro" : "no-client") : undefined,
  };
  const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));
  const listeners = new Map<string, Set<(d: unknown) => void>>();
  return {
    async call(method, ...args) {
      switch (method) {
        case "Status":
          return status;
        case "Logs":
          return ["discarded: motd: welcome (mock)", "stderr: backend ready (mock)"];
        case "WSLDistros":
          return ["Ubuntu", "Debian"];
        case "ReportViolation":
        case "Ready":
        case "Restart":
        case "SetWSLDistro":
        case "ChooseClient":
          return "";
        case "Call": {
          if (mode === "slow") await wait(60_000);
          if (mode === "error") throw new Error("the backend returned an error (code -32000)");
          switch (args[0]) {
            case "sessions.list":
              return mode === "empty" ? fixtures.sessionsEmpty : mode === "hostile" ? fixtures.sessionsHostile : fixtures.sessionsList;
            case "doctor.checks":
              return mode === "empty" ? { status: "pass", generated_at: new Date().toISOString(), checks: [] } : fixtures.doctorChecks;
            case "hello":
              return fixtures.hello;
          }
          throw new Error(`method not allowed from the page: ${String(args[0])}`);
        }
      }
      throw new Error(`unknown shell method ${method}`);
    },
    on(event, handler) {
      const set = listeners.get(event) ?? new Set();
      set.add(handler);
      listeners.set(event, set);
      return () => set.delete(handler);
    },
  };
}
