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
 * Graphical review editor journey (ptone/scion#5913), in each engine:
 * open a review in the graphical view, type, delete and replace, undo,
 * comment by button and by shortcut, edit a comment, see the refusal
 * hints, toggle to the source view and back, save, reload and see the
 * marks, and the stale review path. In Chromium, an IME composition
 * (through the DevTools protocol) is recorded as an insertion.
 */

import { expect, test } from '@playwright/test';
import type { Locator, Page } from '@playwright/test';

import {
  createProject,
  currentText,
  currentVersion,
  editorText,
  openArtifact,
  publish,
  setExperiments,
  startGraphicalReview,
} from './fixtures.js';

const DOC = [
  '# Review journey',
  '',
  'The first paragraph has some words.',
  '',
  'The second paragraph stays as written.',
  '',
  '* a list item',
  '',
  '| Col | Val |',
  '|:----|----:|',
  '| a | 1 |',
  '',
  '```js',
  'code();',
  '```',
  '',
].join('\n');

const editor = (page: Page): Locator => page.locator('scion-review-editor .ProseMirror');
const hint = (page: Page): Locator => page.locator('scion-page-artifact-detail .hint-strip');

/** Double-clicks a word of the editor (selects it in every engine). */
async function selectWord(page: Page, word: string): Promise<void> {
  const box = await page.locator('scion-review-editor').evaluate((host, w) => {
    const root = host.shadowRoot!.querySelector('.ProseMirror')!;
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const i = n.textContent!.search(new RegExp(`\\b${w}\\b`));
      if (i < 0) continue;
      const r = document.createRange();
      r.setStart(n, i);
      r.setEnd(n, i + w.length);
      const b = r.getBoundingClientRect();
      return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
    }
    return null;
  }, word);
  if (!box) throw new Error(`word ${word} not in the editor`);
  await page.mouse.dblclick(box.x, box.y);
}

/** Puts the cursor at the end of the paragraph or heading that contains text. */
async function endOf(page: Page, text: string): Promise<void> {
  await editor(page).locator(':is(p, h1, h2, h3)', { hasText: text }).first().click();
  await page.keyboard.press('End');
}

let project = '';
const errors: string[] = [];

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  await setExperiments(true);
  project = await createProject(`e2e-review-${Date.now().toString(36)}`);
});

test.afterAll(async () => {
  await setExperiments(null);
});

test.beforeEach(({ page }) => {
  // Uncaught errors anywhere fail the test; console errors and failed
  // requests do when they come from the editor's code or are the editor's
  // own diagnostics (the rest of the page has its own suites).
  page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    if (m.text().includes('review editor') || m.location().url.includes('review-editor')) {
      errors.push(`console: ${m.text()}`);
    }
  });
  page.on('response', (r) => {
    if (r.status() === 404 && /\/(assets|shoelace)\//.test(new URL(r.url()).pathname)) {
      errors.push(`404: ${new URL(r.url()).pathname}`);
    }
  });
});

test.afterEach(() => {
  const found = errors.splice(0);
  expect(found, 'no page errors or console errors').toEqual([]);
});

test('toggling views without an edit never changes the text', async ({ page }) => {
  const a = await publish({
    project,
    title: 'Toggle',
    entry: 'index.md',
    files: { 'index.md': Buffer.from(DOC) },
  });
  await openArtifact(page, project, a.id);
  // Reading (not reviewing) keeps the isolated preview frame with the experiment on.
  await expect(
    page.locator('scion-page-artifact-detail scion-artifact-markdown-frame')
  ).toBeVisible();
  await expect(page.locator('scion-review-editor')).toHaveCount(0);
  await startGraphicalReview(page);
  expect(await editorText(page)).toBe(DOC);
  // Opening a document with a heading shows no hint.
  await expect(page.locator('scion-page-artifact-detail .hint-region')).toBeAttached();
  await expect(hint(page)).toHaveCount(0);
  await page.locator('sl-radio-button[value="source"]').click();
  const code = page.locator('scion-page-artifact-detail scion-code-editor.review-editor');
  await expect(code).toBeVisible();
  expect(await code.evaluate((el) => (el as HTMLElement & { content: string }).content)).toBe(DOC);
  await page.locator('sl-radio-button[value="graphical"]').click();
  await expect(editor(page)).toBeVisible();
  expect(await editorText(page)).toBe(DOC);
  // The choice is remembered: leave in Source, and Review opens in Source.
  await page.locator('sl-radio-button[value="source"]').click();
  await page
    .locator('scion-page-artifact-detail .edit-footer sl-button', { hasText: 'Cancel' })
    .click();
  await page
    .locator('scion-page-artifact-detail .header-actions sl-button', { hasText: 'Review' })
    .click();
  await expect(code).toBeVisible();
  await page.locator('sl-radio-button[value="graphical"]').click();
  await expect(editor(page)).toBeVisible();
});

