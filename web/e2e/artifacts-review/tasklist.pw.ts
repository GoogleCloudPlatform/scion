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
 * Task lists and strikethrough in the graphical review editor
 * (ptone/scion#6395), in each engine: the checkbox is drawn and shows its
 * state, clicking it in suggest mode changes nothing, strikethrough looks
 * apart from a suggested deletion, and the document passes an axe check.
 * In Chromium, the checkbox state is part of the item's accessible text.
 * Suggestion, highlight and comment-badge marks keep AA contrast in the
 * light theme, the dark theme and the system dark preference
 * (ptone/scion#6424).
 */

import { AxeBuilder } from '@axe-core/playwright';
import { expect, test } from '@playwright/test';
import type { Locator, Page } from '@playwright/test';

import {
  createProject,
  editorText,
  openArtifact,
  publish,
  setExperiments,
  startGraphicalReview,
} from './fixtures.js';

const DOC = [
  '# Tasks',
  '',
  '- [ ] Write the plan',
  '- [x] Review the **draft**',
  '  - [ ] Nested open',
  '',
  'Single ~tilde~ and a {--~~struck deletion~~--} here.',
  '',
  'A claim with a note.[^1]',
  '',
  '[^1]: The footnote text.',
  '',
].join('\n');

const editor = (page: Page): Locator => page.locator('scion-review-editor .ProseMirror');
const task = (page: Page, text: string): Locator =>
  editor(page).locator('li[data-item-type="task"]', { hasText: text }).first();

/** The checkbox pseudo-element of a task item: its glyph, size and position. */
async function checkbox(
  item: Locator
): Promise<{ content: string; width: number; left: number; top: number; display: string }> {
  return item.evaluate((li) => {
    const s = getComputedStyle(li, '::before');
    const r = li.getBoundingClientRect();
    return {
      content: s.content,
      width: parseFloat(s.width),
      left: r.left + parseFloat(s.left),
      top: r.top + parseFloat(s.top),
      display: s.display,
    };
  });
}

let project = '';

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  await setExperiments(true);
  project = await createProject(`e2e-tasks-${Date.now().toString(36)}`);
});

test.afterAll(async () => {
  await setExperiments(null);
});

test('task checkboxes are drawn, read-only in suggest mode, and strikethrough stands apart', async ({
  page,
  browserName,
}) => {
  const a = await publish({
    project,
    title: 'Tasks',
    entry: 'index.md',
    files: { 'index.md': Buffer.from(DOC) },
  });
  await openArtifact(page, project, a.id);
  await startGraphicalReview(page);
  expect(await editorText(page)).toBe(DOC);

  // No form control anywhere in the editor.
  await expect(editor(page).locator('input')).toHaveCount(0);

  // The checkbox is drawn, with the item's state.
  const open = await checkbox(task(page, 'Write the plan'));
  const done = await checkbox(task(page, 'Review the'));
  expect(open.content).toContain('☐');
  expect(done.content).toContain('☑');
  expect(open.display).not.toBe('none');
  expect(open.width).toBeGreaterThan(0);
  await expect(task(page, 'Write the plan')).toBeVisible();
  // The list's own bullet is hidden on task items.
  expect(
    await task(page, 'Write the plan').evaluate((li) => getComputedStyle(li).listStyleType)
  ).toBe('none');

  // Clicking a checkbox in suggest mode changes nothing in the document.
  for (const [text, box] of [
    ['Write the plan', open],
    ['Review the', done],
  ] as const) {
    const fontSize = await task(page, text).evaluate((li) =>
      parseFloat(getComputedStyle(li).fontSize)
    );
    const x = box.left + box.width / 2;
    const y = box.top + fontSize * 0.8;
    // Positive control: the point is on the checkbox, which belongs to the
    // task item itself (not its paragraph).
    const hit = await page.locator('scion-review-editor').evaluate(
      (host, p) => {
        const el = host.shadowRoot!.elementFromPoint(p.x, p.y);
        return el?.tagName === 'LI' && el.getAttribute('data-item-type') === 'task'
          ? (el.querySelector('p')?.textContent ?? '')
          : `not a task item: ${el?.tagName ?? 'none'}`;
      },
      { x, y }
    );
    expect(hit.startsWith(text), hit).toBe(true);
    await page.mouse.click(x, y);
    expect(await editorText(page)).toBe(DOC);
  }
  await expect(page.locator('scion-page-artifact-detail .hint-strip')).toHaveCount(0);
  await expect(task(page, 'Review the')).toHaveAttribute('data-checked', 'true');
  await expect(task(page, 'Write the plan')).not.toHaveAttribute('data-checked', 'true');

  // Strikethrough: a plain line in the text colour, no background; a
  // suggested deletion keeps its tinted background, outermost.
  const strike = editor(page).locator('s.gfm-strike', { hasText: 'tilde' });
  const style = await strike.evaluate((s) => {
    const c = getComputedStyle(s);
    const p = getComputedStyle(s.parentElement!);
    return {
      line: c.textDecorationLine,
      bg: c.backgroundColor,
      color: c.color,
      parentColor: p.color,
    };
  });
  expect(style.line).toContain('line-through');
  expect(style.bg).toBe('rgba(0, 0, 0, 0)');
  expect(style.color).toBe(style.parentColor);
  const deletion = editor(page).locator('del.critic-del > s.gfm-strike');
  await expect(deletion).toHaveText('struck deletion');
  expect(
    await editor(page)
      .locator('del.critic-del')
      .evaluate((d) => getComputedStyle(d).backgroundColor)
  ).not.toBe('rgba(0, 0, 0, 0)');

  // Footnotes: the reference as superscript, and the definition block.
  await expect(editor(page).locator('sup')).toHaveText('1');
  await expect(editor(page).locator('dl dd')).toHaveText('The footnote text.');

  // axe on the editor document.
  const results = await new AxeBuilder({ page })
    // A shadow-DOM path: the page, then the editor, then its document.
    .include(['scion-page-artifact-detail', 'scion-review-editor', '.ProseMirror'])
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
    .analyze();
  const serious = results.violations.filter(
    (v) => v.impact === 'serious' || v.impact === 'critical'
  );
  expect(
    serious.map((v) => `${v.id}: ${v.nodes.length}`),
    'no serious axe violations'
  ).toEqual([]);

  if (browserName === 'chromium') {
    // The checkbox state is read before the item text.
    await expect(editor(page).locator('ul').first()).toMatchAriaSnapshot(`
      - list:
        - listitem:
          - text: "Not done:"
          - paragraph: Write the plan
        - listitem:
          - text: "Done:"
          - paragraph:
            - text: Review the
            - strong: draft
    `);
  }

  // The document still saves unchanged.
  expect(await editorText(page)).toBe(DOC);
});

