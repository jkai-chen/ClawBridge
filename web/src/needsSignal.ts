// A tiny signal the shell fires on a needs.changed push, so a mounted view
// can read the queue again.
export interface NeedsSignal {
  subscribe(fn: () => void): () => void;
  fire(): void;
}

export function createNeedsSignal(): NeedsSignal {
  const fns = new Set<() => void>();
  return {
    subscribe: (fn) => {
      fns.add(fn);
      return () => { fns.delete(fn); };
    },
    fire: () => { for (const fn of [...fns]) fn(); },
  };
}
