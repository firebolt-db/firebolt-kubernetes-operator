// Browser smoke test for the Engine Web UI workspace, run by
// verify-ui-sidecar.sh inside the pinned Playwright container.
//
// Loads the SPA in a real browser, runs a query from the SQL editor, and
// asserts the result renders. This is the only layer that catches
// client-side regressions (uncaught exceptions, request storms, broken
// query construction) — server-side request replay cannot see JavaScript
// that fails before a request is made.
//
// Selector contract with the webui repo (lite build): the run button
// carries data-testid "execute-query-btn" (or
// "execute-first-template-query-btn" for the first template script), the
// results table is data-testid "datagrid", a failed query renders
// data-testid "query-error-message", and the editor is CodeMirror
// (".cm-content"). If webui renames these, this test fails loudly — update
// both sides together.
import { chromium } from "playwright";

const BASE_URL = process.env.UI_BASE_URL ?? "http://localhost:9100";
const TIMEOUT_MS = 60_000;

const fail = msg => {
  console.error(`FAIL: ${msg}`);
  process.exit(1);
};

const browser = await chromium.launch();
// A real browser always carries a locale; the bare container does not, and
// the SPA's Intl usage throws "Incorrect locale information provided"
// without one.
const page = await browser.newPage({ locale: "en-US", timezoneId: "UTC" });

const pageErrors = [];
const failedRequests = [];
page.on("pageerror", err => pageErrors.push(String(err)));
page.on("response", resp => {
  if (resp.status() >= 400) {
    failedRequests.push(`${resp.status()} ${resp.request().method()} ${resp.url()}`);
  }
});

console.log(`Loading ${BASE_URL} ...`);
await page.goto(BASE_URL, { waitUntil: "load", timeout: TIMEOUT_MS });

// The run button rendering means the workspace survived startup (engines
// and databases resolved; no error boundary).
const runButton = page.locator(
  '[data-testid="execute-query-btn"], [data-testid="execute-first-template-query-btn"]'
);
await runButton
  .first()
  .waitFor({ state: "visible", timeout: TIMEOUT_MS })
  .catch(() => fail(`workspace did not render a run button within ${TIMEOUT_MS}ms; ` +
    `page errors: ${JSON.stringify(pageErrors)}; failed requests: ${JSON.stringify(failedRequests)}`));
console.log("Workspace rendered (run button visible).");

// Type a query into the CodeMirror editor, replacing any template content.
const editor = page.locator(".cm-content").first();
await editor.click({ timeout: TIMEOUT_MS });
await page.keyboard.press("ControlOrMeta+a");
await page.keyboard.type("SELECT 42 AS ui_smoke;");
await runButton.first().click();
console.log("Query submitted from the SQL editor.");

// Either the results grid renders with our value, or the workspace surfaced
// a query error — both are deterministic outcomes; anything else times out.
const outcome = await Promise.race([
  page
    .locator('[data-testid="datagrid"]')
    .filter({ hasText: "42" })
    .first()
    .waitFor({ state: "visible", timeout: TIMEOUT_MS })
    .then(() => "results"),
  page
    .locator('[data-testid="query-error-message"]')
    .first()
    .waitFor({ state: "visible", timeout: TIMEOUT_MS })
    .then(() => "query-error"),
]).catch(() => "timeout");

if (outcome !== "results") {
  const errText =
    outcome === "query-error"
      ? await page.locator('[data-testid="query-error-message"]').first().innerText()
      : "(no results and no error rendered)";
  fail(`query did not produce results (outcome=${outcome}): ${errText}; ` +
    `page errors: ${JSON.stringify(pageErrors)}; failed requests: ${JSON.stringify(failedRequests)}`);
}
console.log("Results grid rendered the query result.");

if (pageErrors.length > 0) {
  fail(`uncaught page errors during the session: ${JSON.stringify(pageErrors)}`);
}

await browser.close();
console.log("UI workspace browser smoke passed.");
