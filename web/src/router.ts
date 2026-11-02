// A hash router: #/sessions, #/doctor. Nothing is parsed from the hash but
// the first path segment, and only known ids are ever used.

export const ROUTES = ["overview", "needs-you", "sessions", "projects", "teams", "integrations", "dispatcher", "account", "doctor", "settings"] as const;
export type RouteId = (typeof ROUTES)[number];

export function parseRoute(hash: string): RouteId {
  const first = hash.replace(/^#\/?/, "").split(/[/?]/)[0] ?? "";
  return (ROUTES as readonly string[]).includes(first) ? (first as RouteId) : "overview";
}

export function go(id: RouteId): void {
  const target = `#/${id}`;
  if (location.hash !== target) location.hash = target;
}

/** The path segment at `index` (0 is the route id), decoded; "" when absent or malformed. */
export function routeSegment(hash: string, index: number): string {
  const parts = hash.replace(/^#\/?/, "").split("?")[0]?.split("/") ?? [];
  const part = parts[index];
  if (!part) return "";
  try {
    return decodeURIComponent(part);
  } catch {
    return "";
  }
}

/** The second path segment (e.g. a session name in #/sessions/NAME), decoded; "" when absent or malformed. */
export function routeArg(hash: string): string {
  return routeSegment(hash, 1);
}

/** The third path segment (e.g. the tab in #/integrations/linear/binding), decoded; "" when absent or malformed. */
export function routeArg2(hash: string): string {
  return routeSegment(hash, 2);
}