test('edits become suggestions, comments work by button and shortcut, and the review saves', async ({
  page,
  browserName,
}) => {
  const a = await publish({
    project,
    title: 'Journey',
    entry: 'index.md',
    files: { 'index.md': Buffer.from(DOC) },
  });
  await openArtifact(page, project, a.id);
  await startGraphicalReview(page);

  // Typing at the end of a paragraph inserts.
  await endOf(page, 'second paragraph');
  await page.keyboard.type(' Really.');
  expect(await editorText(page)).toContain('stays as written.{++ Really.++}');

  // Typing inside one's own insertion extends it, without nesting.
  await page.keyboard.press('ArrowLeft');
  await page.keyboard.type('!');
  let text = await editorText(page);
  expect(text).toContain('{++ Really!.++}');
  expect(text).not.toContain('{++{++');

  // One undo reverts one edit.
  await page.keyboard.press('Control+z');
  expect(await editorText(page)).toContain('{++ Really.++}');

  // Typing over a selected word replaces it.
  await selectWord(page, 'some');
  await page.keyboard.type('many');
  expect(await editorText(page)).toContain('has {~~some~>many~~} words.');

  // Deleting a selected word suggests a deletion.
  await selectWord(page, 'first');
  await page.keyboard.press('Backspace');
  expect(await editorText(page)).toContain('The {--first--} paragraph');

  // Comment by toolbar button on a selection.
  await selectWord(page, 'written');
  await page.locator('scion-page-artifact-detail .g-toolbar sl-button.comment').click();
  const dialog = page.locator('scion-review-note-dialog sl-dialog');
  await expect(dialog).toHaveAttribute('open', '');
  await expect(page.locator('scion-review-note-dialog .quote')).toHaveText('written');
  await page.locator('scion-review-note-dialog textarea').fill('Why?');
  await page.keyboard.press('Enter');
  await expect(dialog).not.toHaveAttribute('open', '');
  await expect(dialog.locator('div.dialog[part="base"]')).toBeHidden();
  expect(await editorText(page)).toContain('{==written==}{>>Why?<<}');

  // Comment by shortcut at the cursor.
  await endOf(page, 'Review journey');
  await page.keyboard.press('Control+Alt+m');
  await expect(dialog).toHaveAttribute('open', '');
  await page.locator('scion-review-note-dialog textarea').fill('Title note');
  await page.keyboard.press('Enter');
  await expect(dialog.locator('div.dialog[part="base"]')).toBeHidden();
  expect(await editorText(page)).toContain('# Review journey{>>Title note<<}');

  // The comment card shows a note on hover.
  await editor(page).locator('.critic-comment').first().hover();
  await expect(page.locator('scion-review-editor sl-popup .card-note')).toHaveText('Title note');

  // Edit a comment: select its badge and press Enter.
  await editor(page).locator('.critic-comment').nth(1).click();
  await page.keyboard.press('Enter');
  await expect(dialog).toHaveAttribute('open', '');
  await expect(dialog).toHaveAttribute('label', 'Edit comment 2');
  await page.locator('scion-review-note-dialog textarea').fill('Why not?');
  await page.keyboard.press('Enter');
  await expect(dialog.locator('div.dialog[part="base"]')).toBeHidden();
  expect(await editorText(page)).toContain('{==written==}{>>Why not?<<}');
  // Double-click opens it too; saving it empty deletes it with its highlight.
  await editor(page).locator('.critic-comment').nth(1).dblclick();
  await expect(dialog).toHaveAttribute('label', 'Edit comment 2');
  await page.locator('scion-review-note-dialog textarea').fill('');
  await page.keyboard.press('Enter');
  await expect(dialog.locator('div.dialog[part="base"]')).toBeHidden();
  let afterDelete = await editorText(page);
  expect(afterDelete).toContain('stays as written.');
  expect(afterDelete).not.toContain('Why not?');
  // Put the comment back for the rest of the journey.
  await selectWord(page, 'written');
  await page.locator('scion-page-artifact-detail .g-toolbar sl-button.comment').click();
  await expect(dialog).toHaveAttribute('open', '');
  await page.locator('scion-review-note-dialog textarea').fill('Why?');
  await page.keyboard.press('Enter');
  await expect(dialog.locator('div.dialog[part="base"]')).toBeHidden();
  afterDelete = await editorText(page);
  expect(afterDelete).toContain('{==written==}{>>Why?<<}');

  // Refusals: a paragraph break, formatting, a new list item or table
  // line, and code change nothing.
  const before = await editorText(page);
  await endOf(page, 'second paragraph');
  await page.keyboard.press('Enter');
  await expect(hint(page)).toContainText("Paragraph breaks can't be suggested here yet.");
  await selectWord(page, 'paragraph');
  await page.keyboard.press('Control+b');
  await expect(hint(page)).toContainText("Formatting can't be suggested here.");
  await endOf(page, 'a list item');
  await page.keyboard.press('Enter');
  await expect(hint(page)).toContainText(
    "List and table structure can't be changed in a suggestion."
  );
  await editor(page).locator('td', { hasText: /^a$/ }).click();
  await page.keyboard.press('Enter');
  await expect(hint(page)).toContainText(
    "List and table structure can't be changed in a suggestion."
  );
  await editor(page).locator('pre').click();
  await page.keyboard.type('x');
  await expect(hint(page)).toContainText("Code blocks can't take suggestions.");
  expect(await editorText(page)).toBe(before);
  await expect(editor(page).locator('li')).toHaveText('a list item');

  // Typing in a list item and in a table cell is recorded, and the list's
  // bullet and the table's alignment stay as written.
  await endOf(page, 'a list item');
  await page.keyboard.type('s');
  await expect.poll(() => editorText(page)).toContain('* a list item{++s++}\n');
  await editor(page).locator('td', { hasText: /^a$/ }).click();
  await page.keyboard.press('End');
  await page.keyboard.type('b');
  await expect
    .poll(() => editorText(page))
    .toContain('| Col | Val |\n|:----|----:|\n| a{++b++} | 1 |\n');

  if (browserName === 'chromium') {
    // An IME composition is recorded as an insertion.
    await endOf(page, 'Review journey');
    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Input.imeSetComposition', { text: 'にほ', selectionStart: 2, selectionEnd: 2 });
    await cdp.send('Input.imeSetComposition', {
      text: 'にほん',
      selectionStart: 3,
      selectionEnd: 3,
    });
    await cdp.send('Input.insertText', { text: '日本' });
    await expect.poll(() => editorText(page)).toContain('{++日本++}');
    // Two compositions in a row are both recorded (whether the second
    // starts before the first one's end step is up to the browser; the
    // unit tests drive that order through the DOM events).
    await cdp.send('Input.imeSetComposition', { text: 'か', selectionStart: 1, selectionEnd: 1 });
    await cdp.send('Input.insertText', { text: '下' });
    await cdp.send('Input.imeSetComposition', { text: 'き', selectionStart: 1, selectionEnd: 1 });
    await cdp.send('Input.insertText', { text: '木' });
    await expect.poll(() => editorText(page)).toContain('{++日本下木++}');
    // A composition over a selected word (it would remove text) is undone,
    // with the generic hint.
    const beforeDelete = await editorText(page);
    await selectWord(page, 'journey');
    await cdp.send('Input.imeSetComposition', { text: 'た', selectionStart: 1, selectionEnd: 1 });
    await cdp.send('Input.insertText', { text: '旅' });
    await expect(hint(page)).toContainText("That change can't be recorded as a suggestion.");
    await expect.poll(() => editorText(page)).toBe(beforeDelete);
    await cdp.detach();
  }

  // A switch to Source and back, with no edit, makes the reviewer's own
  // comments earlier: the notice says so, and Enter on one shows the
  // earlier-review hint.
  await page.locator('sl-radio-button[value="source"]').click();
  await expect(hint(page)).toContainText(
    "Suggestions you've made so far become read-only in the graphical view after you switch to Source."
  );
  await page.locator('sl-radio-button[value="graphical"]').click();
  await expect(editor(page)).toBeVisible();
  await editor(page).locator('.critic-comment').nth(1).click();
  await page.keyboard.press('Enter');
  await expect(hint(page)).toContainText('This suggestion is from an earlier review.');
  await expect(dialog).not.toHaveAttribute('open', '');

  // Source view: the tokens are there, and Ctrl+Alt+M comments there too.
  await page.locator('sl-radio-button[value="source"]').click();
  const code = page.locator('scion-page-artifact-detail scion-code-editor.review-editor');
  await expect(code).toBeVisible();
  text = await code.evaluate((el) => (el as HTMLElement & { content: string }).content);
  expect(text).toContain('{~~some~>many~~}');
  await code.locator('.cm-content').click();
  await page.keyboard.press('Control+End');
  await page.keyboard.press('Control+Alt+m');
  await expect
    .poll(() => code.evaluate((el) => (el as HTMLElement & { content: string }).content))
    .not.toBe(text);
  await page.keyboard.press('Control+z');
  await page.locator('sl-radio-button[value="graphical"]').click();
  await expect(editor(page)).toBeVisible();
  const final = await editorText(page);

  // Save review publishes the text as a review version.
  await page
    .locator('scion-page-artifact-detail .edit-footer sl-button', { hasText: 'Save review' })
    .click();
  await expect(
    page.locator('scion-page-artifact-detail sl-badge', { hasText: 'Review pending' })
  ).toBeVisible();
  const v = await currentVersion(a.id);
  expect(v.kind).toBe('review');
  expect(await currentText(a.id, 'index.md')).toBe(final);

  // Reload: the review's marks show inline in the graphical view.
  await page.reload();
  await page
    .locator('scion-page-artifact-detail .header-actions sl-button', { hasText: 'Review' })
    .click();
  await expect(editor(page)).toBeVisible();
  await expect(editor(page).locator('del.critic-del')).toHaveText('first');
  await expect(editor(page).locator('ins.critic-sub-new')).toHaveText('many');
  await expect(editor(page).locator('.critic-comment')).toHaveCount(2);
  expect(await editorText(page)).toBe(final);
});

test('a stale review reloads the current version and keeps the discarded text', async ({
  page,
}) => {
  const a = await publish({
    project,
    title: 'Stale',
    entry: 'index.md',
    files: { 'index.md': Buffer.from(DOC) },
  });
  await openArtifact(page, project, a.id);
  await startGraphicalReview(page);
  await endOf(page, 'second paragraph');
  await page.keyboard.type(' Late.');
  // Someone publishes a new version meanwhile.
  await publish({
    artifactId: a.id,
    entry: 'index.md',
    files: { 'index.md': Buffer.from(DOC + '\nNew.\n') },
  });
  await page
    .locator('scion-page-artifact-detail .edit-footer sl-button', { hasText: 'Save review' })
    .click();
  await expect(page.locator('scion-page-artifact-detail sl-alert[variant="danger"]')).toContainText(
    'Your review was not saved'
  );
  await expect(page.locator('scion-page-artifact-detail .discarded-review')).toContainText(
    '{++ Late.++}'
  );
  await expect(editor(page)).toContainText('New.');
  expect(await editorText(page)).toBe(DOC + '\nNew.\n');
});
