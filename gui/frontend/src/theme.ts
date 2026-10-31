// The theme follows the OS (GUI-D-SPEC §8). The tokens switch on
// <html data-theme>, set here from prefers-color-scheme.

export function applyTheme(doc: Document = document, win: Window = window): () => void {
  const query = win.matchMedia("(prefers-color-scheme: dark)");
  const set = () => doc.documentElement.setAttribute("data-theme", query.matches ? "dark" : "light");
  set();
  query.addEventListener("change", set);
  return () => query.removeEventListener("change", set);
}
