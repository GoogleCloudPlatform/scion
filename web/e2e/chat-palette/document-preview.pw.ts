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
 * Chromium: the Documents group renders as the fourth quadrant and matches
 * both file names and container paths, sharing the palette's global ranking
 * and Tab order with Agents/Threads/People. A document selection closes the
 * palette before the page-level preview opens — exactly one modal ever
 * controls focus — leaving the URL, selected conversation, composer draft and
 * mobile panel unchanged; closing the preview restores the focus captured
 * when the palette opened. A binary attachment shows its details and a
 * Download control without starting a download.
 */

import { test, expect, type Page } from '@playwright/test';
import {
  setupApiMocks,
  AGENT_WITH_DM,
  SELF_USER_ID,
  DOC_TEXT_FILE,
  DOC_BINARY_ATTACHMENT,
  type PaletteFixtureOverrides,
} from './mock-api.js';

const DM_KEY = `dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`;
const DM_ROUTE = `/chat/dm/${encodeURIComponent(DM_KEY)}`;

/** Comfortably longer than Shoelace's 250ms default dialog hide animation. */
const SETTLE_MS = 700;

const TEXT_FILE_FIXTURE: PaletteFixtureOverrides = {
  workspaceFiles: {
    [DOC_TEXT_FILE.projectId]: {
      [DOC_TEXT_FILE.filePath]: {
        content: DOC_TEXT_FILE.content,
        size: DOC_TEXT_FILE.content.length,
      },
    },
  },
};

const BINARY_ATTACHMENT_FIXTURE: PaletteFixtureOverrides = {
  attachmentsById: {
    [DOC_BINARY_ATTACHMENT.id]: { mime: DOC_BINARY_ATTACHMENT.mime, body: 'binary-bytes' },
  },
};

async function gotoChat(
  page: Page,
  overrides: PaletteFixtureOverrides = {},
  route?: string
): Promise<void> {
  await setupApiMocks(page, overrides);
  const url = route
    ? `/e2e/chat-palette/fixture.html?route=${encodeURIComponent(route)}`
    : '/e2e/chat-palette/fixture.html';
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
}

/** Seed the Documents group through the fixture's real store/localStorage seam — see fixture.ts. */
async function seedDocuments(
  page: Page,
  files: Array<{
    name: string;
    sentAt: string;
    projectId?: string;
    projectName?: string;
    target:
      | { kind: 'attachment'; id: string; mime: string; size: number }
      | {
          kind: 'path';
          projectId: string;
          containerPath: string;
          location: { kind: 'workspace'; filePath: string };
        };
  }>
): Promise<void> {
  await page.evaluate((f) => window.chatPaletteFixture.seedRecentFiles(f), files);
}

function textDocumentInput() {
  return {
    name: DOC_TEXT_FILE.name,
    sentAt: DOC_TEXT_FILE.sentAt,
    projectId: DOC_TEXT_FILE.projectId,
    projectName: DOC_TEXT_FILE.projectName,
    target: {
      kind: 'path' as const,
      projectId: DOC_TEXT_FILE.projectId,
      containerPath: DOC_TEXT_FILE.containerPath,
      location: { kind: 'workspace' as const, filePath: DOC_TEXT_FILE.filePath },
    },
  };
}

function binaryAttachmentInput() {
  return {
    name: DOC_BINARY_ATTACHMENT.name,
    sentAt: DOC_BINARY_ATTACHMENT.sentAt,
    target: {
      kind: 'attachment' as const,
      id: DOC_BINARY_ATTACHMENT.id,
      mime: DOC_BINARY_ATTACHMENT.mime,
      size: DOC_BINARY_ATTACHMENT.size,
    },
  };
}

function paletteInput(page: Page) {
  return page.locator('scion-chat-switcher #palette-query-input');
}

function paletteDialog(page: Page) {
  return page.locator('scion-chat-switcher sl-dialog[label="Quick switcher"]');
}

function groupOptions(page: Page, group: string) {
  return page.locator(
    `scion-chat-switcher [aria-labelledby="palette-heading-${group}"] .palette-option`
  );
}

function previewDialog(page: Page) {
  return page.locator('scion-chat-file-preview sl-dialog');
}

/** Deep-query into the real composer's native textarea, through both shadow roots. */
function composerTextarea(page: Page) {
  return page
    .locator('scion-page-chat')
    .locator('scion-chat-thread')
    .locator('scion-chat-composer')
    .locator('sl-textarea')
    .locator('textarea');
}

