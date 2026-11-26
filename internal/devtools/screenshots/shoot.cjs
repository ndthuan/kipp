"use strict";

const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("playwright");

const base = need("KIPP_DEMO_URL").replace(/\/$/, "");
const user = need("KIPP_DEMO_USER");
const password = need("KIPP_DEMO_PASSWORD");
const jobs = need("KIPP_DEMO_JOBS");
const out = process.env.SHOTS || path.join(__dirname, "../../../docs/screenshots");
const channel = process.env.PLAYWRIGHT_CHANNEL || undefined;

const desktop = { width: 1440, height: 900 };
const phone = { width: 390, height: 844 };

function need(name) {
  const v = process.env[name];
  if (!v) {
    console.error(`shoot: ${name} is not set`);
    process.exit(2);
  }
  return v;
}

const release = (run) => `name: release
group: deploy
description: Ships the site once its checks pass.
secrets: [CHAT_TOKEN, DEPLOY_KEY]
stages:
  ship: {run: ${run}}
`;

const e2e = `name: payments-e2e
group: frontend
description: Runs the payment flows end to end against a test server.
stages:
  lint: {run: "echo 'eslint: 214 files, no problems'"}
  unit: {run: "echo 'ok   payments  1.84s'; echo 'ok   cart      0.92s'"}
  e2e:
    needs: [lint, unit]
    junit: "reports/*.xml"
    run: |
      mkdir -p reports
      echo 'starting the test server on :4010'
      echo 'PASS  checkout/pays-with-card (2.1s)'
      echo 'PASS  checkout/applies-coupon (1.4s)'
      echo 'FAIL  checkout/refunds-a-partial-order (3.0s)'
      echo '      expected status 200, got 409: refund exceeds captured amount'
      echo '2 passed, 1 failed'
      cat > reports/e2e.xml <<'XML'
      <testsuite name="checkout" tests="3" failures="1">
        <testcase classname="checkout" name="pays-with-card" time="2.1"/>
        <testcase classname="checkout" name="applies-coupon" time="1.4"/>
        <testcase classname="checkout" name="refunds-a-partial-order" time="3.0">
          <failure message="expected status 200, got 409">refund exceeds captured amount</failure>
        </testcase>
      </testsuite>
      XML
      exit 1
  preview: {needs: [e2e], run: ./deploy-preview.sh}
`;

async function failedBuild(page) {
  fs.writeFileSync(path.join(jobs, "payments-e2e.yml"), e2e, { mode: 0o600 });
  await reload(page);
  await page.goto(`${base}/jobs/payments-e2e`);
  await Promise.all([page.waitForLoadState("load"), page.click("form.build button")]);
  const url = `${base}/jobs/payments-e2e/1`;
  for (let i = 0; i < 60; i++) {
    await page.goto(url);
    if (await page.locator(".head .pill, h1 .pill, h1 + *").filter({ hasText: "failed" }).count()) return url;
    if ((await page.textContent("main")).match(/\bfailed\b/) && !(await page.textContent("main")).includes("running")) return url;
    await page.waitForTimeout(500);
  }
  throw new Error("the payments-e2e build did not fail in time");
}

async function login(context) {
  const page = await context.newPage();
  await page.goto(`${base}/login`);
  await page.fill("#name", user);
  await page.fill("#password", password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith("/login")), page.click("button[type=submit], form[action='/login'] button")]);
  return page;
}

async function settle(page) {
  await page.waitForLoadState("networkidle");
  await page.evaluate(() => document.fonts.ready);
}

async function shot(page, name, opts = {}) {
  await settle(page);
  const file = path.join(out, `${name}.png`);
  if (opts.element) {
    await page.locator(opts.element).first().screenshot({ path: file });
  } else {
    await page.screenshot({ path: file, fullPage: Boolean(opts.full) });
  }
  console.log(`shoot: ${file}`);
}

async function reload(page) {
  await page.goto(`${base}/admin/server`);
  await Promise.all([page.waitForLoadState("load"), page.click("form[action='/admin/server/reload'] button")]);
}

