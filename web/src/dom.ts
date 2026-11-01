// h() builds elements for the whole UI. It never parses markup: strings
// become text nodes, `text` becomes textContent, and attribute names that
// run script or load markup are refused. Text written by other people
// (Linear titles, messages, session notes) reaches the page only through
// this file (GUI-DESIGN §7, GUI-D-SPEC §9).

export type Child = Node | string | number | null | undefined | false | readonly Child[];

export interface Props {
  class?: string;
  /** Sets textContent. */
  text?: string;
  /** Applied through the CSSOM (style.cssText), never as an attribute. */
  style?: string;
  on?: Record<string, (event: Event) => void>;
  [attr: string]: unknown;
}

const REFUSED_ATTRS = new Set(["srcdoc", "innerhtml", "outerhtml", "style"]);
const URL_ATTRS = new Set(["href", "src", "action", "formaction", "xlink:href"]);

function checkAttr(name: string, value: string): void {
  const lower = name.toLowerCase();
  if (lower.startsWith("on")) throw new Error(`h(): event-handler attribute ${name} is refused; use the "on" prop`);
  if (REFUSED_ATTRS.has(lower)) throw new Error(`h(): attribute ${name} is refused`);
  if (URL_ATTRS.has(lower) && !/^(#|\.\/|\/(?![/\\]))/.test(value)) {
    throw new Error(`h(): ${name} must point inside the app`);
  }
}

function append(parent: Node, child: Child): void {
  if (child === null || child === undefined || child === false) return;
  if (Array.isArray(child)) {
    for (const c of child) append(parent, c);
    return;
  }
  parent.appendChild(child instanceof Node ? child : document.createTextNode(String(child)));
}

export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props?: Props | null,
  ...kids: Child[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props ?? {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key === "class") el.className = String(value);
    else if (key === "text") el.textContent = String(value);
    else if (key === "style") el.style.cssText = String(value);
    else if (key === "on") {
      for (const [event, fn] of Object.entries(value as Record<string, EventListener>)) el.addEventListener(event, fn);
    } else {
      const text = value === true ? "" : String(value);
      checkAttr(key, text);
      el.setAttribute(key, text);
    }
  }
  for (const kid of kids) append(el, kid);
  return el;
}

/** Replaces all of parent's children. */
export function setChildren(parent: Element, ...kids: Child[]): void {
  parent.replaceChildren();
  for (const kid of kids) append(parent, kid);
}

/** Text with `code` spans: the pieces become text nodes and <code> elements. */
export function rich(source: string): Node[] {
  const out: Node[] = [];
  for (const part of source.split(/(`[^`]+`)/)) {
    if (!part) continue;
    out.push(part.startsWith("`") && part.endsWith("`") ? h("code", { text: part.slice(1, -1) }) : document.createTextNode(part));
  }
  return out;
}

// C0 except tab and newline, DEL, C1, the Arabic letter mark, zero-width and
// bidi marks, line/paragraph separators, bidi embeds/overrides/isolates, BOM,
// interlinear annotation marks, and the invisible Unicode tag characters.
// eslint-disable-next-line no-control-regex -- matching control characters is the point
const CONTROLS = /[\u0000-\u0008\u000B-\u001F\u007F-\u009F\u061C\u200B-\u200F\u2028-\u202E\u2060-\u2069\uFEFF\uFFF9-\uFFFB\u{E0000}-\u{E007F}]/gu;


/**
 * Makes text from another person safe to read: bidi overrides and isolates,
 * zero-width characters and control characters (ANSI escapes included) are
 * shown as visible ‹U+XXXX› marks instead of acting on the layout or a
 * terminal. Newlines and tabs stay. Markup is not touched: it is only ever
 * placed in the page as text.
 */
export function visible(input: unknown): string {
  const s = typeof input === "string" ? input : input === null || input === undefined ? "" : String(input);
  return s.replace(CONTROLS, (c) => `‹U+${c.codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")}›`);
}

/** Copies text; resolves false if the clipboard is blocked. */
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}
