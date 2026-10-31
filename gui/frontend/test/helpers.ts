import { Api, type BackendStatus } from "../src/api";
import type { Transport } from "../src/transport";

export const RUNNING: BackendStatus = { state: "running", os: "darwin", target: "this computer", client_version: "1.1.0" };

export interface FakeShell {
  transport: Transport;
  calls: { method: string; args: unknown[] }[];
  emit(event: string, data: unknown): void;
}

/** A transport whose Shell.Call answers come from `answers[rpcMethod]`. */
export function fakeShell(
  answers: Record<string, () => unknown | Promise<unknown>>,
  status: BackendStatus = RUNNING,
): FakeShell {
  const calls: FakeShell["calls"] = [];
  const handlers = new Map<string, Set<(d: unknown) => void>>();
  const transport: Transport = {
    async call(method, ...args) {
      calls.push({ method, args });
      if (method === "Status") return status;
      if (method === "Call") {
        const fn = answers[String(args[0])];
        if (!fn) throw new Error(`method not allowed from the page: ${String(args[0])}`);
        return fn();
      }
      if (method === "Logs") return ["discarded: motd"];
      if (method === "WSLDistros") return ["Ubuntu", "Debian"];
      return undefined;
    },
    on(event, handler) {
      const set = handlers.get(event) ?? new Set();
      set.add(handler);
      handlers.set(event, set);
      return () => set.delete(handler);
    },
  };
  return {
    transport,
    calls,
    emit: (event, data) => handlers.get(event)?.forEach((h) => h(data)),
  };
}

export const apiFor = (s: FakeShell) => new Api(s.transport);

/** Lets pending promises settle. */
export const settle = async () => {
  for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0));
};
