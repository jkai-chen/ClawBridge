// Loads the link `clawbridge ui --no-open` printed in headless Chrome and
// checks the page under the server's real headers: the shell renders, the
// secret moved to sessionStorage, the address is "/", Sessions and Doctor
// load through /rpc, a reload and a second tab behave, and there is not one
// CSP or Trusted Types violation. Exit status 1 on any failure.
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import puppeteer from "puppeteer-core";

const link = process.argv[2];
if (!link) {
  console.error("usage: headless.mjs LINK");
  process.exit(2);
}
const chrome =
  process.env.CHROME_PATH ||
  ["/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/usr/bin/google-chrome", "/usr/bin/chromium"].find((p) => existsSync(p));

const failures = [];
const check = (ok, what) => {
  console.log(`${ok ? "ok  " : "FAIL"} ${what}`);
  if (!ok) failures.push(what);
};

const profile = mkdtempSync(join(tmpdir(), "cb-chrome-"));
const browser = await puppeteer.launch({
  executablePath: chrome,
  headless: true,
  userDataDir: profile,
  args: ["--no-sandbox", "--disable-gpu", "--no-first-run"],
});
const problems = [];
try {
  const watch = async (page) => {
    await page.evaluateOnNewDocument(() => {
      window.__violations = [];
      document.addEventListener("securitypolicyviolation", (e) => window.__violations.push(`${e.violatedDirective} ${e.blockedURI}`));
    });
    page.on("console", (m) => {
      if (m.type() === "error" || m.type() === "warning") problems.push(`console ${m.type()}: ${m.text()}`);
    });
    page.on("pageerror", (e) => problems.push(`pageerror: ${e.message}`));
  };

  const page = await browser.newPage();
  await watch(page);
  const rpc = [];
  page.on("response", (r) => {
    if (new URL(r.url()).pathname === "/rpc") rpc.push({ status: r.status(), body: r.request().postData() ?? "" });
  });

  const res = await page.goto(link, { waitUntil: "domcontentloaded" });
  check(res?.status() === 200, "the launch link answers 200");
  check((res?.headers()["content-security-policy"] ?? "").includes("require-trusted-types-for 'script'"), "the CSP header is on the page");
  await page.waitForSelector(".sidebar a.nav-item", { timeout: 15000 });
  check(true, "the shell rendered");
  await page.waitForFunction(() => document.querySelector(".conn")?.textContent?.startsWith("Connected"), { timeout: 15000 });
  check(true, "the top bar says Connected (hello and /events worked)");

  const state = await page.evaluate(() => ({
    secret: sessionStorage.getItem("cb-session"),
    meta: document.querySelector('meta[name="cb-session"]') !== null,
    path: location.pathname,
    search: location.search,
  }));
  check(/^[0-9a-f]{64}$/.test(state.secret ?? ""), "sessionStorage holds a 256-bit secret");
  check(!state.meta, "the cb-session meta is gone");
  check(state.path === "/" && state.search === "", "the address is / with no token");

  await page.evaluate(() => { location.hash = "#/sessions"; });
  await page.waitForFunction(() => document.querySelector("h1")?.textContent === "Sessions" && !document.querySelector(".finding")?.textContent?.startsWith("Loading"), { timeout: 15000 });
  check(rpc.some((r) => r.status === 200 && r.body.includes('"sessions.list"')), "Sessions loaded through /rpc");
  await page.evaluate(() => { location.hash = "#/doctor"; });
  await page.waitForFunction(() => document.querySelector("h1")?.textContent === "Doctor" && !document.querySelector(".finding")?.textContent?.startsWith("Checking"), { timeout: 30000 });
  check(rpc.some((r) => r.status === 200 && r.body.includes('"doctor.checks"')), "Doctor loaded through /rpc");

  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForFunction(() => document.querySelector(".conn")?.textContent?.startsWith("Connected"), { timeout: 15000 });
  check(true, "a reload works from sessionStorage");

  const tab = await browser.newPage();
  await watch(tab);
  await tab.goto(new URL("/", link).href, { waitUntil: "domcontentloaded" });
  await tab.waitForSelector(".state-panel h1", { timeout: 15000 });
  check((await tab.$eval(".state-panel h1", (e) => e.textContent)) === "Open this from clawbridge ui", "a second tab with no secret says Open this from clawbridge ui");

  for (const [name, p] of [["first tab", page], ["second tab", tab]]) {
    const v = await p.evaluate(() => window.__violations);
    check(v.length === 0, `no CSP or Trusted Types violation in the ${name}${v.length ? `: ${v.join("; ")}` : ""}`);
  }
  // A reload or a refused call may log a failed fetch only if something broke.
  check(problems.length === 0, `no console errors${problems.length ? `: ${problems.join("; ")}` : ""}`);
} catch (e) {
  check(false, `the check ran: ${e instanceof Error ? e.message : String(e)}`);
} finally {
  await browser.close();
  rmSync(profile, { recursive: true, force: true });
}
if (failures.length) {
  console.error(`${failures.length} check(s) failed`);
  process.exit(1);
}
console.log("headless check passed");
