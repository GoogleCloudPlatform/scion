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
 * Real-Chromium coverage for gs:// link detection and the gcs preview
 * target. happy-dom diverges from Chromium on marked+DOMPurify output (see
 * chat-file-links.test.ts / chat-message.test.ts for what unit tests already
 * cover), so the linkifier and the hostile-name cases need a real browser.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupGcsApiMocks, type TrackedRequest } from './mock-api.js';
import {
  GCS_BUCKET,
  GCS_XPROJECT_BUCKET,
  GCS_XPROJECT_OBJECT,
  GCS_MARKDOWN_BODY,
  GCS_JSON_BODY,
  GCS_MESSAGES,
  GCS_TEXT_AND_CODE_MSG,
  GCS_TRAILING_PUNCT_MSG,
  GCS_DIR_MSG,
  GCS_NOLINK_SCHEME_MSG,
  GCS_START_MSG,
  GCS_AFTER_CODE_MSG,
  GCS_WORKSPACE_COLLISION_MSG,
  GCS_HOSTILE_QUOTE_MSG,
  GCS_HOSTILE_IMG_MSG,
  GCS_HOSTILE_AMP_MSG,
  GCS_USER_OWN_MSG,
  GCS_XPROJECT_MSG,
  GCS_JSON_MSG,
  GCS_NOTFOUND_MSG,
  GCS_DENIED_MSG,
  GCS_SHORTLINK_MSG,
  GCS_FRAGMENT_HASH_MSG,
  GCS_FRAGMENT_QUERY_MSG,
  GCS_QUOTED_MSG,
  GCS_OTHER_USER_MSG,
  GCS_EMPHASIS_MSG,
  GCS_STRIKETHROUGH_MSG,
  GCS_TAG_SPLIT_QUESTION_MSG,
  GCS_RAW_ENTITY_MSG,
  GCS_BACKSLASH_ESCAPE_MSG,
  GCS_DOUBLE_STAR_MSG,
  GCS_RAW_LT_MSG,
  GCS_RAW_QUOT_MSG,
  GCS_BINARY_NOTFOUND_MSG,
  GCS_TRAILING_PAREN_MSG,
  GCS_FENCED_BLOCK_MSG,
  GCS_IMAGE_PNG_MSG,
  GCS_IMAGE_JPEG_MSG,
  GCS_IMAGE_GIF_MSG,
  GCS_IMAGE_WEBP_MSG,
  GCS_SVG_MSG,
  GCS_HTML_AS_PNG_MSG,
  GCS_OCTET_STREAM_MSG,
  GCS_TOO_LARGE_INLINE_MSG,
  GCS_413_MSG,
} from './data.js';

async function gotoGcsThread(page: Page): Promise<TrackedRequest[]> {
  await page.addInitScript(() => {
    (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
      'web.gcs_links': true,
    };
  });
  const requests = await setupGcsApiMocks(page);
  await page.goto('/e2e/chat-file-preview/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-chat-thread'));
  await expect(page.locator('scion-chat-thread scion-chat-message')).toHaveCount(
    GCS_MESSAGES.length
  );
  return requests;
}

function messageLocator(page: Page, id: string) {
  return page.locator(`#msg-${id}`);
}

function previewDialog(page: Page) {
  return page.locator('scion-chat-file-preview sl-dialog.file-preview-dialog');
}

test('renders as plain text when web.gcs_links is pinned off, even for an agent message', async ({
  page,
}) => {
  await page.addInitScript(() => {
    (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
      'web.gcs_links': false,
    };
  });
  await setupGcsApiMocks(page);
  await page.goto('/e2e/chat-file-preview/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-chat-thread'));
  await expect(page.locator('scion-chat-thread scion-chat-message')).toHaveCount(
    GCS_MESSAGES.length
  );

  const msg = messageLocator(page, GCS_START_MSG.id);
  await expect(msg).toContainText(`gs://${GCS_BUCKET}/start.txt`);
  // Wait for this message's markdown render to replace its plain-text
  // fallback (.md-content exists only after chat-message.ts's asynchronous
  // renderContent has finished) before asserting the absence of a
  // sub-element: the plain-text fallback already contains the same text, so
  // a text check alone cannot tell the two states apart.
  await expect(msg.locator('.md-content')).toHaveCount(1);
  await expect(msg.locator('.gcs-link')).toHaveCount(0);
});