/** True once the real focused element, descended through every open shadow root, sits inside `<scion-chat-file-preview>`. */
async function deepActiveElementIsInsidePreview(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el) {
      if (el.tagName.toLowerCase() === 'scion-chat-file-preview') return true;
      el = (el as HTMLElement).shadowRoot?.activeElement ?? null;
    }
    return false;
  });
}

test('Documents render as the fourth group and match both file names and container paths', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE);
  // A second file whose *name* would never match "report", but whose
  // container path does — proves path matching, not just name matching.
  await seedDocuments(page, [
    textDocumentInput(),
    {
      name: 'summary.pdf',
      sentAt: '2026-09-28T11:00:00Z',
      projectId: DOC_TEXT_FILE.projectId,
      projectName: DOC_TEXT_FILE.projectName,
      target: {
        kind: 'path',
        projectId: DOC_TEXT_FILE.projectId,
        containerPath: '/workspace/reports/summary.pdf',
        location: { kind: 'workspace', filePath: 'reports/summary.pdf' },
      },
    },
  ]);

  await page.keyboard.press('Control+k');
  await expect(groupOptions(page, 'documents')).toHaveCount(2);

  await paletteInput(page).fill('reports');
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  await expect(groupOptions(page, 'documents')).toContainText('summary.pdf');

  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  await expect(groupOptions(page, 'documents')).toContainText(DOC_TEXT_FILE.name);
});

test('Tab/Shift+Tab cycle through all four groups, including Documents, in reading order', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE);
  await seedDocuments(page, [textDocumentInput()]);

  await page.keyboard.press('Control+k');
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  // Empty query: the only real-activity candidate is AGENT_WITH_DM (its DM
  // has lastActivityAt), so it starts as the global best.
  await expect(
    page.locator('scion-chat-switcher .palette-option').filter({ hasText: AGENT_WITH_DM.name })
  ).toHaveClass(/active/);

  await page.keyboard.press('Tab'); // -> Threads (empty, skipped) -> People (empty, skipped) -> Documents
  await expect(groupOptions(page, 'documents').first()).toHaveClass(/active/);

  await page.keyboard.press('Tab'); // wraps back to Agents
  await expect(
    page.locator('scion-chat-switcher .palette-option').filter({ hasText: AGENT_WITH_DM.name })
  ).toHaveClass(/active/);

  await page.keyboard.press('Shift+Tab'); // wraps backward to Documents
  await expect(groupOptions(page, 'documents').first()).toHaveClass(/active/);
});

test('selecting a document closes the palette before the preview opens, and closing the preview restores focus without changing the URL, conversation, draft or mobile panel', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE, DM_ROUTE);
  await page.waitForSelector('scion-chat-composer sl-textarea', { state: 'attached' });
  // The document belongs to a real project; the active conversation here is
  // a DM with no project of its own at all — this exercises "a different
  // project's active conversation" as directly as this fixture can.
  await seedDocuments(page, [textDocumentInput()]);

  const textarea = composerTextarea(page);
  await textarea.click();
  await textarea.fill('draft in progress');
  const urlBefore = page.url();
  const mobilePanelBefore = await page.evaluate(
    () =>
      (document.querySelector('scion-page-chat') as unknown as { mobilePanel: string }).mobilePanel
  );

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  await page.keyboard.press('Enter');

  // The palette is gone and the preview is up — never both at once.
  await expect(previewDialog(page)).toBeVisible();
  await expect(paletteDialog(page)).toBeHidden();
  await expect(previewDialog(page)).toHaveAttribute('label', DOC_TEXT_FILE.name);
  await expect(page.locator('scion-chat-file-preview scion-code-editor')).toContainText(
    DOC_TEXT_FILE.content
  );
  // The preview is born `<sl-dialog open>` (never a false->true transition),
  // so Shoelace's own sl-initial-focus never runs for it — this component
  // must move focus in itself, or a keyboard/screen-reader user is left on
  // whatever was focused before the palette closed (typically nothing).
  expect(await deepActiveElementIsInsidePreview(page)).toBe(true);

  expect(page.url()).toBe(urlBefore);
  await expect(textarea).toHaveValue('draft in progress');
  expect(
    await page.evaluate(
      () =>
        (document.querySelector('scion-page-chat') as unknown as { mobilePanel: string })
          .mobilePanel
    )
  ).toBe(mobilePanelBefore);

  // Closing the preview restores focus to the composer, and does not
  // implicitly reopen the palette.
  await page.locator('scion-chat-file-preview sl-dialog').getByLabel('Close').click();
  await expect(previewDialog(page)).toBeHidden();
  await expect(textarea).toBeFocused();
  await expect(textarea).toHaveValue('draft in progress');
  await expect(paletteDialog(page)).toBeHidden();
  expect(page.url()).toBe(urlBefore);
});

