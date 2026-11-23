// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { expect, test, type Page } from '@playwright/test';

const COMMIT_SHA = '0123456789abcdef0123456789abcdef01234567';

interface MockRun {
  id: string;
  operationKey: string;
  status: string;
  startedAt: string;
  completedAt?: string;
  startedBy: string;
  log: string;
  result: string;
}

function failedPreflightRun(): MockRun {
  return {
    id: 'run-fail-1',
    operationKey: 'rebuild-server',
    status: 'failed',
    startedAt: '2026-11-21T12:00:00Z',
    completedAt: '2026-11-21T12:01:00Z',
    startedBy: 'admin@example.com',
    log: '==> Preflight checks\n[BLOCK] settings.profiles.missing: profile dev missing',
    result: JSON.stringify({
      branch: 'main',
      branchPinnable: true,
      commit: COMMIT_SHA,
      error:
        'preflight found 1 blocking finding(s); review them on the run detail and re-run with force',
      preflight: {
        version: 1,
        checks: ['settings.profiles'],
        blocking: 1,
        warnings: 0,
        findings: [
          {
            check: 'settings.profiles',
            code: 'settings.profiles.missing',
            severity: 'block',
            message: 'profile dev is referenced by 2 agents',
            remediation: 'restore profile dev in settings',
            subjects: ['agent-a', 'agent-b'],
            subjectCount: 2,
            data: { profile: 'dev', project: 'p1', agents: 2 },
          },
        ],
      },
    }),
  };
}

async function setupMockHub(page: Page, runs: MockRun[], onForcePost: (body: unknown) => void) {
  await page.addInitScript(() => {
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      onerror: (() => void) | null = null;
      readyState = 1;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'user-admin-1',
        email: 'admin@example.com',
        name: 'Admin User',
        role: 'admin',
      }),
    })
  );

  await page.route('**/api/**', (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const path = url.pathname;

    if (path === '/api/v1/system/status') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ complete: true }),
      });
    }
    if (path === '/api/v1/auth/admin-status' || path === '/api/v1/admin/status') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          isAdmin: true,
          isSuperAdmin: true,
          permissions: ['*'],
        }),
      });
    }
    if (path === '/api/v1/experiments') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ experiments: {} }),
      });
    }
    if (path === '/api/v1/settings/public') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({}),
      });
    }
    if (path === '/api/v1/admin/maintenance') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ enabled: false, message: '' }),
      });
    }
    if (path === '/api/v1/admin/maintenance/operations') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          operations: [
            {
              id: 'op-rebuild-server',
              key: 'rebuild-server',
              title: 'Rebuild Server',
              description: 'Rebuild hub binary from git',
              category: 'operation',
              status: 'idle',
              createdAt: '2026-11-21T00:00:00Z',
              lastRun: runs[0],
            },
          ],
          migrations: [],
        }),
      });
    }
    if (path === '/api/v1/admin/maintenance/operations/rebuild-server/runs') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ runs }),
      });
    }
    if (path.startsWith('/api/v1/admin/maintenance/operations/rebuild-server/runs/')) {
      const id = path.split('/').pop();
      const found = runs.find((r) => r.id === id) ?? runs[0];
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(found),
      });
    }
    if (
      path === '/api/v1/admin/maintenance/operations/rebuild-server/run' &&
      req.method() === 'POST'
    ) {
      const parsed: unknown = JSON.parse(req.postData() ?? '{}');
      onForcePost(parsed);
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ runId: 'run-forced-2', status: 'running' }),
      });
    }
    if (path === '/api/v1/admin/maintenance/update-status') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          current: {
            version: 'git-old',
            channel: 'nightly',
            commit: '111111111111',
            isGitBuild: true,
          },
          mode: { effective: 'git', repoConfigured: false, repoBranch: 'main' },
          channel: { effective: 'nightly', source: 'default' },
          pin: '',
          installer: { strategy: 'symlink', supported: true },
          updatePolicy: 'manual',
          downgrade: { floor: '', snapshotsSupported: true },
          pendingUpdate: null,
          lastUpdate: null,
        }),
      });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });
}

test('admin reviews blocking preflight findings and confirms force re-run', async ({ page }) => {
  const runs = [failedPreflightRun()];
  let postedBody: unknown = null;
  await setupMockHub(page, runs, (body) => {
    postedBody = body;
  });

  await page.goto('/admin/maintenance', { waitUntil: 'domcontentloaded' });
  await expect(page.getByText('Rebuild Server')).toBeVisible();

  // Expand History and click the failed run row to open Run Detail.
  await page.getByRole('button', { name: /History/i }).click();
  await page.locator('tr', { hasText: 'failed' }).first().click();

  // Findings table is visible with the blocking finding.
  await expect(page.getByText('settings.profiles.missing', { exact: true })).toBeVisible();
  await expect(page.getByText('profile dev is referenced by 2 agents')).toBeVisible();

  // Click "Re-run with force" to open confirm dialog.
  await page.getByRole('button', { name: /Re-run with force/i }).click();
  await expect(
    page.getByText(`This builds commit ${COMMIT_SHA} on main, not the branch tip.`)
  ).toBeVisible();

  // Confirm button is disabled until checkbox is checked.
  const confirmBtn = page.getByRole('button', { name: /Confirm force re-run/i });
  await expect(confirmBtn).toBeDisabled();

  await page.locator('sl-checkbox').locator('[part="control"]').click();
  await expect(confirmBtn).toBeEnabled();

  await confirmBtn.click();
  await expect
    .poll(() => postedBody)
    .toEqual({
      params: { force: 'true', rerunOf: 'run-fail-1' },
    });
});