test('renders as plain text when the web.gcs_links experiment is off, with no local pin (ptone/scion#2545)', async ({
  page,
}) => {
  // No __SCION_FEATURES__ init script here: this drives the real
  // /api/v1/experiments fetch path end to end, rather than bypassing it via
  // a pinned value, complementing the pinned-off case above.
  await setupGcsApiMocks(page, { gcsLinksExperimentEnabled: false });
  await page.goto('/e2e/chat-file-preview/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-chat-thread'));
  await expect(page.locator('scion-chat-thread scion-chat-message')).toHaveCount(
    GCS_MESSAGES.length
  );

  const msg = messageLocator(page, GCS_START_MSG.id);
  await expect(msg).toContainText(`gs://${GCS_BUCKET}/start.txt`);
  // Same render-wait-before-absence-check pattern as the pinned-off case
  // above (lesson 3): a not-yet-rendered message would otherwise satisfy the
  // absence check for the wrong reason.
  await expect(msg.locator('.md-content')).toHaveCount(1);
  await expect(msg.locator('.gcs-link')).toHaveCount(0);
});

test('links a gs:// URI in plain text and inside inline code', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_TEXT_AND_CODE_MSG.id);
  const links = msg.locator('.gcs-link');
  await expect(links).toHaveCount(2);
  for (const i of [0, 1]) {
    await expect(links.nth(i)).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/dir/file.md`);
  }
  await expect(msg.locator('code .gcs-link')).toHaveCount(1);
});

test('excludes trailing comma and period from the link', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_TRAILING_PUNCT_MSG.id);
  const links = msg.locator('.gcs-link');
  await expect(links).toHaveCount(2);
  await expect(links.nth(0)).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/dir/file.md`);
  await expect(links.nth(1)).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/dir/file.md`);
});

test('excludes a trailing, unmatched ")" from the link', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_TRAILING_PAREN_MSG.id);
  // Positive assertion (a link is present), so no render-wait is needed
  // before it — see the absence-check convention noted above.
  const links = msg.locator('.gcs-link');
  await expect(links).toHaveCount(1);
  await expect(links.first()).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/dir/file.md`);
  await expect(msg).toContainText(`(see gs://${GCS_BUCKET}/dir/file.md)`);
});

test('a gs:// URI inside a fenced code block does not link', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_FENCED_BLOCK_MSG.id);
  // Wait for the fenced block's own <pre> before asserting absence — the
  // same render-wait convention as every other absence check in this file,
  // but keyed on <pre> rather than .md-content alone, since .md-content is
  // present as soon as any part of this message's markdown has rendered.
  await expect(msg.locator('.md-content pre')).toHaveCount(1);
  await expect(msg.locator('.gcs-link')).toHaveCount(0);
});

test('gs://bkt/dir/ and gs://bkt/a/b.c/ stay plain text', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_DIR_MSG.id);
  await expect(msg).toContainText(`gs://${GCS_BUCKET}/dir/`);
  await expect(msg.locator('.md-content')).toHaveCount(1);
  await expect(msg.locator('.gcs-link')).toHaveCount(0);
});

test('xgs://... and /gs://... do not link', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_NOLINK_SCHEME_MSG.id);
  await expect(msg).toContainText(`xgs://${GCS_BUCKET}/o and /gs://${GCS_BUCKET}/o`);
  await expect(msg.locator('.md-content')).toHaveCount(1);
  await expect(msg.locator('.gcs-link')).toHaveCount(0);
});

test('links a gs:// URI at the start of the message body', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_START_MSG.id);
  const links = msg.locator('.gcs-link');
  await expect(links).toHaveCount(1);
  await expect(links.first()).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/start.txt`);
});

test('links a gs:// URI directly after a closing </code>', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_AFTER_CODE_MSG.id);
  const links = msg.locator('.gcs-link');
  await expect(links).toHaveCount(1);
  await expect(links.first()).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/after.txt`);
});

test('gs://bkt/workspace/collision.md yields one gcs link and no path link', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_WORKSPACE_COLLISION_MSG.id);
  await expect(msg.locator('.gcs-link')).toHaveCount(1);
  await expect(msg.locator('.path-link')).toHaveCount(0);
});

test('hostile name (quote/onmouseover) produces no extra attribute', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_HOSTILE_QUOTE_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/a`);
  const handle = await link.elementHandle();
  const hasOnMouseOver = await handle?.evaluate((el) => el.hasAttribute('onmouseover'));
  expect(hasOnMouseOver).toBe(false);
});

test('hostile name (<img>) produces no extra element', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_HOSTILE_IMG_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/a`);
  await expect(msg.locator('img')).toHaveCount(0);
});

