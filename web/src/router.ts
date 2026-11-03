// A hash router: #/sessions, #/doctor. Nothing is parsed from the hash but
// the first path segment, and only known ids are ever used.

export const ROUTES = ["overview", "needs-you", "sessions", "projects", "teams", "linear", "dispatcher", "account", "doctor", "settings"] as const;
export type RouteId = (typeof ROUTES)[number];

export function parseRoute(hash: string): RouteId {
  const first = hash.replace(/^#\/?/, "").split(/[/?]/)[0] ?? "";
  return (ROUTES as readonly string[]).includes(first) ? (first as RouteId) : "overview";
}

export function go(id: RouteId): void {
  const target = `#/${id}`;
  if (location.hash !== target) location.hash = target;
}

/** The second path segment (e.g. a session name in #/sessions/NAME), decoded; "" when absent or malformed. */
export function routeArg(hash: string): string {
  const parts = hash.replace(/^#\/?/, "").split("?")[0]?.split("/") ?? [];
  if (!parts[1]) return "";
  try {
    return decodeURIComponent(parts[1]);
  } catch {
    return "";
  }
}
