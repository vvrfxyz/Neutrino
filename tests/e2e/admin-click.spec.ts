import { expect, test, type Page } from '@playwright/test';

const adminUser = process.env.E2E_ADMIN_USER || 'e2e-admin';
const adminPass = process.env.E2E_ADMIN_PASS || 'e2e-pass';

test.describe.configure({ mode: 'serial' });

async function login(page: Page) {
  await page.goto('/login');
  await expect(page.getByRole('heading', { name: 'Neutrino' })).toBeVisible();
  await page.locator('input[name="username"]').fill(adminUser);
  await page.locator('input[name="password"]').fill(adminPass);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page).toHaveURL(/\/users$/);
  await expect(page.locator('#app-main')).toHaveCount(1);
  await page.waitForFunction(() => Boolean((window as any).htmx && (window as any).Alpine));
}

async function expectActiveSidebar(page: Page, key: string) {
  const active = page.locator(`[data-sidebar-page="${key}"]`);
  await expect(active).toHaveAttribute('aria-current', 'page');
  await expect(active).toHaveClass(/bg-sidebar-active/);
}

async function expectMainPaddingLeft(page: Page, expectedPx: number) {
  await expect.poll(async () => {
    return page.evaluate(() => {
      const main = document.querySelector('#app-main');
      return main ? Math.round(Number.parseFloat(getComputedStyle(main).paddingLeft)) : -1;
    });
  }).toBe(expectedPx);
}

async function clickConfirmOK(page: Page) {
  await page.getByRole('button', { name: '确认' }).click();
}

function nodeCard(page: Page, name: string) {
  return page.getByRole('heading', { name }).locator('xpath=ancestor::div[contains(@class, "rounded-xl")][1]');
}

async function createUserFromUsersPage(page: Page, username: string) {
  await page.goto('/users');
  await page.getByRole('button', { name: '创建用户' }).first().click();
  const form = page.locator('form[hx-post="/users"]');
  await expect(form).toBeVisible();
  await form.locator('input[name="username"]').fill(username);
  await form.locator('input[name="monthly_limit_gb"]').fill('30');
  await form.locator('select[name="counting_mode"]').selectOption('single');
  await form.locator('input[name="plan_days"]').fill('14');
  await form.locator('select[name="quota_cycle"]').selectOption('week');
  await form.locator('input[name="quota_tz"]').fill('Asia/Shanghai');
  await form.locator('input[name="device_limit"]').fill('3');
  await Promise.all([
    page.waitForResponse(resp => resp.url().endsWith('/users') && resp.request().method() === 'POST' && resp.status() === 200),
    form.getByRole('button', { name: '创建用户' }).click(),
  ]);
  await expect(page.locator('#users-data')).toContainText(username);
  await expect(form).toBeHidden();
}

async function createNodeFromNodesPage(page: Page, name: string, managed: boolean) {
  await page.goto('/nodes');
  await page.getByRole('button', { name: '添加节点' }).click();
  const form = page.locator('form[hx-post="/api/v1/nodes"]');
  await expect(form).toBeVisible();
  await form.locator('input[name="name"]').fill(name);
  await form.locator('select[name="core_type"]').selectOption('xray');
  await form.locator('select[name="protocol"]').selectOption('vless_reality');
  await form.locator('input[name="host"]').fill(`${name}.example.test`);
  await form.locator('input[name="port"]').fill('443');
  const managedInput = form.locator('input[name="managed"]');
  if (managed) {
    await managedInput.check();
  } else {
    await managedInput.uncheck();
  }
  await Promise.all([
    page.waitForResponse(resp => resp.url().endsWith('/api/v1/nodes') && resp.request().method() === 'POST' && resp.status() === 201),
    form.getByRole('button', { name: '创建' }).click(),
  ]);
  await expect(page.getByRole('heading', { name })).toBeVisible();
}

