// The only file that knows how the page reaches the Go shell. In the app it
// is the Wails runtime, which the shell's asset server serves from
// /wails/runtime.js (same origin, so the CSP allows it). In `vite dev` a
// fixture transport stands in, so the UI can be worked on without a window.

export interface Transport {
  /** Calls a method of the Go Shell service. */
  call(method: string, ...args: unknown[]): Promise<unknown>;
  /** Subscribes to an event the shell emits; returns the unsubscribe. */
  on(event: string, handler: (data: unknown) => void): () => void;
}

interface WailsRuntime {
  Call: { ByName(name: string, ...args: unknown[]): Promise<unknown> };
  Events: {
    On(name: string, cb: (event: { data: unknown }) => void): () => void;
  };
}

const RUNTIME_URL = "/wails/runtime.js";

export async function wailsTransport(): Promise<Transport> {
  const rt = (await import(/* @vite-ignore */ RUNTIME_URL)) as WailsRuntime;
  return {
    call: (method, ...args) => rt.Call.ByName(`main.Shell.${method}`, ...args),
    on: (event, handler) => rt.Events.On(event, (e) => handler(e.data)),
  };
}

export async function defaultTransport(): Promise<Transport> {
  if (import.meta.env.DEV && new URLSearchParams(location.search).has("mock")) {
    const { mockTransport } = await import("./mock");
    return mockTransport(new URLSearchParams(location.search));
  }
  return wailsTransport();
}
