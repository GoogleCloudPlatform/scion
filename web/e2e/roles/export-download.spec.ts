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
 * E2E: Role Export Download Tests
 *
 * Proves that the export endpoints produce real browser downloads with correct
 * Content-Disposition, Content-Type, filename, envelope version, and role data.
 *
 * These tests use the Playwright download event API to capture actual file
 * artifacts -- NOT just API response assertions.
 */

import { test, expect } from '@playwright/test';
import * as fs from 'node:fs';
import { getE2EEnv } from '../harness/env.js';

// -- Helpers --

interface RoleExportEnvelope {
  version: string;
  exportedAt: string;
  roles: Array<{
    name: string;
    description: string;
    scopeType: string;
    permissions: string[];
  }>;
}

/**
 * Create a custom role via the hub API. Returns the created role.
 */
async function createCustomRole(
  baseURL: string,
  devToken: string,
  name: string,
  description: string,
  permissions: string[],
): Promise<{ id: string; name: string }> {
  const res = await fetch(`${baseURL}/api/v1/admin/roles`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${devToken}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      name,
      description,
      scopeType: 'system',
      permissions,
    }),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`Failed to create role "${name}": ${res.status} ${text}`);
  }
  return (await res.json()) as { id: string; name: string };
}

/**
 * Delete a custom role via the hub API.
 */
async function deleteCustomRole(
  baseURL: string,
  devToken: string,
  roleId: string,
): Promise<void> {
  await fetch(`${baseURL}/api/v1/admin/roles/${roleId}`, {
    method: 'DELETE',
    headers: { Authorization: `Bearer ${devToken}` },
  });
}

// -- Tests --

test.describe('Role export -- browser download', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  let createdRoleIds: string[] = [];

  test.afterEach(async () => {
    // Clean up roles created during the test
    for (const id of createdRoleIds) {
      await deleteCustomRole(env.baseURL, env.devToken, id);
    }
    createdRoleIds = [];
  });

  test('list export produces a real browser download with correct filename and content', async ({
    page,
  }) => {
    // Seed a custom role
    const role = await createCustomRole(
      env.baseURL,
      env.devToken,
      'e2e-export-list-test',
      'Role for list export E2E test',
      ['agent.read', 'project.read'],
    );
    createdRoleIds.push(role.id);

    // Navigate to the roles page
    await page.goto('/admin/roles', { waitUntil: 'domcontentloaded' });

    // Wait for the roles table to load
    await expect(page.getByText('e2e-export-list-test')).toBeVisible({
      timeout: 15_000,
    });

    // Click the "Export Custom Roles" button and capture the download event.
    // The button uses window.location.href navigation, which triggers a
    // Content-Disposition: attachment response from the server.
    const downloadPromise = page.waitForEvent('download', { timeout: 15_000 });
    await page.getByRole('button', { name: /Export Custom Roles/i }).click();
    const download = await downloadPromise;

    // Verify the suggested filename
    expect(download.suggestedFilename()).toBe('scion-custom-roles.json');

    // Save the download and verify the content
    const downloadPath = await download.path();
    expect(downloadPath).toBeTruthy();

    const content = fs.readFileSync(downloadPath!, 'utf-8');
    const envelope: RoleExportEnvelope = JSON.parse(content);

    // Validate envelope structure
    expect(envelope.version).toBe('1');
    expect(envelope.exportedAt).toBeTruthy();
    expect(Array.isArray(envelope.roles)).toBe(true);

    // Our test role must be in the export
    const testRole = envelope.roles.find(
      (r) => r.name === 'e2e-export-list-test',
    );
    expect(testRole).toBeTruthy();
    expect(testRole!.permissions).toEqual(
      expect.arrayContaining(['agent.read', 'project.read']),
    );
  });

  test('single-role export produces a real browser download with per-role filename', async ({
    page,
  }) => {
    // Seed a custom role
    const role = await createCustomRole(
      env.baseURL,
      env.devToken,
      'e2e-single-export-test',
      'Role for single export E2E test',
      ['agent.read'],
    );
    createdRoleIds.push(role.id);

    // Navigate to the roles page
    await page.goto('/admin/roles', { waitUntil: 'domcontentloaded' });

    // Wait for the roles table to load
    await expect(page.getByText('e2e-single-export-test')).toBeVisible({
      timeout: 15_000,
    });

    // Click the per-role download (export) button.
    const roleRow = page.locator('tr', {
      has: page.getByText('e2e-single-export-test'),
    });
    const downloadPromise = page.waitForEvent('download', { timeout: 15_000 });
    await roleRow.getByLabel('Export role').click();
    const download = await downloadPromise;

    // Verify filename
    expect(download.suggestedFilename()).toBe(
      'scion-role-e2e-single-export-test.json',
    );

    // Save and validate content
    const downloadPath = await download.path();
    expect(downloadPath).toBeTruthy();

    const content = fs.readFileSync(downloadPath!, 'utf-8');
    const envelope: RoleExportEnvelope = JSON.parse(content);

    expect(envelope.version).toBe('1');
    expect(envelope.exportedAt).toBeTruthy();
    expect(envelope.roles).toHaveLength(1);
    expect(envelope.roles[0].name).toBe('e2e-single-export-test');
  });

  test('export API returns correct Content-Type and Content-Disposition headers', async ({
    request,
  }) => {
    // Seed a custom role
    const role = await createCustomRole(
      env.baseURL,
      env.devToken,
      'e2e-header-test',
      'Role for header E2E test',
      ['agent.read'],
    );
    createdRoleIds.push(role.id);

    // Test list export headers
    const listRes = await request.get('/api/v1/admin/roles/export');
    expect(listRes.status()).toBe(200);
    expect(listRes.headers()['content-type']).toBe('application/json');
    expect(listRes.headers()['content-disposition']).toContain('attachment');
    expect(listRes.headers()['content-disposition']).toContain(
      'scion-custom-roles.json',
    );

    // Test single-role export headers
    const singleRes = await request.get(
      `/api/v1/admin/roles/${role.id}/export`,
    );
    expect(singleRes.status()).toBe(200);
    expect(singleRes.headers()['content-type']).toBe('application/json');
    expect(singleRes.headers()['content-disposition']).toContain('attachment');
    expect(singleRes.headers()['content-disposition']).toContain(
      'scion-role-e2e-header-test.json',
    );
  });

  test('system role export is rejected with 422', async ({ request }) => {
    // List roles to find a system role
    const rolesRes = await request.get('/api/v1/admin/roles');
    const rolesData = (await rolesRes.json()) as {
      items: Array<{ id: string; system: boolean; name: string }>;
    };
    const systemRole = rolesData.items.find((r) => r.system);
    expect(systemRole).toBeTruthy();

    const exportRes = await request.get(
      `/api/v1/admin/roles/${systemRole!.id}/export`,
    );
    expect(exportRes.status()).toBe(422);
    const body = await exportRes.json();
    expect(body.code).toBe('system_role');
  });
});
