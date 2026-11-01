// The session secret (GUI-DESIGN §12.3 C3). The server puts it in a one-time
// <meta name="cb-session"> of the page opened with a launch token. It is
// moved into sessionStorage (isolated by origin, so by port), the meta is
// removed and the address is reset to "/". A reload reads sessionStorage; a
// new tab has neither and shows "Open this from clawbridge ui".

export const STORAGE_KEY = "cb-session";

export interface SecretEnv {
  doc: Document;
  store: Pick<Storage, "getItem" | "setItem" | "removeItem">;
  history: Pick<History, "replaceState">;
}

export function takeSecret(env?: Partial<SecretEnv>): string | null {
  const doc = env?.doc ?? document;
  const store = env?.store ?? sessionStorage;
  const hist = env?.history ?? window.history;
  const meta = doc.querySelector('meta[name="cb-session"]');
  if (meta) {
    const secret = meta.getAttribute("content") ?? "";
    meta.remove();
    if (secret) {
      store.setItem(STORAGE_KEY, secret);
      hist.replaceState(null, "", "/");
      return secret;
    }
  }
  try {
    return store.getItem(STORAGE_KEY) || null;
  } catch {
    return null;
  }
}

export function forgetSecret(store: Pick<Storage, "removeItem"> = sessionStorage): void {
  try {
    store.removeItem(STORAGE_KEY);
  } catch {
    /* storage blocked: nothing to forget */
  }
}