test('hostile name (&) does not link at all: the continuation rule rejects the whole occurrence', async ({
  page,
}) => {
  // '&' immediately followed by non-whitespace ('b') is a fragment/query/
  // param-like continuation — a full reject, not a truncation to "a" the
  // way a quote or '<' would be.
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_HOSTILE_AMP_MSG.id);
  await expect(msg).toContainText(`gs://${GCS_BUCKET}/a&b`);
  await expect(msg.locator('.md-content')).toHaveCount(1);
  await expect(msg.locator('.gcs-link')).toHaveCount(0);
});

test('the same text in a user (own) message does not link', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_USER_OWN_MSG.id);
  await expect(msg).toContainText(`gs://${GCS_BUCKET}/dir/file.md`);
  await expect(msg.locator('.md-content')).toHaveCount(1);
  await expect(msg.locator('.gcs-link')).toHaveCount(0);
});

test('another user\'s message does not link, even though v2 renders it as "not me" — the agent message beside it is the positive control', async ({
  page,
}) => {
  await gotoGcsThread(page);

  // GCS_OTHER_USER_MSG is sent by the DM's other party (a user, not the
  // viewer), so v2's fromAgent heuristic (senderId !== currentUserId)
  // renders it exactly like an agent message — left-aligned, "not me". gs://
  // linkification must gate on the real sender kind and not link it.
  const otherUserMsg = messageLocator(page, GCS_OTHER_USER_MSG.id);
  await expect(otherUserMsg).toContainText(`gs://${GCS_BUCKET}/dir/file.md`);
  await expect(otherUserMsg.locator('.md-content')).toHaveCount(1);
  await expect(otherUserMsg.locator('.gcs-link')).toHaveCount(0);

  // Positive control: an actual agent message in the same thread, same
  // rendering path, does link.
  const agentMsg = messageLocator(page, GCS_TEXT_AND_CODE_MSG.id);
  await expect(agentMsg.locator('.gcs-link')).toHaveCount(2);
});

test('interplay with a GitHub-shortlink-like fragment: neither a gcs link nor a gh link forms', async ({
  page,
}) => {
  // '#' immediately followed by non-whitespace ('1') is a fragment-like
  // continuation: the gs:// URI itself does not link at all. Separately,
  // the GitHub-ref pattern requires its own
  // left boundary immediately before "o" that is not itself a word
  // character or '/' (chat-message.ts's GITHUB_REF_REGEX) — the preceding
  // '/' in ".../o/r#1" fails that boundary — so no gh-ref-link forms either,
  // confirming the two features do not cross-trigger each other.
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_SHORTLINK_MSG.id);
  await expect(msg).toContainText(`gs://${GCS_BUCKET}/o/r#1`);
  await expect(msg.locator('.md-content')).toHaveCount(1);
  await expect(msg.locator('.gcs-link')).toHaveCount(0);
  await expect(msg.locator('.gh-ref-link')).toHaveCount(0);
});

test('a fragment/query continuation does not link, and the quoted form does', async ({ page }) => {
  await gotoGcsThread(page);

  const hashMsg = messageLocator(page, GCS_FRAGMENT_HASH_MSG.id);
  await expect(hashMsg).toContainText(`gs://${GCS_BUCKET}/a#frag`);
  await expect(hashMsg.locator('.md-content')).toHaveCount(1);
  await expect(hashMsg.locator('.gcs-link')).toHaveCount(0);

  const queryMsg = messageLocator(page, GCS_FRAGMENT_QUERY_MSG.id);
  await expect(queryMsg).toContainText(`gs://${GCS_BUCKET}/a?x=1`);
  await expect(queryMsg.locator('.md-content')).toHaveCount(1);
  await expect(queryMsg.locator('.gcs-link')).toHaveCount(0);

  const quotedMsg = messageLocator(page, GCS_QUOTED_MSG.id);
  const quotedLink = quotedMsg.locator('.gcs-link');
  await expect(quotedLink).toHaveCount(1);
  await expect(quotedLink).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/o.md`);
});

test('the cross-project-exchange URI: one gcs link, no path link, correct request, renders as markdown with Source toggle', async ({
  page,
}) => {
  const requests = await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_XPROJECT_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute(
    'data-gcs-uri',
    `gs://${GCS_XPROJECT_BUCKET}/${GCS_XPROJECT_OBJECT}`
  );
  await expect(msg.locator('.path-link')).toHaveCount(0);

  await link.click();
  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog.locator('scion-markdown-preview')).toBeVisible();
  await expect(dialog.locator('sl-button', { hasText: 'Source' })).toBeVisible();

  const gcsRequest = requests.find((r) => r.url.includes('/api/v1/gcs/object?'));
  expect(gcsRequest).toBeDefined();
  const reqUrl = new URL(gcsRequest!.url);
  expect(reqUrl.searchParams.get('bucket')).toBe(GCS_XPROJECT_BUCKET);
  expect(reqUrl.searchParams.get('object')).toBe(GCS_XPROJECT_OBJECT);
  expect(reqUrl.searchParams.get('message')).toBe(GCS_XPROJECT_MSG.id);

  await dialog.locator('sl-button', { hasText: 'Source' }).click();
  await expect(dialog).toContainText(GCS_MARKDOWN_BODY.trim().split('\n')[0]);
});

