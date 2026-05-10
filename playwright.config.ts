import { defineConfig, devices } from '@playwright/test';

const port = Number(process.env.E2E_PORT || 18080);
const host = process.env.E2E_HOST || '127.0.0.1';
const baseURL = process.env.E2E_BASE_URL || `http://${host}:${port}`;

export default defineConfig({
  testDir: './tests/e2e',
  timeout: 45_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },
  webServer: {
    command: [
      'rm -rf .tmp-e2e',
      'mkdir -p .tmp-e2e/backups',
      [
        `ADDR=${host}:${port}`,
        'DB_PATH=.tmp-e2e/neutrino-e2e.db',
        'BACKUP_DIR=.tmp-e2e/backups',
        `SUB_BASE_URL=${baseURL}`,
        'ADMIN_USER=e2e-admin',
        'ADMIN_PASS=e2e-pass',
        'ALLOW_BASIC_AUTH=false',
        'PANEL_TZ=UTC',
        'NODE_DEFAULT_PANEL_MTLS_URL=https://panel-mtls.example.test',
        'NODE_DEFAULT_AGENT_IMAGE=neutrino-agent:e2e',
        'NODE_DEFAULT_XRAY_IMAGE=ghcr.io/xtls/xray-core:26.2.6',
        'go run ./cmd/server',
      ].join(' '),
    ].join(' && '),
    url: baseURL + '/healthz',
    timeout: 120_000,
    reuseExistingServer: false,
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