test('the palette panel and the preview dialog are never both visible in the same frame', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE);
  await seedDocuments(page, [textDocumentInput()]);

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);

  // Sample every animation frame from the moment the selection commits
  // until well after the preview settles — the ordering guarantee (close
  // fully, only then open) only means something if no frame in between ever
  // shows both.
  await page.evaluate(() => {
    (window as unknown as { __bothVisibleFrames: number }).__bothVisibleFrames = 0;
    let samples = 0;
    const tick = () => {
      const switcher = document
        .querySelector('scion-page-chat')
        ?.shadowRoot?.querySelector('scion-chat-switcher');
      const panel = switcher?.shadowRoot
        ?.querySelector('sl-dialog')
        ?.shadowRoot?.querySelector('[part=panel]') as HTMLElement | null;
      const paletteVisible = !!panel && panel.checkVisibility();
      const previewDialogEl = document
        .querySelector('scion-page-chat')
        ?.shadowRoot?.querySelector('scion-chat-file-preview')
        ?.shadowRoot?.querySelector('sl-dialog');
      const previewOpen = !!(previewDialogEl as unknown as { open?: boolean } | null)?.open;
      if (paletteVisible && previewOpen) {
        (window as unknown as { __bothVisibleFrames: number }).__bothVisibleFrames++;
      }
      samples++;
      if (samples < 180) requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });

  await page.keyboard.press('Enter');
  await expect(previewDialog(page)).toBeVisible();
  await page.waitForTimeout(500);

  const bothVisibleFrames = await page.evaluate(
    () => (window as unknown as { __bothVisibleFrames: number }).__bothVisibleFrames
  );
  expect(bothVisibleFrames).toBe(0);
});

test('selecting a document opens the preview from /chat with no thread mounted', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE);
  await seedDocuments(page, [textDocumentInput()]);

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  await page.keyboard.press('Enter');

  await expect(previewDialog(page)).toBeVisible();
  await expect(previewDialog(page)).toHaveAttribute('label', DOC_TEXT_FILE.name);
  await expect(page).toHaveURL(/\/chat$/);
});

test('a small binary attachment (classified by MIME, not size) shows details and a Download control without starting a download or ever fetching its bytes as text', async ({
  page,
}) => {
  await gotoChat(page, BINARY_ATTACHMENT_FIXTURE);
  await seedDocuments(page, [binaryAttachmentInput()]);
  let downloadFired = false;
  page.on('download', () => {
    downloadFired = true;
  });
  const attachmentBodyRequests: string[] = [];
  page.on('request', (req) => {
    if (req.url().includes(`/api/v1/chat/attachments/${DOC_BINARY_ATTACHMENT.id}`)) {
      attachmentBodyRequests.push(req.url());
    }
  });

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_BINARY_ATTACHMENT.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  await page.keyboard.press('Enter');

  await expect(previewDialog(page)).toBeVisible();
  await expect(previewDialog(page)).toContainText("can't be shown here");
  await expect(previewDialog(page).locator('scion-code-editor')).toHaveCount(0);
  const downloadButton = page.locator('scion-chat-file-preview sl-button', { hasText: 'Download' });
  await expect(downloadButton).toBeVisible();
  await page.waitForTimeout(300); // rendering alone must never trigger a download
  expect(downloadFired).toBe(false);
  // DOC_BINARY_ATTACHMENT's size (2048 bytes) is well under the text-preview
  // limit — it is classified binary because its MIME is
  // application/octet-stream and archive.bin is not a recognized text file
  // name, so its body must never be fetched at all.
  expect(attachmentBodyRequests).toEqual([]);
});

