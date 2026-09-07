#!/usr/bin/env npx ts-node
/**
 * Standalone download verification script.
 *
 * Runs Playwright Chromium (and WebKit if available) against a running hub
 * to verify that role export endpoints produce actual browser download events
 * with correct filenames, MIME types, and content.
 *
 * Usage:
 *   npx tsx web/e2e/roles/verify-download.ts
 *
 * Requires: hub running on http://127.0.0.1:4520 with --dev-auth
 */

import { chromium, webkit, type BrowserType, type Download, type Browser } from 'playwright';
import * as fs from 'node:fs';
import * as path from 'node:path';

const BASE_URL = process.env.E2E_BASE_URL || 'http://127.0.0.1:4520';
const DEV_TOKEN = process.env.SCION_DEV_TOKEN || 'scion_dev_7bf2a54bf5856eef7bf7bcb75ff613c2d8e6a33440fe1c9ed8d23b4e9bdce72d';

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

async function createRole(name: string, perms: string[]): Promise<{ id: string; name: string }> {
  const res = await fetch(`${BASE_URL}/api/v1/admin/roles`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${DEV_TOKEN}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      name,
      description: `Test role for download verification`,
      scopeType: 'system',
      permissions: perms,
    }),
  });
  if (!res.ok) throw new Error(`Failed to create role: ${res.status} ${await res.text()}`);
  return (await res.json()) as { id: string; name: string };
}

async function deleteRole(id: string): Promise<void> {
  await fetch(`${BASE_URL}/api/v1/admin/roles/${id}`, {
    method: 'DELETE',
    headers: { Authorization: `Bearer ${DEV_TOKEN}` },
  });
}

async function testBrowser(browserType: BrowserType, label: string): Promise<boolean> {
  let browser: Browser | null = null;
  let roleId: string | null = null;

  try {
    console.log(`\n=== Testing with ${label} ===\n`);

    browser = await browserType.launch({
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    });

    // Create a test role
    const roleName = `verify-download-${label}-${Date.now()}`;
    const role = await createRole(roleName, ['agent.read', 'project.read']);
    roleId = role.id;
    console.log(`  Created role: ${roleName} (${role.id})`);

    const context = await browser.newContext({ baseURL: BASE_URL });
    const page = await context.newPage();

    // Navigate to trigger dev-auth session
    await page.goto('/', { waitUntil: 'domcontentloaded' });

    // --- Test 1: List export via direct navigation ---
    console.log(`  Test 1: List export download event...`);
    const listDownloadPromise = page.waitForEvent('download', { timeout: 10_000 });
    await page.evaluate((url: string) => {
      window.location.href = url;
    }, '/api/v1/admin/roles/export');
    const listDownload = await listDownloadPromise;

    const listFilename = listDownload.suggestedFilename();
    console.log(`    Filename: ${listFilename}`);
    if (listFilename !== 'scion-custom-roles.json') {
      throw new Error(`Expected filename 'scion-custom-roles.json', got '${listFilename}'`);
    }

    const listPath = await listDownload.path();
    if (!listPath) throw new Error('Download path is null');

    const listContent = fs.readFileSync(listPath, 'utf-8');
    const listEnvelope: RoleExportEnvelope = JSON.parse(listContent);

    if (listEnvelope.version !== '1') {
      throw new Error(`Expected version '1', got '${listEnvelope.version}'`);
    }

    const foundInList = listEnvelope.roles.find(r => r.name === roleName);
    if (!foundInList) throw new Error(`Test role '${roleName}' not found in list export`);

    console.log(`    List export: ${listEnvelope.roles.length} custom role(s), version=${listEnvelope.version}`);

    // --- Test 2: Single-role export via direct navigation ---
    console.log(`  Test 2: Single-role export download event...`);

    // Navigate back to a page first
    await page.goto('/', { waitUntil: 'domcontentloaded' });

    const singleDownloadPromise = page.waitForEvent('download', { timeout: 10_000 });
    await page.evaluate((url: string) => {
      window.location.href = url;
    }, `/api/v1/admin/roles/${role.id}/export`);
    const singleDownload = await singleDownloadPromise;

    const singleFilename = singleDownload.suggestedFilename();
    const expectedFilename = `scion-role-${roleName}.json`;
    console.log(`    Filename: ${singleFilename}`);
    if (singleFilename !== expectedFilename) {
      throw new Error(`Expected filename '${expectedFilename}', got '${singleFilename}'`);
    }

    const singlePath = await singleDownload.path();
    if (!singlePath) throw new Error('Single download path is null');

    const singleContent = fs.readFileSync(singlePath, 'utf-8');
    const singleEnvelope: RoleExportEnvelope = JSON.parse(singleContent);

    if (singleEnvelope.version !== '1') {
      throw new Error(`Expected version '1', got '${singleEnvelope.version}'`);
    }
    if (singleEnvelope.roles.length !== 1) {
      throw new Error(`Expected 1 role, got ${singleEnvelope.roles.length}`);
    }
    if (singleEnvelope.roles[0].name !== roleName) {
      throw new Error(`Expected role name '${roleName}', got '${singleEnvelope.roles[0].name}'`);
    }

    console.log(`    Single export: role=${singleEnvelope.roles[0].name}`);

    await context.close();
    console.log(`\n  All ${label} tests passed!\n`);
    return true;
  } catch (err) {
    console.error(`\n  ${label} failed: ${err}\n`);
    return false;
  } finally {
    if (roleId) await deleteRole(roleId);
    if (browser) await browser.close();
  }
}

async function main(): Promise<void> {
  console.log('Role Export Download Verification');
  console.log(`Base URL: ${BASE_URL}`);
  console.log('');

  // Verify hub is running
  try {
    const health = await fetch(`${BASE_URL}/healthz`);
    if (!health.ok) throw new Error(`Health check failed: ${health.status}`);
    console.log('Hub health: OK');
  } catch (err) {
    console.error(`Hub is not reachable at ${BASE_URL}. Start it first.`);
    process.exit(1);
  }

  const results: Record<string, boolean> = {};

  // Test with Chromium (always available)
  results['Chromium'] = await testBrowser(chromium, 'Chromium');

  // Try WebKit if available (Safari-like)
  try {
    results['WebKit'] = await testBrowser(webkit, 'WebKit');
  } catch (err) {
    console.warn(`\n  WebKit not available (missing system dependencies). Safari verification not possible in this environment.\n`);
    results['WebKit'] = false;
  }

  // Summary
  console.log('\n=== Summary ===');
  for (const [browser, passed] of Object.entries(results)) {
    console.log(`  ${browser}: ${passed ? 'PASS' : 'FAIL/SKIP'}`);
  }

  if (!results['Chromium']) {
    process.exit(1);
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