const MARKS_DOC = 'Some {++added++} and {--removed--} and {==marked==}{>>why<<} text.\n';

/**
 * Each mark's text-on-background contrast and its background colour,
 * computed in the page.
 */
async function markContrasts(
  page: Page
): Promise<Record<string, { ratio: number; background: string }>> {
  return editor(page).evaluate((root) => {
    const rgb = (v: string): number[] => {
      // Computed colours of hex tokens are rgb()/rgba(); anything else
      // (oklch(), color()) would be misread, so it fails here instead.
      if (!/^rgba?\(/.test(v)) throw new Error(`not an rgb() colour: ${v}`);
      return (v.match(/[\d.]+/g) ?? []).map(Number);
    };
    const lum = (c: number[]): number => {
      const [r, g, b] = c.slice(0, 3).map((x) => {
        const s = x / 255;
        return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
      });
      return 0.2126 * r + 0.7152 * g + 0.0722 * b;
    };
    const out: Record<string, { ratio: number; background: string }> = {};
    for (const sel of ['ins.critic-ins', 'del.critic-del', 'mark.critic-hl', '.critic-comment']) {
      const s = getComputedStyle(root.querySelector(sel)!);
      const bg = rgb(s.backgroundColor);
      if (bg.length === 4 && bg[3] !== 1) throw new Error(`${sel}: translucent background`);
      const [hi, lo] = [lum(rgb(s.color)), lum(bg)].sort((x, y) => y - x);
      out[sel] = { ratio: (hi + 0.05) / (lo + 0.05), background: s.backgroundColor };
    }
    return out;
  });
}

test('review marks and comment badges keep AA contrast in every theme', async ({ page }) => {
  const a = await publish({
    project,
    title: 'Marks',
    entry: 'index.md',
    files: { 'index.md': Buffer.from(MARKS_DOC) },
  });
  await openArtifact(page, project, a.id);
  await startGraphicalReview(page);
  const seen: Record<string, Record<string, { ratio: number; background: string }>> = {};
  for (const theme of ['light', 'dark', 'system dark']) {
    if (theme === 'system dark') {
      // No theme attribute: the system preference decides.
      await page.emulateMedia({ colorScheme: 'dark' });
      await page.evaluate(() => document.documentElement.removeAttribute('data-theme'));
    } else {
      await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
    }
    seen[theme] = await markContrasts(page);
    for (const [sel, { ratio }] of Object.entries(seen[theme])) {
      expect(ratio, `${sel} in the ${theme} theme`).toBeGreaterThanOrEqual(4.5);
    }
  }
  // The system preference gives the dark colours, not the light ones.
  for (const sel of Object.keys(seen.dark)) {
    expect(seen['system dark'][sel].background, sel).toBe(seen.dark[sel].background);
    expect(seen.dark[sel].background, sel).not.toBe(seen.light[sel].background);
  }
  await page.emulateMedia({ colorScheme: 'light' });
  expect(await editorText(page)).toBe(MARKS_DOC);
});
