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
 * Real-browser coverage for lazy file-tab mounting (ptone/scion#2381, AC3).
 *
 * The Vitest/happy-dom component tests in
 * src/components/pages/project-detail-files.test.ts drive tab switching by
 * dispatching a synthetic `sl-tab-show` event on `<sl-tab-group>` directly,
 * because Shoelace custom elements are never upgraded in that environment.
 * That proves the `@sl-tab-show=${this.onFileTabChange}` template binding
 * is wired correctly, but not Shoelace's own tab-group behavior — a real
 * click and its resulting `active` attribute mutation and sl-tab-show
 * emission. This spec runs the real, compiled component against a real,
 * registered Shoelace `<sl-tab-group>` in a real browser to close that gap:
 * one listing request for the initially-active tab, a real click switching
 * to a second tab (mounting and loading it), the never-opened third tab
 * staying untouched throughout (including across the editor round trip,
 * which remounts every *previously visited* tab), the file editor's
 * open/Back round trip, and the Files section's viewport-deferred reveal
 * (a below-the-fold placeholder that mounts nothing and requests nothing
 * until scrolled near).
 *
 * This spec does not replay the round-1 SSE active-shared-dir-removal
 * regression. normalizeActiveFileTab() corrects `activeFileTab` in
 * willUpdate(), before render, so the component never hands Shoelace an
 * `?active=` binding naming a tab that doesn't exist in the first place —
 * Shoelace's own tabs[0] fallback is what the *bug* relied on, not what the
 * fix does. The Vitest regression tests for that scenario check the
 * consequence directly (the correct tab is what actually mounts), which
 * holds regardless of Shoelace being registered.
 */

import { test, expect } from '@playwright/test';
import { installApiMocks, SHARED_DIR_A, SHARED_DIR_B } from './mock-api.js';

test.describe('project-detail Files tabs — real Shoelace tab-group', () => {
  test('loads the active tab once, switches tabs via a real click, and round-trips the editor', async ({
    page,
  }) => {
    const counts = await installApiMocks(page);
    await page.goto('/e2e/project-files-tabs/fixture.html', { waitUntil: 'domcontentloaded' });

    // The Files section may start as a placeholder until IntersectionObserver
    // reports it visible; make sure it's in view rather than relying on the
    // fixture happening to fit inside the default viewport.
    const placeholder = page.locator('.files-section-placeholder');
    if (await placeholder.count()) {
      await placeholder.scrollIntoViewIfNeeded();
    }

    // Tab 1 (workspace, the default active tab) mounts and issues exactly
    // one listing request — this branch is stacked on ptone/scion#2380's
    // dedup fix.
    const workspaceBrowser = page.locator('scion-file-browser[data-tab="workspace"]');
    await expect(workspaceBrowser).toBeAttached({ timeout: 10_000 });
    await expect.poll(() => counts.workspaceListings).toBe(1);

    // Tab 2 (shared-a) is not mounted, and made no request, before it's opened.
    await expect(page.locator(`scion-file-browser[data-tab="${SHARED_DIR_A}"]`)).toHaveCount(0);
    expect(counts.sharedDirListings[SHARED_DIR_A]).toBe(0);

    // Click tab 2 for real — a genuine Shoelace <sl-tab> activation, which
    // internally sets the `active` attribute and emits sl-tab-show with the
    // clicked tab's panel name. This is the exact mechanism round-1's fix
    // depends on, and the happy-dom component tests cannot exercise it.
    await page.locator(`sl-tab[panel="${SHARED_DIR_A}"]`).click();

    const sharedABrowser = page.locator(`scion-file-browser[data-tab="${SHARED_DIR_A}"]`);
    await expect(sharedABrowser).toBeAttached({ timeout: 10_000 });
    await expect.poll(() => counts.sharedDirListings[SHARED_DIR_A]).toBe(1);

    // Tab 2's rows are visible.
    const sharedAFileName = `${SHARED_DIR_A}-file.txt`;
    await expect(sharedABrowser.locator('.file-name', { hasText: sharedAFileName })).toBeVisible();

    // Open that file (the pencil/Edit icon opens the in-page editor; the
    // eye/Preview icon would open a new browser tab for a plain .txt file,
    // which this test doesn't need to handle).
    await sharedABrowser.locator(`sl-icon-button[label="Edit ${sharedAFileName}"]`).click();

    const editor = page.locator('scion-file-editor');
    await expect(editor).toBeAttached({ timeout: 10_000 });
    // Every file browser (and the tab group) unmounts while the editor is open.
    await expect(page.locator('scion-file-browser')).toHaveCount(0);

    // Click Back.
    await page.locator('sl-button', { hasText: 'Back to files' }).click();
    await expect(editor).toHaveCount(0);

    // Tab 2's rows are visible again after returning from the editor.
    await expect(
      page
        .locator(`scion-file-browser[data-tab="${SHARED_DIR_A}"]`)
        .locator('.file-name', { hasText: sharedAFileName })
    ).toBeVisible({ timeout: 10_000 });

    // The never-opened third tab (shared-b) still made no request and is
    // still unmounted, through the entire flow above — including the
    // editor round trip, which remounts every *previously visited* tab but
    // must not touch this one. Settle first: both checks below are a single
    // point-in-time read/first-truthy-poll, which would pass even if a
    // regression mounted shared-b on a later, delayed render (e.g. an async
    // catch-up scheduled after Back) rather than synchronously with it.
    // A fixed wait rather than networkidle: networkidle resolves within
    // ~0.5s here, before a delayed regression like this would fire.
    await page.waitForTimeout(2000);
    expect(counts.sharedDirListings[SHARED_DIR_B]).toBe(0);
    await expect(page.locator(`scion-file-browser[data-tab="${SHARED_DIR_B}"]`)).toHaveCount(0);
  });

  test('defers the Files section until it nears the viewport, then reveals and loads it', async ({
    page,
  }) => {
    const counts = await installApiMocks(page);
    await page.goto('/e2e/project-files-tabs/fixture.html?spacer=1', {
      waitUntil: 'domcontentloaded',
    });

    // The 2000px spacer (see fixture.ts) pushes the whole component below
    // the fold. The placeholder is in the DOM, but nothing has mounted or
    // requested anything yet.
    const placeholder = page.locator('.files-section-placeholder');
    await expect(placeholder).toBeAttached({ timeout: 10_000 });
    await expect(page.locator('scion-file-browser')).toHaveCount(0);
    expect(counts.workspaceListings).toBe(0);
    expect(counts.sharedDirListings[SHARED_DIR_A]).toBe(0);
    expect(counts.sharedDirListings[SHARED_DIR_B]).toBe(0);

    // Give the IntersectionObserver a moment to have observed and reported
    // "not intersecting" at least once, so the reveal below is caused by
    // the scroll, not a race on the observer's first callback.
    await page.waitForTimeout(300);
    await expect(placeholder).toBeAttached();
    expect(counts.workspaceListings).toBe(0);

    // Scroll the placeholder into view — this is what crosses the
    // rootMargin threshold and triggers observeFilesSection()'s reveal.
    await placeholder.scrollIntoViewIfNeeded();

    await expect(placeholder).toHaveCount(0, { timeout: 10_000 });
    await expect(page.locator('scion-file-browser[data-tab="workspace"]')).toBeAttached();
    await expect.poll(() => counts.workspaceListings).toBe(1);
    // Only the active tab reveals — the shared dirs are still untouched.
    expect(counts.sharedDirListings[SHARED_DIR_A]).toBe(0);
    expect(counts.sharedDirListings[SHARED_DIR_B]).toBe(0);
  });
});