test('a .json object renders as code', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_JSON_MSG.id);
  await msg.locator('.gcs-link').click();

  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog.locator('scion-code-editor')).toBeVisible();
  await expect(dialog).toContainText(GCS_JSON_BODY);
});

test('404 and 403 both show the identical uniform message with Retry, and the Cloud Console fallback', async ({
  page,
}) => {
  await gotoGcsThread(page);

  await messageLocator(page, GCS_NOTFOUND_MSG.id).locator('.gcs-link').click();
  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("isn't available");
  await expect(dialog.locator('sl-button', { hasText: 'Retry' })).toBeVisible();
  const notFoundLink = dialog.locator('a.console-fallback-link');
  await expect(notFoundLink).toHaveAttribute(
    'href',
    `https://console.cloud.google.com/storage/browser/_details/${GCS_BUCKET}/missing.txt`
  );
  await expect(notFoundLink).toHaveAttribute('target', '_blank');
  await expect(notFoundLink).toHaveAttribute('rel', 'noopener noreferrer');
  const notFoundText = await dialog.locator('.file-preview-placeholder.error p').textContent();

  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();

  await messageLocator(page, GCS_DENIED_MSG.id).locator('.gcs-link').click();
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("isn't available");
  const deniedText = await dialog.locator('.file-preview-placeholder.error p').textContent();
  expect(deniedText).toBe(notFoundText);
  const deniedLink = dialog.locator('a.console-fallback-link');
  await expect(deniedLink).toHaveAttribute(
    'href',
    `https://console.cloud.google.com/storage/browser/_details/${GCS_BUCKET}/denied.txt`
  );
  await expect(deniedLink).toHaveAttribute('target', '_blank');
  await expect(deniedLink).toHaveAttribute('rel', 'noopener noreferrer');
});

test('a binary-looking name (report.pdf) still fetches and shows the uniform 404 message with the Cloud Console fallback, not the download-only placeholder', async ({
  page,
}) => {
  const requests = await gotoGcsThread(page);
  await messageLocator(page, GCS_BINARY_NOTFOUND_MSG.id).locator('.gcs-link').click();

  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("isn't available");
  await expect(dialog.locator('a.console-fallback-link')).toHaveAttribute(
    'href',
    `https://console.cloud.google.com/storage/browser/_details/${GCS_BUCKET}/report.pdf`
  );
  expect(requests.some((r) => r.url.includes('object=report.pdf'))).toBe(true);
});

// ---------------------------------------------------------------------------
// Accepted client-wider mismatches: the client links these, but the server
// (pkg/hub/gcs_link_test.go) extracts something different, or nothing at
// all, from the same raw body — the client runs its linkifier over marked's
// rendered, tag-split, HTML-escaped text; the server scans the raw markdown
// body. Each test below asserts only the client's actual current rendering,
// never that it is correct UX.
// ---------------------------------------------------------------------------

test('emphasis wrapper: the client links the object without the underscores', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_EMPHASIS_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/o`);
  await expect(msg.locator('em .gcs-link, i .gcs-link')).toHaveCount(1);
});

test('strikethrough wrapper: the client links the object without the tildes', async ({ page }) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_STRIKETHROUGH_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/o`);
  await expect(msg.locator('del .gcs-link, s .gcs-link')).toHaveCount(1);
});

test('a "?" directly followed by an inline-code span: the client links up to the "?"', async ({
  page,
}) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_TAG_SPLIT_QUESTION_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/a`);
});

test('a raw "&amp;" entity followed by whitespace: the client links the object before it', async ({
  page,
}) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_RAW_ENTITY_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/a`);
});

test('a markdown-escaped underscore: the client links the full unescaped object', async ({
  page,
}) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_BACKSLASH_ESCAPE_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/secret_v2`);
});

test('a "?" directly followed by strong emphasis: the client links up to the "?"', async ({
  page,
}) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_DOUBLE_STAR_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/a`);
});

