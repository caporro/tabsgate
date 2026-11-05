// End-to-end UI checks against the real API and an isolated local fake provider.
// Optional development tool: no npm dependencies or browser tooling ship in Go.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, mkdir, rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.TABSGATE_PLAYWRIGHT_MODULE || 'playwright');
const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const directory = await mkdtemp(path.join(tmpdir(), 'tabsgate-ui-smoke-'));
const binary = path.join(directory, 'fixture');
const screenshots = process.env.TABSGATE_UI_SCREENSHOTS;
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
let server, browser;

try {
  const build = spawnSync(process.env.TABSGATE_GO_BIN || 'go', ['build', '-o', binary, './scripts/ui_fixture.go'], { cwd: root, encoding: 'utf8' });
  assert.equal(build.status, 0, build.stderr || build.error?.message);
  server = spawn(binary, [], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] });
  const url = await new Promise((resolve, reject) => {
    let output = '';
    const timeout = setTimeout(() => reject(new Error('UI fixture did not start')), 15000);
    server.on('error', error => { clearTimeout(timeout); reject(error); });
    server.stdout.on('data', data => {
      output += data;
      if (output.includes('\n')) {
        try { clearTimeout(timeout); resolve(JSON.parse(output.split('\n')[0]).url); }
        catch (error) { reject(error); }
      }
    });
    server.once('exit', code => { clearTimeout(timeout); reject(new Error('Fixture exited: ' + code)); });
  });
  console.log('UI fixture: ' + url + ' (local fake provider)');
  browser = await chromium.launch({ headless: true, ...(process.env.TABSGATE_CHROME_EXECUTABLE ? { executablePath: process.env.TABSGATE_CHROME_EXECUTABLE } : {}) });
  const context = await browser.newContext({ viewport: { width: 960, height: 1000 } });
  const page = await context.newPage();
  const pageErrors = [], cspErrors = [], requests = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  page.on('console', message => { if (message.text().includes('Content Security Policy')) cspErrors.push(message.text()); });
  page.on('request', request => requests.push(request.url()));
  // Respect the production management limiter while exercising many actions.
  let nextManagementRequest = 0;
  await page.route('**/api/v1/connections**', async route => {
    const now = Date.now();
    const wait = Math.max(0, nextManagementRequest - now);
    nextManagementRequest = Math.max(now, nextManagementRequest) + 2050;
    await delay(wait);
    await route.continue();
  });
  const ready = () => page.waitForFunction(() => document.getElementById('tabsgate')?.getAttribute('aria-busy') === 'false');
  const screenshot = async name => {
    if (!screenshots) return;
    await mkdir(screenshots, { recursive: true });
    await ready();
    await page.screenshot({ path: path.join(screenshots, name + '.png'), fullPage: true });
  };
  const fits = async () => {
    const size = await page.evaluate(() => ({ width: innerWidth, scroll: document.documentElement.scrollWidth }));
    assert(size.scroll <= size.width, JSON.stringify(size));
  };
  let testKey = 'ui-test-provider-key';
  let testPass = 'ui-test-pass';
  let openedConnection;
  const signOut = async () => {
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Sign out', exact: true }).click();
  };
  const open = async (pass = testPass, key = testKey) => {
    await page.locator('#tg-original').fill(key);
    await page.locator('#tg-pass').fill(pass);
    const response = page.waitForResponse(response => response.url().endsWith('/api/v1/connections') && response.request().method() === 'POST');
    await page.locator('#tg-pass').press('Enter');
    openedConnection = await (await response).json();
    await page.getByRole('button', { name: '+ New key', exact: true }).waitFor();
    await ready();
  };
  await page.goto(url);
  await ready();
  assert.equal(await page.locator('#tg-provider option').count(), 2);
  assert.equal(await page.locator('html').getAttribute('lang'), 'en');
  assert.equal(await page.locator('#tg-original').inputValue(), '');
  await screenshot('entry');
  await open();
  assert.equal(await page.locator('.tg-key-row').count(), 0);
  await page.getByRole('button', { name: '+ New key', exact: true }).click();
  await page.locator('#tg-model-options option[value="test-model"]').waitFor({ state: 'attached' });
  await page.locator('#tg-name').fill('Demo <img src=x onerror="window.uiXss=1">');
  await page.locator('#tg-model').fill('test-model');
  await page.locator('#tg-requests').fill('30');
  await page.getByText('Options', { exact: true }).click();
  await page.locator('#tg-extra-models').fill('other-model');
  const issuedResponse = page.waitForResponse(response => response.url().endsWith('/keys') && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Create key', exact: true }).click();
  const issued = await (await issuedResponse).json();
  await page.locator('#tg-created-secret').waitFor();
  assert.equal(await page.locator('#tg-created-secret').inputValue(), issued.key);
  assert.equal(await page.locator('#tg-base-url').inputValue(), url + '/v1');
  assert.equal(issued.max_output_tokens, 4096);
  assert.deepEqual(issued.allowed_models, ['test-model', 'other-model']);
  await screenshot('created');
  const chat = await fetch(url + '/v1/chat/completions', {
    method: 'POST', headers: { Authorization: 'Bearer ' + issued.key, 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: 'test-model', messages: [{ role: 'user', content: 'test' }] }),
  });
  assert.equal(chat.status, 200);
  assert.equal((await chat.json()).usage.total_tokens, 75);
  await page.getByRole('button', { name: 'Go to virtual keys', exact: true }).click();
  assert.equal(await page.locator('#tg-created-secret').count(), 0);
  assert.equal(await page.locator('.tg-key-row').count(), 1);
  assert.equal(await page.locator('#tg-content img').count(), 0);
  assert.equal(await page.evaluate(() => window.uiXss), undefined);
  await screenshot('list');
  await page.locator('.tg-key-row').click();
  await ready();
  assert.match(await page.locator('.tg-meter').first().innerText(), /1\s*\/\s*30/);
  assert.match(await page.locator('.tg-meter').nth(1).innerText(), /75/);
  await screenshot('detail');
  await page.getByRole('button', { name: 'Edit limits', exact: true }).click();
  await page.locator('#tg-requests').fill('50');
  const editResponse = page.waitForResponse(response => response.request().method() === 'PATCH');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  const edited = await (await editResponse).json();
  await ready();
  assert.equal(edited.requests_used, 1);
  assert.equal(edited.tokens_used, 75);
  assert.equal(edited.expires_at, issued.expires_at);
  assert.deepEqual(edited.allowed_models, issued.allowed_models);
  assert.equal(edited.max_requests, 50);
  // Editing the upstream credentials keeps the same connection and virtual keys.
  const originalConnectionID = openedConnection.id;
  await page.getByRole('button', { name: 'Connection', exact: true }).click();
  await page.locator('#tg-new-pass').fill('ui-rotated-pass');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await ready();
  testPass = 'ui-rotated-pass';
  assert.equal(await page.locator('.tg-key-row').count(), 1);
  await page.getByRole('button', { name: 'Refresh', exact: true }).click(); await ready();
  const rejectedOldPass = await fetch(url + '/api/v1/connections/' + originalConnectionID + '/keys', {
    headers: { Authorization: 'Bearer ui-test-provider-key', 'X-TabsGate-Keyword': 'ui-test-pass' },
  });
  assert.equal(rejectedOldPass.status, 401);
  await page.getByRole('button', { name: 'Connection', exact: true }).click();
  await page.locator('#tg-replacement-key').fill('ui-test-provider-key-rotated');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await ready();
  testKey = 'ui-test-provider-key-rotated';
  await page.getByRole('button', { name: 'Refresh', exact: true }).click(); await ready();
  const rejectedOldKey = await fetch(url + '/api/v1/connections/' + originalConnectionID + '/keys', {
    headers: { Authorization: 'Bearer ui-test-provider-key', 'X-TabsGate-Keyword': testPass },
  });
  assert.equal(rejectedOldKey.status, 401);
  const afterRotation = await fetch(url + '/v1/chat/completions', {
    method: 'POST', headers: { Authorization: 'Bearer ' + issued.key, 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: 'test-model', messages: [{ role: 'user', content: 'test' }] }),
  });
  assert.equal(afterRotation.status, 200);
  await page.locator('.tg-key-row').click(); await ready();
  assert.match(await page.locator('.tg-meter').first().innerText(), /2\s*\/\s*50/);
  await page.setViewportSize({ width: 320, height: 1000 });
  await fits();
  await page.getByRole('button', { name: 'Edit limits', exact: true }).click();
  await fits();
  await screenshot('mobile-form');
  await page.emulateMedia({ colorScheme: 'dark' });
  await screenshot('mobile-dark');
  // Cancel and Escape close the sign-out modal without losing the form draft.
  await page.locator('#tg-name').fill('Unsaved draft');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel', exact: true }).click();
  assert.equal(await page.locator('#tg-name').inputValue(), 'Unsaved draft');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await screenshot('signout-dialog');
  await page.keyboard.press('Escape');
  await page.getByRole('dialog').waitFor({ state: 'hidden' });
  assert.equal(await page.locator('#tg-name').inputValue(), 'Unsaved draft');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await page.getByRole('button', { name: 'Revoke key', exact: true }).click();
  await page.getByRole('button', { name: 'Revoke', exact: true }).click();
  await ready();
  assert.equal(await page.locator('.tg-status').innerText(), 'Revoked');
  const revoked = await fetch(url + '/v1/chat/completions', {
    method: 'POST', headers: { Authorization: 'Bearer ' + issued.key, 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: 'test-model', messages: [{ role: 'user', content: 'test' }] }),
  });
  assert.equal(revoked.status, 403);
  await signOut();
  assert.equal(await page.locator('#tg-original').inputValue(), '');
  assert.equal(await page.locator('#tg-pass').inputValue(), '');
  await page.locator('#tg-original').fill(testKey);
  await page.locator('#tg-pass').fill('wrong-pass');
  await page.getByRole('button', { name: 'Open', exact: true }).click();
  await ready();
  assert.match(await page.locator('#tg-error').innerText(), /key or pass is invalid/);
  // A different connection exercises UTF-8 pass transport over management headers.
  await page.locator('#tg-provider').selectOption('other');
  await open('parola-è-🔑');
  assert.equal(await page.locator('.tg-key-row').count(), 0);
  await signOut();
  await page.locator('#tg-provider').selectOption('test');
  await open();
  const keysBeforeInterruptedCreate = await page.locator('.tg-key-row').count();
  await page.route('**/models', route => route.fulfill({ status: 502, contentType: 'application/json', body: JSON.stringify({ error: { code: 'upstream_error' } }) }));
  await page.getByRole('button', { name: '+ New key', exact: true }).click();
  await page.getByText('Models are unavailable. You can enter a model ID manually.', { exact: true }).waitFor();
  await page.locator('#tg-name').fill('Interrupted response');
  await page.locator('#tg-model').fill('test-model');
  let createAttempts = 0;
  const routeErrors = [];
  await page.route('**/keys', async route => {
    if (route.request().method() !== 'POST') { await route.fallback(); return; }
    createAttempts++;
    try {
      const response = await route.fetch();
      assert.equal(response.status(), 201);
      await route.abort('failed');
    } catch (error) {
      routeErrors.push(error.message);
      await route.abort('failed');
    }
  });
  await page.getByRole('button', { name: 'Create key', exact: true }).click();
  await ready();
  assert.match(await page.locator('#tg-error').innerText(), /may have been created/);
  assert.equal(await page.getByRole('button', { name: 'Create key', exact: true }).isDisabled(), true);
  await page.locator('[data-action="back"]').click();
  await ready();
  assert.equal(await page.locator('.tg-key-row').count(), keysBeforeInterruptedCreate + 1);
  assert.equal(createAttempts, 1);
  await page.locator('.tg-key-row').first().click();
  await ready();
  await page.getByRole('button', { name: 'Refresh', exact: true }).click();
  await ready();
  await page.getByRole('button', { name: 'Connection', exact: true }).click();
  await fits(); await screenshot('connection-settings');
  await page.getByRole('button', { name: 'Delete connection', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel', exact: true }).click();
  assert.equal(await page.locator('#tg-connection-form').count(), 1);
  await page.getByRole('button', { name: 'Delete connection', exact: true }).click();
  await screenshot('delete-dialog');
  await page.getByRole('dialog').getByRole('button', { name: 'Delete connection', exact: true }).click();
  await ready();
  assert.equal(await page.locator('#tg-original').inputValue(), '');
  const deletedKeys = await fetch(url + '/api/v1/connections/' + originalConnectionID + '/keys', {
    headers: { Authorization: 'Bearer ' + testKey, 'X-TabsGate-Keyword': testPass },
  });
  assert.equal(deletedKeys.status, 401);
  const deletedToken = await fetch(url + '/api/v1/key', { headers: { Authorization: 'Bearer ' + issued.key } });
  assert.equal(deletedToken.status, 401);
  // Deleting one connection leaves a separate provider connection intact.
  await page.locator('#tg-provider').selectOption('other');
  await open('parola-è-🔑');
  assert.equal(await page.locator('.tg-key-row').count(), 0);
  const storage = await page.evaluate(() => ({ local: localStorage.length, session: sessionStorage.length, cookie: document.cookie }));
  assert.deepEqual(storage, { local: 0, session: 0, cookie: '' });
  assert(!requests.some(request => request.includes('ui-test-provider-key') || request.includes(issued.key) || request.includes('ui-test-pass')));
  await page.reload(); await ready();
  assert.equal(await page.locator('#tg-original').inputValue(), '');
  assert.equal(await page.locator('#tg-pass').inputValue(), '');
  await fits();
  assert.deepEqual(pageErrors, []);
  assert.deepEqual(cspErrors, []);
  assert.deepEqual(routeErrors, []);
  console.log(JSON.stringify({ open: true, providerModels: true, create: true, actualUsage: true,
    editPreservesUsageAndExpiry: true, multipleModelsPreserved: true, revoke: true, wrongPass: true,
    unicodePass: true, discoveryFailure: true, ambiguousCreateNotRetried: true, noBrowserPersistence: true,
    connectionRotation: true, signOutConfirmation: true, cancelledSignOutPreservesDraft: true,
    deleteConnectionAndKeys: true, connectionIsolation: true, englishUI: true,
    oneTimeKey: true, mobile320: true, dark: true, noXSS: true, consoleErrors: pageErrors }));
} finally {
  if (browser) await browser.close();
  if (server && server.exitCode === null) {
    const exited = once(server, 'exit');
    server.kill('SIGTERM');
    await exited;
  }
  await rm(directory, { recursive: true, force: true });
}