test('emptying the Documents group after the palette opened leaves no row to activate, so Enter is a no-op', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE);
  await seedDocuments(page, [textDocumentInput()]);

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  // Clear the store's records out from under the still-open palette.
  await page.evaluate(() => window.chatPaletteFixture.seedRecentFiles([]));
  await expect(groupOptions(page, 'documents')).toHaveCount(0);

  // The query input still has committed keyboard focus from before the
  // group emptied out; Enter now is a no-op (nothing active to commit —
  // this never reaches _handlePaletteSelect's own stale-candidate guard at
  // all, since there is no longer an active candidate to commit in the
  // first place; see the next test for that guard specifically).
  await page.keyboard.press('Enter');
  await expect(previewDialog(page)).toBeHidden();
  await expect(paletteDialog(page)).toBeVisible();
});

test('a stale document target (its identity replaced by a group refresh before the select is handled) does not open a preview', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE);
  await seedDocuments(page, [textDocumentInput()]);

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);

  // Capture the real, currently-active candidate's own target object —
  // exactly what a real Enter press would commit right now — before
  // replacing the underlying record out from under it.
  const staleTarget = await page.evaluate(
    () =>
      (document.querySelector('scion-page-chat') as unknown as { v2PaletteGroups: any })
        .v2PaletteGroups.documents.candidates[0].target
  );

  // Replace the record with a same-named one at a different path (a
  // different identity key) — the group's own candidate list no longer
  // contains that exact target, even though the visible row looks
  // unchanged. A real keyboard Enter press cannot be timed to land inside
  // this exact window (the switcher's own re-ranking already picks up the
  // replacement's new identity on the very next render, closing the race
  // before another keydown can reach it) — dispatching the real
  // `palette-select` event directly, carrying the target actually captured
  // above, reproduces the state `_handlePaletteSelect` must defend against
  // without depending on winning that unwinnable timing race.
  await page.evaluate((doc) => {
    window.chatPaletteFixture.seedRecentFiles([
      {
        name: doc.name,
        sentAt: doc.sentAt,
        projectId: doc.projectId,
        projectName: doc.projectName,
        target: {
          kind: 'path',
          projectId: doc.projectId,
          containerPath: `/workspace/replaced/${doc.name}`,
          location: { kind: 'workspace', filePath: `replaced/${doc.filePath}` },
        },
      },
    ]);
  }, DOC_TEXT_FILE);
  await expect(groupOptions(page, 'documents')).toHaveCount(1); // same visible row, different identity now

  await page.evaluate((target) => {
    const switcher = document
      .querySelector('scion-page-chat')!
      .shadowRoot!.querySelector('scion-chat-switcher')!;
    switcher.dispatchEvent(
      new CustomEvent('palette-select', { detail: { target }, bubbles: true, composed: true })
    );
  }, staleTarget);

  // The preview only ever opens on the palette's own sl-after-hide, which
  // real Shoelace fires only once its ~250ms close animation actually
  // finishes — asserting the preview is hidden *before* that has happened
  // is trivially true regardless of whether the stale-target guard rejected
  // anything, since the preview could not possibly have opened yet either
  // way. Waiting for the palette to actually finish closing first (then
  // settling past that for margin) is what makes the following assertion
  // mean something.
  await expect(paletteDialog(page)).toBeHidden();
  await page.waitForTimeout(SETTLE_MS);
  await expect(previewDialog(page)).toBeHidden();
});

const PREVIEW_REOPEN_DELAYS_MS = [0, 50, 100, 150];

/**
 * Stretch the preview's own hide animation well past every delay this loop
 * tests — the real gap between Escape and the browser actually starting that
 * animation varies with machine load, so a fixed delay measured from the
 * keypress can't otherwise guarantee every delay in the list still lands
 * before a real ~250ms hide has finished. Mirrors reopen-race.pw.ts's own
 * DETERMINISTIC_HIDE_MS for the palette's close.
 */
const DETERMINISTIC_PREVIEW_HIDE_MS = 1500;
const DETERMINISTIC_PREVIEW_SETTLE_MS = DETERMINISTIC_PREVIEW_HIDE_MS + 700;

