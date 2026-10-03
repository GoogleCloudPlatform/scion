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
 * Real-Chromium coverage for the extracted file preview: message attachment
 * expansion and thread path clicks both use the real extracted renderers
 * (image/text/markdown/code); markdown toggle/copy and explicit binary
 * download are preserved. Close/switch-target aborts pending loads; the same
 * path in another project cannot display the old response; object URLs are
 * released.
 *
 * Both surfaces (chat-message's attachment overlay, chat-thread's path
 * overlay) delegate to one nested-shadow-root `<scion-chat-file-preview>` —
 * a composedPath/event-retargeting scenario happy-dom cannot reproduce,
 * which is what makes this suite necessary alongside the unit tests
 * (chat-file-preview.test.ts, chat-thread.test.ts, chat-message.test.ts).
 */

import { test, expect, type Page } from '@playwright/test';
import {
  ALPHA_NOTES_CONTENT,
  BETA_NOTES_CONTENT,
  setupApiMocks,
  TEXT_ATTACHMENT_BODY,
} from './mock-api.js';
import { PROJECT_A } from './data.js';

async function gotoThread(page: Page) {
  const requests = await setupApiMocks(page);
  await page.goto('/e2e/chat-file-preview/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-chat-thread'));
  // Wait for the five seeded messages to render before interacting.
  await expect(page.locator('scion-chat-thread scion-chat-message')).toHaveCount(5);
  return requests;
}

function previewDialog(page: Page) {
  return page.locator('scion-chat-file-preview sl-dialog.file-preview-dialog');
}

function previewPanel(page: Page) {
  return page.locator('scion-chat-file-preview sl-dialog.file-preview-dialog [part~="panel"]');
}

test('a path link opens the extracted viewer with the resolved project’s content', async ({
  page,
}) => {
  await gotoThread(page);

  await page.locator('.path-link', { hasText: 'notes.md' }).first().click();

  await expect(previewDialog(page)).toBeVisible();
  await expect(previewDialog(page)).toContainText(ALPHA_NOTES_CONTENT);
});

test('the dialog panel is sized for real content, not the sl-dialog default', async ({ page }) => {
  await gotoThread(page);

  // The 1200px fixture viewport makes min(90vw, 900px) resolve to the fixed
  // 900px cap; the un-styled sl-dialog default is 496px (the regression this
  // pins).
  await page.locator('.path-link', { hasText: 'notes.md' }).first().click();
  await expect(previewDialog(page)).toBeVisible();
  const pathBox = await previewPanel(page).boundingBox();
  expect(pathBox?.width).toBeGreaterThan(800);

  await page.keyboard.press('Escape');
  await expect(previewDialog(page)).toBeHidden();

  await page.locator('.image-actions sl-icon-button[name="arrows-angle-expand"]').click();
  await expect(previewDialog(page)).toBeVisible();
  const attachmentBox = await previewPanel(page).boundingBox();
  expect(attachmentBox?.width).toBeGreaterThan(800);
});

test('the same file name in a different project shows that project’s content, not a stale one', async ({
  page,
}) => {
  await gotoThread(page);

  const pathLinks = page.locator('.path-link', { hasText: 'notes.md' });
  await pathLinks.nth(0).click();
  await expect(previewDialog(page)).toContainText(ALPHA_NOTES_CONTENT);

  // The thread owns a single shared preview instance, so the first dialog
  // must actually close (sl-after-hide -> closeFilePreview) before reusing
  // it for the second path.
  await page.keyboard.press('Escape');
  await expect(previewDialog(page)).toBeHidden();

  await pathLinks.nth(1).click();
  await expect(previewDialog(page)).toBeVisible();
  await expect(previewDialog(page)).toContainText(BETA_NOTES_CONTENT);
  await expect(previewDialog(page)).not.toContainText(ALPHA_NOTES_CONTENT);
});

test('an attachment image expands via a fetched object URL and can be closed with Escape', async ({
  page,
}) => {
  await gotoThread(page);

  // The image thumbnail and its hover toolbar visually overlap by design;
  // click the explicit expand action rather than the thumbnail itself.
  await page.locator('.image-actions sl-icon-button[name="arrows-angle-expand"]').click();
  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();

  const img = dialog.locator('img.file-preview-image');
  await expect(img).toBeVisible();
  const src = await img.getAttribute('src');
  expect(src).toMatch(/^blob:/);

  // The object URL is revoked on close — assert the dialog actually goes
  // away, which only happens once chat-message's `expanded` state clears via
  // the composed chat-file-preview-close event crossing its shadow boundary.
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
});

test('a markdown attachment renders as preview by default, toggles to source, and copy works', async ({
  page,
}) => {
  await gotoThread(page);

  // The markdown attachment ("plan.md") sits in the download-chip-free
  // previewable group; open its expand control specifically.
  const mdPreview = page.locator('.attachment-preview', {
    has: page.locator('.preview-filename', { hasText: 'plan.md' }),
  });
  await mdPreview.locator('sl-icon-button[name="arrows-angle-expand"]').click();

  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog.locator('scion-markdown-preview')).toBeVisible();

  await dialog.locator('sl-button', { hasText: 'Source' }).click();
  await expect(dialog.locator('scion-code-editor')).toBeVisible();
  await expect(dialog).toContainText(TEXT_ATTACHMENT_BODY.trim().split('\n')[0]);

  await dialog.locator('sl-button', { hasText: 'Copy' }).click();
  await expect(dialog.locator('sl-button', { hasText: 'Copied!' })).toBeVisible();
});

