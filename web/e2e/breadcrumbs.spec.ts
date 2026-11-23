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

/**
 * Breadcrumb navigation E2E spec (ptone/scion#8281 / #9021), under the main
 * Playwright config against the real hub started by global-setup.
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv } from './harness/env.js';

interface CreatedProject {
  id: string;
  name: string;
  slug: string;
}

async function createProject(
  baseURL: string,
  devToken: string,
  name: string
): Promise<CreatedProject> {
  const res = await fetch(`${baseURL}/api/v1/projects`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${devToken}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) {
    throw new Error(`POST /api/v1/projects failed (${res.status}): ${await res.text()}`);
  }
  const body = (await res.json()) as CreatedProject | { project?: CreatedProject };
  const project =
    body && typeof body === 'object' && 'project' in body && body.project
      ? body.project
      : (body as CreatedProject);
  return project;
}

async function deleteProject(baseURL: string, devToken: string, id: string): Promise<void> {
  await fetch(`${baseURL}/api/v1/projects/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: { Authorization: `Bearer ${devToken}` },
  });
}

test.describe('Breadcrumb navigation (ptone/scion#8281)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  let project: CreatedProject;

  test.beforeAll(async () => {
    project = await createProject(env.baseURL, env.devToken, `Breadcrumb E2E ${Date.now()}`);
  });

  test.afterAll(async () => {
    if (project?.id) {
      await deleteProject(env.baseURL, env.devToken, project.id);
    }
  });

  test('project page shows Breadcrumb landmark distinct from Main and navigates to /projects', async ({
    page,
  }) => {
    await page.goto(`/projects/${project.id}`, { waitUntil: 'domcontentloaded' });

    const breadcrumb = page.getByRole('navigation', { name: 'Breadcrumb' });
    await expect(breadcrumb).toBeVisible({ timeout: 15_000 });
    await expect(page.getByRole('navigation', { name: 'Main' })).toBeAttached();

    const projectsLink = breadcrumb.getByRole('link', { name: 'Projects' });
    await expect(projectsLink).toBeVisible();
    await expect(breadcrumb.getByText(project.name)).toBeVisible();
    await expect(breadcrumb.locator('[aria-current="page"]')).toHaveText(project.name);

    await projectsLink.click();
    await expect(page).toHaveURL(/\/projects$/);
  });

  test('agent page shows Projects / {project} / {agent} breadcrumb', async ({ page }) => {
    const agentId = 'e2e-crumb-agent-1';
    const agentName = 'crumb-worker-01';

    await page.route(`**/api/v1/agents/${agentId}`, async (route) => {
      if (route.request().method() !== 'GET') {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          id: agentId,
          name: agentName,
          projectId: project.id,
          project: project.name,
          projectSlug: project.slug,
          template: 'default',
          phase: 'running',
          created: '2026-01-01T00:00:00Z',
          updated: '2026-01-01T00:00:00Z',
          messageMode: 'project',
          _capabilities: { actions: ['read'] },
        }),
      });
    });

    await page.goto(`/agents/${agentId}`, { waitUntil: 'domcontentloaded' });

    const breadcrumb = page.getByRole('navigation', { name: 'Breadcrumb' });
    await expect(breadcrumb).toBeVisible({ timeout: 15_000 });
    await expect(breadcrumb.getByRole('link', { name: 'Projects' })).toBeVisible();
    const projectLink = breadcrumb.getByRole('link', { name: project.name });
    await expect(projectLink).toBeVisible();
    await expect(breadcrumb.locator('[aria-current="page"]')).toHaveText(agentName);

    await projectLink.click();
    await expect(page).toHaveURL(new RegExp(`/projects/${project.id}$`));
  });

  test('at 480px width a 5-crumb trail collapses middle items and expands on click', async ({
    page,
  }) => {
    await page.setViewportSize({ width: 480, height: 800 });
    await page.goto(`/projects/${project.id}`, { waitUntil: 'domcontentloaded' });

    // Wait for the lazy breadcrumb component to load and register on the page.
    await expect(page.getByRole('navigation', { name: 'Breadcrumb' })).toBeVisible({
      timeout: 15_000,
    });

    // Mount an isolated 5-crumb scion-breadcrumb in a 480px container so
    // project-detail's own async re-renders cannot overwrite the test trail.
    await page.evaluate((projId: string) => {
      const wrapper = document.createElement('div');
      wrapper.dataset.testid = 'e2e-collapse-fixture';
      wrapper.style.width = '480px';
      wrapper.style.maxWidth = '100%';
      const host = document.createElement('scion-breadcrumb') as HTMLElement & {
        crumbs: Array<Record<string, unknown>>;
      };
      host.crumbs = [
        { label: 'Projects', href: '/projects' },
        { label: 'Payments', href: `/projects/${projId}`, truncate: true },
        { label: 'Settings', href: `/projects/${projId}/settings` },
        { label: 'Templates', href: `/projects/${projId}/settings?tab=templates` },
        { label: 'base-template', truncate: true },
      ];
      wrapper.appendChild(host);
      document.body.prepend(wrapper);
    }, project.id);

    const fixture = page.getByTestId('e2e-collapse-fixture');
    const breadcrumb = fixture.getByRole('navigation', { name: 'Breadcrumb' });
    const expandBtn = breadcrumb.getByRole('button', { name: 'Show full path' });
    await expect(expandBtn).toBeVisible();
    await expect(breadcrumb.getByRole('link', { name: 'Projects' })).toBeVisible();
    await expect(breadcrumb.getByRole('link', { name: 'Templates' })).toBeVisible();
    await expect(breadcrumb.locator('[aria-current="page"]')).toHaveText('base-template');
    await expect(breadcrumb.getByRole('link', { name: 'Payments' })).toBeHidden();
    await expect(breadcrumb.getByRole('link', { name: 'Settings' })).toBeHidden();

    await expandBtn.click();
    await expect(breadcrumb.getByRole('link', { name: 'Payments' })).toBeVisible();
    await expect(breadcrumb.getByRole('link', { name: 'Settings' })).toBeVisible();
    await expect(expandBtn).toBeHidden();
  });
});