test('a raw "&lt;" entity directly after the object: the client links the object before it', async ({
  page,
}) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_RAW_LT_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/a`);
});

test('a raw "&quot;" entity directly after the object: the client links the object before it', async ({
  page,
}) => {
  await gotoGcsThread(page);
  const msg = messageLocator(page, GCS_RAW_QUOT_MSG.id);
  const link = msg.locator('.gcs-link');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('data-gcs-uri', `gs://${GCS_BUCKET}/a`);
});

// ---------------------------------------------------------------------------
// Image sniffing, SVG-as-source, octet-stream, the Content-Length
// preview-size abort, 413 and an HTML-bodied object's download.
// ---------------------------------------------------------------------------

for (const [msg, ext] of [
  [GCS_IMAGE_PNG_MSG, 'png'],
  [GCS_IMAGE_JPEG_MSG, 'jpg'],
  [GCS_IMAGE_GIF_MSG, 'gif'],
  [GCS_IMAGE_WEBP_MSG, 'webp'],
] as const) {
  test(`a .${ext} object renders as a real, decoded <img>`, async ({ page }) => {
    await gotoGcsThread(page);
    await messageLocator(page, msg.id).locator('.gcs-link').click();

    const dialog = previewDialog(page);
    await expect(dialog).toBeVisible();
    const img = dialog.locator('img.file-preview-image');
    await expect(img).toBeVisible();
    await expect(img).toHaveAttribute('src', /^blob:/);
    await expect(dialog.locator('scion-code-editor')).toHaveCount(0);

    // Proves the browser actually decoded real image bytes, not just that an
    // <img> tag with some src exists — a broken image would report 0.
    const naturalWidth = await img.evaluate((el) => (el as HTMLImageElement).naturalWidth);
    expect(naturalWidth).toBeGreaterThan(0);
  });
}

test('a .svg object shows as source text, never as <img>', async ({ page }) => {
  await gotoGcsThread(page);
  await messageLocator(page, GCS_SVG_MSG.id).locator('.gcs-link').click();

  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog.locator('img.file-preview-image')).toHaveCount(0);
  await expect(dialog.locator('scion-code-editor')).toBeVisible();
  await expect(dialog).toContainText('<svg');
});

test('a .png object whose bytes are HTML is not rendered as an image, and Download forces a file download rather than a rendered page', async ({
  page,
}) => {
  await gotoGcsThread(page);
  await messageLocator(page, GCS_HTML_AS_PNG_MSG.id).locator('.gcs-link').click();

  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  // Served as text, never as an image and never as rendered HTML — shown as
  // source/code text, with the literal tags visible rather than executed.
  await expect(dialog.locator('img.file-preview-image')).toHaveCount(0);
  await expect(dialog.locator('scion-code-editor')).toBeVisible();
  await expect(dialog).toContainText('<html>');

  const downloadButton = dialog.locator('sl-button', { hasText: 'Download' });
  const [download] = await Promise.all([page.waitForEvent('download'), downloadButton.click()]);
  expect(download.suggestedFilename()).toBe('fake.png');
  // The page itself must never have navigated to/rendered the HTML body —
  // if it had, this heading would be present in the live DOM.
  await expect(page.locator('h1', { hasText: 'not a png' })).toHaveCount(0);
});

test('an octet-stream object shows "can\'t be previewed" with Download, not the generic placeholder', async ({
  page,
}) => {
  await gotoGcsThread(page);
  await messageLocator(page, GCS_OCTET_STREAM_MSG.id).locator('.gcs-link').click();

  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("This file can't be previewed.");
  await expect(dialog.locator('img.file-preview-image')).toHaveCount(0);
  await expect(dialog.locator('scion-code-editor')).toHaveCount(0);
  await expect(dialog.locator('sl-button', { hasText: 'Download' })).toBeVisible();
});

test('text over the 512 KB Content-Length threshold shows the too-large-inline message with Download', async ({
  page,
}) => {
  await gotoGcsThread(page);
  await messageLocator(page, GCS_TOO_LARGE_INLINE_MSG.id).locator('.gcs-link').click();

  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText('too large to preview inline');
  await expect(dialog.locator('scion-code-editor')).toHaveCount(0);
  await expect(dialog.locator('sl-button', { hasText: 'Download' })).toBeVisible();
});

test('a 413 shows the size-limit message with the Cloud Console fallback and NO Download', async ({
  page,
}) => {
  await gotoGcsThread(page);
  await messageLocator(page, GCS_413_MSG.id).locator('.gcs-link').click();

  const dialog = previewDialog(page);
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText('too large to open here (12.0 MB, limit 10.0 MB)');
  await expect(dialog.locator('a.console-fallback-link')).toBeVisible();
  await expect(dialog.locator('sl-button', { hasText: 'Download' })).toHaveCount(0);
});