for (const delayMs of PREVIEW_REOPEN_DELAYS_MS) {
  test(`Ctrl+K ${delayMs}ms after Escape on the document preview reopens the palette focused, then restores the original composer focus and selection on close`, async ({
    page,
  }) => {
    await gotoChat(page, TEXT_FILE_FIXTURE, DM_ROUTE);
    await page.waitForSelector('scion-chat-composer sl-textarea', { state: 'attached' });
    await seedDocuments(page, [textDocumentInput()]);

    const textarea = composerTextarea(page);
    await textarea.click();
    await textarea.fill('draft text');
    await textarea.evaluate((el) => (el as HTMLTextAreaElement).setSelectionRange(2, 5));

    await page.keyboard.press('Control+k');
    await paletteInput(page).fill(DOC_TEXT_FILE.name);
    await expect(groupOptions(page, 'documents')).toHaveCount(1);
    await page.keyboard.press('Enter');
    await expect(previewDialog(page)).toBeVisible();

    await page.evaluate(
      (ms) => window.chatPaletteFixture.setFilePreviewDialogHideDuration(ms),
      DETERMINISTIC_PREVIEW_HIDE_MS
    );

    await page.keyboard.press('Escape'); // starts the preview's own (stretched) close
    // The close animation is still in flight for every delay in this list —
    // the exact race: a still-hiding preview's own sl-dialog has already
    // flipped `open` to false, so it no longer counts as an active modal.
    await page.waitForTimeout(delayMs);
    await page.keyboard.press('Control+k');

    // The preview's own chat-file-preview-close (stretched above) can still
    // be pending at this point for every delay above, and arrives well after
    // this reopen attempt — settling for longer than that stretched
    // animation before asserting anything proves the reopened palette
    // *stays* correct, not just that it looked correct for an instant.
    await page.waitForTimeout(DETERMINISTIC_PREVIEW_SETTLE_MS);

    await expect(paletteDialog(page)).toBeVisible();
    await expect(paletteInput(page)).toBeFocused();

    await page.keyboard.type('probe-text');
    await expect(paletteInput(page)).toHaveValue('probe-text');
    await expect(textarea).not.toHaveValue(/probe-text/);

    await page.keyboard.press('Escape'); // close the reopened palette normally
    await expect(paletteDialog(page)).toBeHidden();

    // The invoker captured before the palette was ever first opened — the
    // composer and its selection — must be exactly what this ordinary
    // dismissal restores, not the fallback: the preview reopen never
    // touched it (skipInvokerCapture), and the document selection was never
    // a "focus a new composer" kind of close.
    await expect(textarea).toBeFocused();
    const selection = await textarea.evaluate((el) => [
      (el as HTMLTextAreaElement).selectionStart,
      (el as HTMLTextAreaElement).selectionEnd,
    ]);
    expect(selection).toEqual([2, 5]);
  });
}

test('a second Escape while a Ctrl+K is queued behind a closing document preview cancels the queue, ending closed', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE, DM_ROUTE);
  await page.waitForSelector('scion-chat-composer sl-textarea', { state: 'attached' });
  await seedDocuments(page, [textDocumentInput()]);

  const textarea = composerTextarea(page);
  await textarea.click();
  await textarea.fill('draft text');
  await textarea.evaluate((el) => (el as HTMLTextAreaElement).setSelectionRange(2, 5));

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  await page.keyboard.press('Enter');
  await expect(previewDialog(page)).toBeVisible();

  await page.evaluate(
    (ms) => window.chatPaletteFixture.setFilePreviewDialogHideDuration(ms),
    DETERMINISTIC_PREVIEW_HIDE_MS
  );

  await page.keyboard.press('Escape'); // starts the preview's own (stretched) close
  await page.waitForTimeout(100); // still hiding — chat-file-preview-close has not fired yet
  await page.keyboard.press('Control+k'); // queues a reopen behind it
  // The user's very next keystroke asks to close again — the last key
  // pressed before the preview's own close finally arrives is Escape, so the
  // palette must not reopen out from under it.
  await page.keyboard.press('Escape');
  await page.waitForTimeout(DETERMINISTIC_PREVIEW_SETTLE_MS);

  await expect(paletteDialog(page)).toBeHidden();
  await expect(textarea).toBeFocused();
  const selection = await textarea.evaluate((el) => [
    (el as HTMLTextAreaElement).selectionStart,
    (el as HTMLTextAreaElement).selectionEnd,
  ]);
  expect(selection).toEqual([2, 5]);
});