test('a binary/non-previewable attachment stays a download chip, never an inline overlay', async ({
  page,
}) => {
  await gotoThread(page);

  // archive.zip is neither an image nor text-previewable, so it must render
  // as chat-message's plain `.download-chip` — no expand button, no overlay
  // — with its href pointing at the real attachment endpoint.
  const downloadChip = page.locator('.download-chip', { hasText: 'archive.zip' });
  await expect(downloadChip).toBeVisible();
  await expect(downloadChip).toHaveAttribute('href', /\/api\/v1\/chat\/attachments\//);
  // No preview dialog exists until something is actually clicked open.
  await expect(previewDialog(page)).toHaveCount(0);
});

test('switching targets aborts the pending load: rapid double-click never shows the first path’s content on the second', async ({
  page,
}) => {
  await gotoThread(page);

  // Delay project A's response (fulfilling it directly, since the generic
  // mock route already registered by setupApiMocks would otherwise win) so
  // it is still in-flight when the second click, for project B, fires.
  await page.route(`**/api/v1/projects/${PROJECT_A}/workspace/files/notes.md**`, async (route) => {
    await new Promise((r) => setTimeout(r, 300));
    await route.fulfill({
      json: { content: ALPHA_NOTES_CONTENT, size: ALPHA_NOTES_CONTENT.length },
    });
  });

  const failedRequests: Array<{ url: string; errorText: string | null }> = [];
  page.on('requestfailed', (req) =>
    failedRequests.push({ url: req.url(), errorText: req.failure()?.errorText ?? null })
  );

  // sl-dialog is modal once open — a real user cannot click a second link on
  // the page behind it without closing the first. The race this proves
  // (`handlePathLinkClick` retargeting the shared preview mid-load) is real
  // regardless: `dispatchEvent('click')` fires the same click handler
  // without Playwright's pointer-interception actionability gate, which
  // exists to catch *accidental* overlap, not to forbid a legitimate
  // programmatic re-click while a dialog covers the page.
  const pathLinks = page.locator('.path-link', { hasText: 'notes.md' });
  await pathLinks.nth(0).dispatchEvent('click');
  await page.waitForTimeout(50); // let the first (slow) request start, but not finish
  await pathLinks.nth(1).dispatchEvent('click');

  await expect(previewDialog(page)).toContainText(BETA_NOTES_CONTENT, { timeout: 5000 });
  await expect(previewDialog(page)).not.toContainText(ALPHA_NOTES_CONTENT);

  // Not just "the stale content never showed" — the first request itself
  // must actually have been aborted (not merely out-raced), i.e. the
  // controller.abort() call, not the generation check alone, did the work.
  await expect
    .poll(() =>
      failedRequests.find((r) => r.url.includes(`/projects/${PROJECT_A}/workspace/files/notes.md`))
    )
    .toMatchObject({ errorText: 'net::ERR_ABORTED' });
});

test('a path pointing at an image renders it inline, not as a text/code view', async ({ page }) => {
  await gotoThread(page);

  await page.locator('.path-link', { hasText: 'diagram.png' }).click();
  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();

  const img = dialog.locator('img.file-preview-image');
  await expect(img).toBeVisible();
  expect(await img.getAttribute('src')).toMatch(/^blob:/);
  await expect(dialog.locator('scion-code-editor')).toHaveCount(0);
});

test('a path whose real content exceeds the inline limit shows download-only, inside the viewer', async ({
  page,
}) => {
  await gotoThread(page);

  await page.locator('.path-link', { hasText: 'huge.log' }).click();
  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("can't be shown here");
  await expect(dialog.locator('scion-code-editor')).toHaveCount(0);
  await expect(dialog.locator('sl-button', { hasText: 'Download' })).toHaveAttribute(
    'href',
    /\/workspace\/files\/huge\.log/
  );
});

test('closing while a load is in flight aborts the request rather than letting it resolve into a closed dialog', async ({
  page,
}) => {
  await gotoThread(page);

  // The delay must comfortably outlast sl-dialog's own close animation:
  // toBeHidden() below can resolve once the dialog is visually faded, which
  // may be *before* sl-after-hide actually fires and calls abort() — too
  // short a delay here flakes by letting the mock response land in that gap.
  await page.route(`**/api/v1/projects/${PROJECT_A}/workspace/files/notes.md**`, async (route) => {
    await new Promise((r) => setTimeout(r, 1500));
    await route.fulfill({
      json: { content: ALPHA_NOTES_CONTENT, size: ALPHA_NOTES_CONTENT.length },
    });
  });
  const failedRequests: Array<{ url: string; errorText: string | null }> = [];
  page.on('requestfailed', (req) =>
    failedRequests.push({ url: req.url(), errorText: req.failure()?.errorText ?? null })
  );

  await page.locator('.path-link', { hasText: 'notes.md' }).first().click();
  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText('Loading file');

  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();

  // Let the slow response land after close; it must not reopen the dialog,
  // flash an error, or otherwise resurrect state for a target no longer set.
  await page.waitForTimeout(2000);
  await expect(dialog).toBeHidden();
  await expect
    .poll(() => failedRequests.find((r) => r.url.includes('workspace/files/notes.md')))
    .toMatchObject({ errorText: 'net::ERR_ABORTED' });
});

test('the object URL is revoked on close — fetching it afterward fails', async ({ page }) => {
  await gotoThread(page);

  await page.locator('.image-actions sl-icon-button[name="arrows-angle-expand"]').click();
  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  const src = await dialog.locator('img.file-preview-image').getAttribute('src');
  expect(src).toMatch(/^blob:/);
  const objectUrl = src!;

  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();

  const stillReadable = await page.evaluate(async (blobUrl) => {
    try {
      const res = await fetch(blobUrl);
      return res.ok;
    } catch {
      return false;
    }
  }, objectUrl);
  expect(stillReadable).toBe(false);
});
