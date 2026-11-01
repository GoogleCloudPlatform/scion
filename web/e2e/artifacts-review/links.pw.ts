/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Following links in the graphical review view (ptone/scion#6848), in a
 * real engine: a Mod-click (Cmd on macOS, Ctrl elsewhere, Playwright's
 * ControlOrMeta) opens exactly one new page at the link's target; a plain
 * click opens nothing and places the caret in the link; a Mod-click leaves
 * the selection a single caret (Firefox's Ctrl-click adds a range
 * otherwise); a middle click opens one page too.
 */

import { expect, test } from '@playwright/test';
import type { BrowserContext, Locator, Page } from '@playwright/test';

import {
  createProject,
  editorText,
  openArtifact,
  publish,
  setExperiments,
  startGraphicalReview,
} from './fixtures.js';

const DOC = ['# Links', '', 'Read the [notes](notes.md) first, then go on.', ''].join('\n');
const NOTES = '# Notes\n\nSome notes.\n';

const editor = (page: Page): Locator => page.locator('scion-review-editor .ProseMirror');
const notesLink = (page: Page): Locator => editor(page).locator('a', { hasText: 'notes' });

/** How many pages the context has after any pending opens settle. */
async function settledPages(context: BrowserContext, page: Page): Promise<number> {
  await page.waitForTimeout(750);
  return context.pages().length;
}

/** Ranges of the document selection (Firefox keeps the editor's selection there). */
async function rangeCount(page: Page): Promise<number> {
  return page.evaluate(() => document.getSelection()?.rangeCount ?? 0);
}

let project = '';

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  await setExperiments(true);
  project = await createProject(`e2e-links-${Date.now().toString(36)}`);
});

test.afterAll(async () => {
  await setExperiments(null);
});

test('a Mod-click on a link opens exactly one new page; a plain click opens none and places the caret', async ({
  page,
  context,
}) => {
  const a = await publish({
    project,
    title: 'Links',
    entry: 'index.md',
    files: { 'index.md': Buffer.from(DOC), 'notes.md': Buffer.from(NOTES) },
  });
  await openArtifact(page, project, a.id);
  await startGraphicalReview(page);
  expect(await editorText(page)).toBe(DOC);
  const link = notesLink(page);
  await expect(link).toHaveAttribute('href', /\/files\/notes\.md$/);
  const start = context.pages().length;

  // Plain click: nothing opens, and typing goes in at the link (a suggestion).
  await link.click();
  expect(await settledPages(context, page)).toBe(start);
  await page.keyboard.type('Z');
  expect(await editorText(page)).toContain('{++Z++}');
  expect(await editorText(page)).toMatch(/\[[^\]]*\{\+\+Z\+\+\}[^\]]*\]\(notes\.md\)/);
  await page.keyboard.press('ControlOrMeta+z');
  expect(await editorText(page)).toBe(DOC);

  // Mod-click: exactly one new page, at the link's file.
  const opened = context.waitForEvent('page');
  await link.click({ modifiers: ['ControlOrMeta'] });
  const tab = await opened;
  expect(await settledPages(context, page)).toBe(start + 1);
  await tab.waitForLoadState('domcontentloaded');
  expect(tab.url()).toMatch(/\/files\/notes\.md$/);
  // The selection is still at most one range (no range added by the click).
  expect(await rangeCount(page)).toBeLessThanOrEqual(1);
  expect(await editorText(page)).toBe(DOC);
  await tab.close();

  // Middle click: one new page too.
  const opened2 = context.waitForEvent('page');
  await link.click({ button: 'middle' });
  const tab2 = await opened2;
  expect(await settledPages(context, page)).toBe(start + 1);
  await tab2.close();
  expect(await editorText(page)).toBe(DOC);
});