test('a Ctrl+K queued behind a closing document preview is abandoned, not opened, if an unrelated modal opens before the preview settles', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE, DM_ROUTE);
  await page.waitForSelector('scion-chat-composer sl-textarea', { state: 'attached' });
  await seedDocuments(page, [textDocumentInput()]);

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  await page.keyboard.press('Enter');
  await expect(previewDialog(page)).toBeVisible();

  await page.evaluate(
    (ms) => window.chatPaletteFixture.setFilePreviewDialogHideDuration(ms),
    DETERMINISTIC_PREVIEW_HIDE_MS
  );

  await page.keyboard.press('Escape'); // starts the preview's own (stretched) close
  await page.waitForTimeout(100); // still hiding — chat-file-preview-close has not fired yet
  await page.keyboard.press('Control+k'); // queues a reopen behind it

  await page.evaluate(() => {
    const dialog = document.createElement('sl-dialog');
    dialog.setAttribute('label', 'Unrelated');
    dialog.id = 'preview-queue-unrelated-dialog';
    document.body.appendChild(dialog);
    (dialog as unknown as { open: boolean }).open = true;
  });
  await page.waitForTimeout(DETERMINISTIC_PREVIEW_SETTLE_MS);

  await expect(page.locator('#preview-queue-unrelated-dialog')).toHaveJSProperty('open', true);
  await expect(paletteDialog(page)).toBeHidden();
});

/** Comfortably longer than a stretched 1500ms palette-close animation. */
const DETERMINISTIC_PALETTE_HIDE_MS = 1500;
const DETERMINISTIC_PALETTE_SETTLE_MS = DETERMINISTIC_PALETTE_HIDE_MS + 700;

test('a deferred document preview is abandoned, not opened, if an unrelated modal opens before the palette settles', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE, DM_ROUTE);
  await seedDocuments(page, [textDocumentInput()]);

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  // Stretched so the palette's own real close can't finish (and let the
  // preview open normally) before the guard re-check this test is actually
  // about ever gets exercised.
  await page.evaluate(
    (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
    DETERMINISTIC_PALETTE_HIDE_MS
  );
  await page.keyboard.press('Enter'); // selects the document, starts the palette's own (stretched) close

  await page.evaluate(() => {
    const dialog = document.createElement('sl-dialog');
    dialog.setAttribute('label', 'Unrelated');
    dialog.id = 'preview-guard-unrelated-dialog';
    document.body.appendChild(dialog);
    (dialog as unknown as { open: boolean }).open = true;
  });

  // Sample every 25ms across the whole (stretched) settle window rather than
  // checking only the final state — a regression here must never report the
  // preview open at all, not just "eventually closed again."
  const samples: boolean[] = [];
  const sampleCount = Math.ceil(DETERMINISTIC_PALETTE_SETTLE_MS / 25);
  for (let i = 0; i < sampleCount; i++) {
    await page.waitForTimeout(25);
    samples.push(await previewDialog(page).isVisible());
  }

  // The unrelated dialog stays the one modal in control; the deferred
  // preview must not stack over it.
  expect(samples.some(Boolean), `preview visibility samples: ${samples.join(',')}`).toBe(false);
  await expect(page.locator('#preview-guard-unrelated-dialog')).toHaveJSProperty('open', true);
  await expect(previewDialog(page)).toBeHidden();
});

test('a deferred document preview is abandoned, not opened, if the route leaves /chat before the palette settles', async ({
  page,
}) => {
  await gotoChat(page, TEXT_FILE_FIXTURE, DM_ROUTE);
  await seedDocuments(page, [textDocumentInput()]);

  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(DOC_TEXT_FILE.name);
  await expect(groupOptions(page, 'documents')).toHaveCount(1);
  await page.evaluate(
    (ms) => window.chatPaletteFixture.setPaletteDialogHideDuration(ms),
    DETERMINISTIC_PALETTE_HIDE_MS
  );
  await page.keyboard.press('Enter');

  await page.evaluate(() => {
    history.pushState({}, '', '/terminals');
    window.dispatchEvent(new PopStateEvent('popstate'));
  });

  // Sample every 25ms across the whole (stretched) settle window rather than
  // checking only the final state — a regression here must never report the
  // preview open at all, not just "eventually closed again."
  const samples: boolean[] = [];
  const sampleCount = Math.ceil(DETERMINISTIC_PALETTE_SETTLE_MS / 25);
  for (let i = 0; i < sampleCount; i++) {
    await page.waitForTimeout(25);
    samples.push(await previewDialog(page).isVisible());
  }

  expect(samples.some(Boolean), `preview visibility samples: ${samples.join(',')}`).toBe(false);
  await expect(previewDialog(page)).toBeHidden();
});
