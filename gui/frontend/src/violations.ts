// Reports what the webview blocks to the Go shell, which logs and counts it.
// A Content-Security-Policy or Trusted Types violation raises a
// `securitypolicyviolation` event; a Trusted Types failure also throws a
// TypeError. Listeners start before anything else loads and buffer until the
// shell bridge is up.

const TT = /Trusted(HTML|Script|ScriptURL)|Trusted Type/i;

export interface ViolationSink {
  /** Sends everything buffered so far, and everything after, to report. */
  connect(report: (text: string) => void): void;
}

export function describeViolation(e: SecurityPolicyViolationEvent): string {
  const where = e.sourceFile ? ` at ${e.sourceFile}:${e.lineNumber}` : "";
  return `${e.violatedDirective} blocked ${e.blockedURI || "inline"}${where}`;
}

export function watchViolations(target: Document | Window = document, win: Window = window): ViolationSink {
  const buffer: string[] = [];
  let report: ((text: string) => void) | null = null;
  const push = (text: string) => {
    if (report) report(text);
    else buffer.push(text);
  };
  target.addEventListener("securitypolicyviolation", (e) => push(describeViolation(e as SecurityPolicyViolationEvent)));
  win.addEventListener("error", (e) => {
    if (TT.test(e.message)) push(`trusted-types error: ${e.message}`);
  });
  win.addEventListener("unhandledrejection", (e) => {
    const msg = e.reason instanceof Error ? e.reason.message : String(e.reason);
    if (TT.test(msg)) push(`trusted-types rejection: ${msg}`);
  });
  return {
    connect(fn) {
      report = fn;
      for (const t of buffer.splice(0)) fn(t);
    },
  };
}
