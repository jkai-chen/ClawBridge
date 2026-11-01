// Line icons (16px grid) and the bridge monogram, ported from the prototype.
// Built with createElementNS: no markup string is ever parsed.

const SVGNS = "http://www.w3.org/2000/svg";

export const ICONS: Record<string, readonly [string, Record<string, string | number>][]> = {
  overview: [['path', { d: 'M2 4h8M5 8h9M3 12h7' }]],
  inbox: [['path', { d: 'm2 9 1.5-5.5h9L14 9v3.5a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1z' }], ['path', { d: 'M2 9h3.5l1 1.5h3L10.5 9H14' }]],
  needs: [['path', { d: 'M8 2.5a4 4 0 0 0-4 4V9l-1.25 2.25h10.5L12 9V6.5a4 4 0 0 0-4-4z' }], ['path', { d: 'M6.5 13.25a1.6 1.6 0 0 0 3 0' }]],
  sessions: [['rect', { x: 2, y: 3, width: 12, height: 10, rx: 1.5 }], ['path', { d: 'm5 7 2 1.5L5 10M8.5 10.5H11' }]],
  projects: [['path', { d: 'M2 4.5A1.5 1.5 0 0 1 3.5 3h2.8l1.4 1.5h4.8A1.5 1.5 0 0 1 14 6v5.5a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 2 11.5z' }]],
  linear: [['circle', { cx: 4, cy: 4, r: 1.75 }], ['circle', { cx: 4, cy: 12, r: 1.75 }], ['circle', { cx: 12, cy: 8, r: 1.75 }], ['path', { d: 'M4 5.75v4.5M5.5 5.2C9 5 12 5.5 12 6.25' }]],
  teams: [['circle', { cx: 6, cy: 5.5, r: 2.25 }], ['path', { d: 'M1.75 13c.4-2.4 2.1-3.6 4.25-3.6s3.85 1.2 4.25 3.6' }], ['circle', { cx: 11.5, cy: 6.5, r: 1.75 }], ['path', { d: 'M11.2 10c1.7.1 2.8 1.2 3.1 3' }]],
  dispatcher: [['path', { d: 'M8 2.5v4M8 6.5 4 10v3.5M8 6.5l4 3.5v3.5' }]],
  account: [['circle', { cx: 8, cy: 5.5, r: 2.5 }], ['path', { d: 'M3 13.5c.5-2.5 2.5-4 5-4s4.5 1.5 5 4' }]],
  doctor: [['path', { d: 'M1.5 8h3L6 4.5l3 7L10.5 8h4' }]],
  settings: [['path', { d: 'M3 5h10M3 11h10' }], ['circle', { cx: 6, cy: 5, r: 1.5 }], ['circle', { cx: 10, cy: 11, r: 1.5 }]],
  search: [['circle', { cx: 7, cy: 7, r: 4.5 }], ['path', { d: 'm10.5 10.5 3.5 3.5' }]],
  more: [['circle', { cx: 3.5, cy: 8, r: 1, fill: 'currentColor', stroke: 'none' }], ['circle', { cx: 8, cy: 8, r: 1, fill: 'currentColor', stroke: 'none' }], ['circle', { cx: 12.5, cy: 8, r: 1, fill: 'currentColor', stroke: 'none' }]],
  down: [['path', { d: 'm4 6 4 4 4-4' }]],
  right: [['path', { d: 'm6 4 4 4-4 4' }]],
  copy: [['rect', { x: 5.5, y: 5.5, width: 8, height: 8, rx: 1.5 }], ['path', { d: 'M3 10.5V4a1 1 0 0 1 1-1h6.5' }]],
  x: [['path', { d: 'm4 4 8 8M12 4l-8 8' }]],
  check: [['path', { d: 'm3.5 8.5 3 3 6-7' }]],
  out: [['path', { d: 'm5 11 6-6M6 5h5v5' }]],
  warn: [['path', { d: 'M8 2.5 14 13H2z' }], ['path', { d: 'M8 6.5v3M8 11.5v.01' }]],
  info: [['circle', { cx: 8, cy: 8, r: 6 }], ['path', { d: 'M8 7.5v3.5M8 5v.01' }]],
  plus: [['path', { d: 'M8 3v10M3 8h10' }]],
  refresh: [['path', { d: 'M13 8a5 5 0 1 1-1.6-3.7M13 2.5v3h-3' }]],
  tune: [['path', { d: 'M3 5h10M3 11h10' }], ['circle', { cx: 6, cy: 5, r: 1.5 }], ['circle', { cx: 10, cy: 11, r: 1.5 }]],
  min: [['path', { d: 'M3 8h10' }]],
  max: [['rect', { x: 3.5, y: 3.5, width: 9, height: 9 }]],
  };


function shapes(svg: SVGElement, parts: readonly [string, Record<string, string | number>][]): void {
  for (const [tag, attrs] of parts) {
    const node = document.createElementNS(SVGNS, tag);
    for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, String(v));
    svg.appendChild(node);
  }
}

export function icon(name: string, size = 16): SVGElement {
  const svg = document.createElementNS(SVGNS, "svg");
  for (const [k, v] of Object.entries({
    viewBox: "0 0 16 16", width: size, height: size, fill: "none", stroke: "currentColor",
    "stroke-width": 1.5, "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true",
  })) svg.setAttribute(k, String(v));
  shapes(svg, ICONS[name] ?? []);
  return svg;
}

/** The drawn bridge monogram: two short bars joined by an arc. */
export function mark(size = 20): SVGElement {
  const svg = document.createElementNS(SVGNS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("aria-hidden", "true");
  shapes(svg, [
    ["rect", { x: 2.5, y: 11, width: 3.5, height: 9, rx: 1, fill: "currentColor" }],
    ["rect", { x: 18, y: 11, width: 3.5, height: 9, rx: 1, fill: "currentColor" }],
    ["path", { d: "M4.25 12C7.5 3.5 16.5 3.5 19.75 12", fill: "none", stroke: "currentColor", "stroke-width": 2.4, "stroke-linecap": "round" }],
  ]);
  return svg;
}