async function confirmed(page) {
  if (!(await page.locator("h1:has-text('Confirm your password'), h2:has-text('Confirm your password')").count())) return false;
  await page.fill("input[type=password]", password);
  await Promise.all([page.waitForLoadState("load"), page.click("button:has-text('Confirm')")]);
  return true;
}

async function setSecret(page) {
  await page.goto(`${base}/admin/secrets`);
  await page.click("summary:has-text('Set a secret')");
  await page.fill("#secret-name", "CHAT_TOKEN");
  await page.fill("#secret-value", "demo-value-not-a-secret");
  await Promise.all([page.waitForLoadState("load"), page.click("#set button.action-main")]);
}

async function prepare(page) {
  fs.writeFileSync(path.join(jobs, "release.yml"), release("./ship.sh"), { mode: 0o600 });
  await reload(page);
  await setSecret(page);
  if (await confirmed(page)) await setSecret(page);
}

async function firstMetricsStore(page) {
  await page.goto(`${base}/metrics`);
  const href = await page.locator("a[href^='/metrics/']").first().getAttribute("href");
  if (!href) throw new Error("the demo has no metrics store");
  return base + href;
}

async function editAndConflict(page) {
  await page.goto(`${base}/jobs/release/edit`);
  await page.fill("#yaml", "name: release\ngroup: deploy\nstages:\n  ship: {run: ./ship.sh, needs: [missing]}\n");
  await Promise.all([page.waitForLoadState("load"), page.click("form.editor button.action-main")]);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await shot(page, "job-edit");
  await page.setViewportSize(desktop);

  await page.goto(`${base}/jobs/release/edit`);
  await page.fill("#yaml", release("./ship.sh --canary"));
  await Promise.all([page.waitForLoadState("load"), page.click("form.editor button.action-main")]);
  fs.writeFileSync(path.join(jobs, "release.yml"), release("./ship.sh --all-regions"), { mode: 0o600 });
  await reload(page);
  await page.goto(`${base}/jobs/release`);
  await page.setViewportSize({ width: 1280, height: 900 });
  await shot(page, "job-conflict", { element: "#conflict" });
  await page.setViewportSize(desktop);
}

async function main() {
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch({ channel, args: ["--ignore-certificate-errors"] });
  try {
    const light = await browser.newContext({ viewport: desktop, colorScheme: "light" });
    const page = await login(light);
    await prepare(page);

    await page.goto(`${base}/`);
    await shot(page, "home");
    await shot(page, "overview", { full: true });
    await failedBuild(page);
    await shot(page, "build", { full: true });
    await page.goto(`${base}/jobs`);
    await shot(page, "job-list", { full: true });
    await page.goto(`${base}/jobs/release`);
    await shot(page, "job");
    await page.goto(await firstMetricsStore(page));
    await shot(page, "metrics", { full: true });
    await page.goto(`${base}/queue`);
    await shot(page, "queue");
    await page.goto(`${base}/admin/secrets`);
    await shot(page, "secrets", { full: true });
    await page.goto(`${base}/admin/users/${encodeURIComponent(user)}`);
    await shot(page, "user", { full: true });
    await editAndConflict(page);
    const state = await light.storageState();

    const dark = await browser.newContext({ viewport: desktop, colorScheme: "dark", storageState: state });
    const darkPage = await dark.newPage();
    await darkPage.goto(`${base}/`);
    await shot(darkPage, "home-dark");

    for (const scheme of ["light", "dark"]) {
      const narrow = await browser.newContext({ viewport: phone, colorScheme: scheme, storageState: state, isMobile: true, hasTouch: true });
      const p = await narrow.newPage();
      const suffix = scheme === "dark" ? "-dark" : "";
      await p.goto(`${base}/`);
      await shot(p, `overview-390${suffix}`, { full: true });
      if (scheme === "light") {
        await p.goto(`${base}/queue`);
        await shot(p, "queue-390");
      }
    }
  } finally {
    await browser.close();
  }
}

main().catch((err) => {
  console.error(`shoot: ${err.stack || err}`);
  process.exit(1);
});
