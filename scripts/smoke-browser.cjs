// Run through smoke-host.py. The throwaway host key arrives on stdin, not argv.
const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

(async () => {
  const { base, key } = JSON.parse(fs.readFileSync(0, 'utf8'));
  const browser = await chromium.launch();
  let page;
  try {
    const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1100 } });
    page = await context.newPage();
    page.on('pageerror', (error) => console.error('Browser script error: ' + error.message));
    const requests = [];
    page.on('request', (request) => {
      if (request.url().includes('/v0/management/codex-wakeup/')) {
        requests.push({ fromParent: request.frame() === page.mainFrame(),
          authenticated: request.headers().authorization === 'Bearer ' + key });
      }
    });
    await page.goto(base + '/management.html#/plugin-pages/codex-wakeup/0');
    await page.locator('input[name="cpa-management-key"]').fill(key);
    const remember = page.getByRole('checkbox', { name: '记住密码' });
    if (!await remember.isChecked()) await remember.press('Space');
    await page.locator('input[name="cpa-management-key"]').press('Enter');
    const pluginLink = page.locator('a[href="#/plugin-pages/codex-wakeup/0"]');
    await pluginLink.click();
    const frame = page.frameLocator('iframe[sandbox]');
    await frame.locator('#msg').filter({ hasText: '已刷新' }).waitFor();
    assert.equal(await frame.locator('#key-entry').isVisible(), false);
    assert.equal(await frame.locator('#key').inputValue(), '');
    assert.equal(await page.locator('iframe').getAttribute('sandbox'), 'allow-scripts');
    const isolated = await frame.locator('body').evaluate(() => {
      let parentBlocked = false, storageBlocked = false;
      try { void parent.document.body; } catch { parentBlocked = true; }
      try { void localStorage.length; } catch { storageBlocked = true; }
      return parentBlocked && storageBlocked;
    });
    assert.equal(isolated, true);
    assert.ok(requests.length >= 5 && requests.every((request) => request.fromParent && request.authenticated));

    // Preview and task mutations must also pass through the parent's client.
    await frame.locator('#new-task').click();
    await frame.locator('[data-kind="interval"]').click();
    await frame.locator('#preview').filter({ hasText: '1.' }).waitFor();
    await frame.locator('#cancel-modal').click();
    const card = frame.locator('#tasks .card').first();
    await card.getByRole('button', { name: '启用', exact: true }).click();
    await card.getByRole('button', { name: '停用', exact: true }).waitFor();
    await card.getByRole('button', { name: '删除', exact: true }).click();
    await frame.locator('#delete-yes').click();
    await frame.locator('#tasks').filter({ hasText: '还没有唤醒任务' }).waitFor();
    await frame.locator('#refresh').click();
    await frame.locator('#msg').filter({ hasText: '已刷新' }).waitFor();
    await page.locator('a[href="#/plugins"]').first().click();
    await page.locator('iframe').waitFor({ state: 'detached' });
    await page.getByRole('heading', { name: '插件管理', exact: true }).waitFor();
    await pluginLink.click();
    await frame.locator('#msg').filter({ hasText: '已刷新' }).waitFor();
    await page.reload();
    await pluginLink.click();
    await frame.locator('#msg').filter({ hasText: '已刷新' }).waitFor();
    assert.ok(requests.every((request) => request.fromParent && request.authenticated));
    if (process.env.SMOKE_SCREENSHOT_DIR) {
      fs.mkdirSync(process.env.SMOKE_SCREENSHOT_DIR, { recursive: true });
      await page.screenshot({ path: path.join(process.env.SMOKE_SCREENSHOT_DIR, 'embedded.png') });
    }

    const standalone = await context.newPage();
    let standaloneRequests = 0;
    standalone.on('request', (request) => {
      if (request.url().includes('/v0/management/')) standaloneRequests += 1;
    });
    await standalone.goto(base + '/v0/resource/plugins/codex-wakeup/status');
    await standalone.locator('#msg').filter({ hasText: '请先输入' }).waitFor();
    await standalone.locator('#refresh').click();
    assert.equal(standaloneRequests, 0);
    assert.equal(await standalone.locator('#key-entry').isVisible(), true);
    await standalone.locator('#key').fill(key);
    await standalone.locator('#refresh').click();
    await standalone.locator('#msg').filter({ hasText: '已刷新' }).waitFor();
    assert.ok(standaloneRequests >= 4);
    const beforeReload = standaloneRequests;
    await standalone.reload();
    await standalone.locator('#msg').filter({ hasText: '请先输入' }).waitFor();
    assert.equal(await standalone.locator('#key').inputValue(), '');
    assert.equal(standaloneRequests, beforeReload);
    if (process.env.SMOKE_SCREENSHOT_DIR) {
      await standalone.screenshot({ path: path.join(process.env.SMOKE_SCREENSHOT_DIR, 'standalone.png') });
    }
    await page.getByTitle(/退出|登出|Logout/i).click();
    await page.locator('input[name="cpa-management-key"]').waitFor();
    assert.equal(await page.locator('iframe').count(), 0);
    console.log('Browser smoke passed: embedded auth, no child key/storage access, preview/mutations, reopen/reload/logout, standalone manual key and empty-key guard');
  } catch (error) {
    if (page) {
      console.error('Browser route: ' + new URL(page.url()).hash);
      for (const frame of page.frames()) {
        console.error('Frame status: ' + await frame.locator('body').innerText().catch(() => 'unavailable'));
      }
    }
    throw error;
  } finally {
    await browser.close();
  }
})().catch((error) => { console.error(error.stack); process.exitCode = 1; });