test('login, htmx sidebar navigation, history, and logout are clickable', async ({ page }) => {
  await login(page);
  const marker = await page.evaluate(() => ((window as any).__e2eShellMarker = crypto.randomUUID()));

  await page.getByLabel('切换侧边栏').first().click();
  await expect.poll(() => page.evaluate(() => localStorage.getItem('neutrino:sidebar:collapsed'))).toBe('1');
  await expectMainPaddingLeft(page, 80);
  await page.getByLabel('切换侧边栏').first().click();
  await expect.poll(() => page.evaluate(() => localStorage.getItem('neutrino:sidebar:collapsed'))).toBe('0');
  await expectMainPaddingLeft(page, 224);
  await page.getByLabel('切换侧边栏').first().click();
  await expect.poll(() => page.evaluate(() => localStorage.getItem('neutrino:sidebar:collapsed'))).toBe('1');
  await expectMainPaddingLeft(page, 80);

  const pages = [
    { key: 'nodes', path: '/nodes', title: '节点管理' },
    { key: 'traffic', path: '/traffic', title: '流量分析' },
    { key: 'enforcements', path: '/enforcements', title: '执行记录' },
    { key: 'apikeys', path: '/apikeys', title: 'API 密钥' },
    { key: 'ops', path: '/ops', title: '运维监控' },
    { key: 'users', path: '/users', title: '用户管理' },
  ];

  for (const item of pages) {
    await page.locator(`[data-sidebar-page="${item.key}"]`).click();
    await expect(page).toHaveURL(new RegExp(`${item.path}$`));
    await expect(page.locator('#app-main')).toHaveCount(1);
    await expect(page.getByRole('heading', { name: item.title })).toBeVisible();
    await expectActiveSidebar(page, item.key);
    await expectMainPaddingLeft(page, 80);
    if (item.key === 'nodes') {
      await expect.poll(() => page.evaluate(() => (window as any).__e2eShellMarker)).toBe(marker);
    }
  }

  await page.goBack();
  await expect(page).toHaveURL(/\/ops$/);
  await expectActiveSidebar(page, 'ops');
  await page.goForward();
  await expect(page).toHaveURL(/\/users$/);
  await expectActiveSidebar(page, 'users');

  await page.getByRole('button', { name: '退出' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto('/users');
  await expect(page).toHaveURL(/\/login$/);
});

test('users workflow is clickable from rendered controls', async ({ page }) => {
  await login(page);
  const username = `pw-user-${Date.now()}`;

  await createUserFromUsersPage(page, username);
  await page.locator('#users-data').getByRole('link', { name: '详情' }).first().click();
  await expect(page).toHaveURL(/\/users\/\d+$/);
  await expect(page.getByRole('heading', { name: username })).toBeVisible();
  const userPath = new URL(page.url()).pathname;

  await page.getByLabel('复制订阅 URL').click();
  await expect(page.locator('body')).toContainText('已复制');
  for (const period of ['日', '月', '小时']) {
    await page.getByRole('button', { name: period }).click();
    await expect(page.getByRole('button', { name: period })).toBeVisible();
  }

  await page.getByRole('button', { name: '新链接' }).click();
  await expect(page).toHaveURL(new RegExp(`${userPath}$`));
  await expect(page.getByRole('heading', { name: username })).toBeVisible();

  await page.locator(`form[action="${userPath}/device_limit"] input[name="device_limit"]`).fill('5');
  await page.locator(`form[action="${userPath}/device_limit"]`).getByRole('button', { name: '更新' }).click();
  await expect(page.locator(`form[action="${userPath}/device_limit"] input[name="device_limit"]`)).toHaveValue('5');

  await page.locator(`form[action="${userPath}/quota_credit"] input[name="credit_gb"]`).fill('1');
  await page.locator(`form[action="${userPath}/quota_credit"]`).getByRole('button', { name: '补偿' }).click();
  await expect(page.locator('body')).toContainText('1.00 GB');

  await page.locator(`form[action="${userPath}/plan_extend"] input[name="extend_days"]`).fill('2');
  await page.locator(`form[action="${userPath}/plan_extend"]`).getByRole('button', { name: '延期' }).click();
  await expect(page.getByRole('heading', { name: username })).toBeVisible();

  await page.locator(`form[action="${userPath}/quota_reset"] input[name="reason"]`).fill('playwright');
  await page.locator(`form[action="${userPath}/quota_reset"]`).getByRole('button', { name: '重置' }).click();
  await clickConfirmOK(page);
  await expect(page.getByRole('heading', { name: username })).toBeVisible();

  await page.getByRole('button', { name: '禁用' }).click();
  await expect(page.locator('body')).toContainText('disabled');
  await page.getByRole('button', { name: '启用' }).click();
  await expect(page.locator('body')).toContainText('active');

  await page.getByRole('button', { name: '删除' }).click();
  await clickConfirmOK(page);
  await expect(page).toHaveURL(/\/users$/);
  await expect(page.locator('#users-data')).not.toContainText(username);
});

test('nodes and deploy workflow are clickable from rendered controls', async ({ page }) => {
  await login(page);

  const deleteNodeName = `pw-delete-node-${Date.now()}`;
  await createNodeFromNodesPage(page, deleteNodeName, false);
  page.once('dialog', dialog => dialog.accept());
  const deleteResponses = await Promise.all([
    page.waitForResponse(resp => resp.url().includes('/api/v1/nodes/') && resp.request().method() === 'DELETE'),
    page.locator('button[hx-delete^="/api/v1/nodes/"]').filter({ hasText: '删除' }).last().click(),
  ]);
  expect([200, 202]).toContain(deleteResponses[0].status());

  const nodeName = `pw-managed-node-${Date.now()}`;
  await createNodeFromNodesPage(page, nodeName, true);
  await nodeCard(page, nodeName).locator('button[hx-post$="/disable"]').filter({ hasText: '禁用' }).click();
  await expect(nodeCard(page, nodeName)).toContainText('disabled');
  await nodeCard(page, nodeName).locator('button[hx-post$="/enable"]').filter({ hasText: '启用' }).click();
  await expect(nodeCard(page, nodeName)).toContainText('enabled');

  await nodeCard(page, nodeName).getByRole('link', { name: '部署' }).click();
  await expect(page.getByRole('heading', { name: '节点部署配置' })).toBeVisible();
  await expect(page.locator('body')).toContainText(nodeName);
  const deployPath = new URL(page.url()).pathname;

  await page.locator(`form[action="${deployPath}"]`).getByRole('button').click();
  await expect(page.locator('#deploy-script')).toBeVisible();
  await page.getByRole('button', { name: '一键复制脚本' }).click();
  await expect(page.locator('body')).toContainText('已复制');

  await page.getByRole('button', { name: '添加变量' }).click();
  await page.locator('#managed-xray-config tbody input').nth(0).fill('XRAY_E2E');
  await page.locator('#managed-xray-config tbody input').nth(1).fill('enabled');
  await page.getByRole('button', { name: '保存配置' }).click();
  await expect(page.locator('#managed-xray')).toContainText('已保存 extra_json');

  // Custom outbounds/routes via the structured form editor: build a socks
  // outbound + a geosite route, validate + preview, then save into extra_json.
  await page.getByRole('button', { name: '添加出站' }).click();
  await page.getByLabel('出站 tag').fill('pw-up');
  await page.getByLabel('出站协议').selectOption('socks');
  await page.getByLabel('出站地址').fill('10.0.0.9');
  await page.getByLabel('出站端口').fill('1080');
  await page.getByRole('button', { name: '添加路由' }).click();
  await page.getByLabel('路由目标出站').selectOption('pw-up');
  await page.getByLabel('路由 domains').fill('geosite:openai');
  await page.getByRole('button', { name: '校验并预览渲染配置' }).click();
  await expect(page.locator('#custom-xray-config')).toContainText('校验通过');
  await expect(page.locator('#render-preview')).toContainText('pw-up');
  await expect(page.locator('#render-preview')).toContainText('geosite:openai');
  await page.getByRole('button', { name: '保存配置' }).click();
  await expect(page.locator('#managed-xray')).toContainText('已保存 extra_json');

  // JSON mode shows the same data the form built (two-way sync).
  await page.locator('#custom-xray-config').getByRole('button', { name: 'JSON' }).click();
  await expect(page.getByLabel('自定义出站 JSON')).toHaveValue(/pw-up/);
  await expect(page.getByLabel('自定义路由 JSON')).toHaveValue(/geosite:openai/);

  // Invalid custom config must be rejected by the preview with an error.
  await page.getByLabel('自定义路由 JSON').fill('[{"outbound_tag":"ghost","ports":"443"}]');
  await page.getByRole('button', { name: '校验并预览渲染配置' }).click();
  await expect(page.locator('#custom-xray-config')).toContainText('ghost');

  // Broken JSON blocks switching back to form mode instead of dropping it.
  await page.getByLabel('自定义路由 JSON').fill('[{"broken"');
  await page.locator('#custom-xray-config').getByRole('button', { name: '表单' }).click();
  await expect(page.locator('#custom-xray-config')).toContainText('不是合法 JSON');
  await page.getByLabel('自定义路由 JSON').fill('');
  await page.locator('#custom-xray-config').getByRole('button', { name: '表单' }).click();
  await expect(page.getByLabel('路由 domains')).toHaveCount(0);

  await page.getByRole('button', { name: 'Deploy Xray' }).click();
  await clickConfirmOK(page);
  await expect(page.locator('#managed-xray')).toContainText('已提交');
  await page.getByRole('button', { name: '刷新 Jobs' }).first().click();
  await expect(page.locator('tr:visible').filter({ hasText: 'xray_apply' })).toBeVisible();

  await page.locator('input[x-model="rollbackBackup"]').fill('manual-backup.zip');
  await page.getByRole('button', { name: 'Rollback' }).click();
  await clickConfirmOK(page);
  await expect(page.locator('#managed-xray')).toContainText('已提交');

  await page.getByRole('button', { name: '清空 extra_json' }).click();
  await clickConfirmOK(page);
  await expect(page.locator('#managed-xray')).toContainText('已清空 extra_json');

  await page.getByRole('button', { name: '吊销全部证书' }).click();
  await clickConfirmOK(page);
  await expect(page).toHaveURL(/\/api\/v1\/nodes\/\d+\/cert\/revoke$/);
  await expect(page.locator('body')).toContainText('"ok":true');
});

test('traffic, ops, and enforcements page controls are clickable', async ({ page }) => {
  await login(page);
  const username = `pw-traffic-user-${Date.now()}`;
  await createUserFromUsersPage(page, username);
  const nodeName = `pw-traffic-node-${Date.now()}`;
  await createNodeFromNodesPage(page, nodeName, false);

  await page.locator('[data-sidebar-page="traffic"]').click();
  await expect(page.getByRole('heading', { name: '流量分析' })).toBeVisible();
  await page.locator('select[x-model="range"]').selectOption('7d');
  await page.locator('select[x-model="userId"]').selectOption({ label: username });
  await page.locator('select[x-model="nodeId"]').selectOption({ label: nodeName });
  await page.getByRole('button', { name: '刷新' }).click();
  await expect(page.locator('body')).toContainText('暂无趋势数据');

  await page.locator('[data-sidebar-page="ops"]').click();
  await expect(page.getByRole('heading', { name: '运维监控' })).toBeVisible();
  await expect(page.locator('#app-main')).toHaveCount(1);

  await page.locator('[data-sidebar-page="enforcements"]').click();
  await expect(page.getByRole('heading', { name: '执行记录' })).toBeVisible();
  await expect(page.locator('#app-main')).toHaveCount(1);
});
