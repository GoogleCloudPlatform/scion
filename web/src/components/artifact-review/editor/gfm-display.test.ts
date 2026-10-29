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
 * Task lists, strikethrough and footnotes in the graphical view
 * (ptone/scion#6395): task items show a checkbox drawn from their own
 * attributes and announced by its state, strikethrough renders apart from
 * a suggested deletion, and the document saves byte for byte, unedited
 * and after one text edit per construct.
 */

import { afterEach, describe, expect, it } from 'vitest';
import { TextSelection } from '@milkdown/prose/state';

import { onlyMarksChanged, projectCritic } from '../../../utils/critic.js';
import { reviewEditorStyles } from '../review-editor-styles.js';
import { ReviewController } from './controller.js';
import type { HintKind } from './policy.js';
import { renderContext } from './__fixtures__/core.js';

const GFM_FIXTURE = [
  '# Tasks, strikethrough and footnotes',
  '',
  '- [ ] Write the plan',
  '- [x] Review the **draft** with `code`',
  '- [ ] Parent task',
  '  - [x] Nested done',
  '  - [ ] Nested open',
  '',
  'Single ~tilde~ and double ~~struck words~~ strike.',
  '',
  'A {--~~struck deletion~~--} and ~~struck {--inner--} text~~ here.',
  '',
  'A claim with a note.[^1]',
  '',
  '[^1]: The footnote text.',
  '',
].join('\n');

const made: ReviewController[] = [];
afterEach(async () => {
  for (const c of made.splice(0)) await c.destroy();
  document.body.replaceChildren();
});

async function open(md: string, hints: HintKind[] = []): Promise<ReviewController> {
  const root = document.createElement('div');
  document.body.append(root);
  const c = await ReviewController.create({
    root,
    render: () => renderContext,
    askNote: () => Promise.resolve(null),
    onHint: (h) => hints.push(h),
    onChange: () => {},
  });
  made.push(c);
  expect(c.load(md)).toBe(true);
  return c;
}

/**
 * The document position just after the first occurrence of needle in a
 * text node, searching only the textblocks whose text contains within.
 */
function after(c: ReviewController, needle: string, within: string): number {
  let found = -1;
  c.view.state.doc.descendants((n, pos) => {
    if (found >= 0) return false;
    if (!n.isTextblock) return true;
    if (!n.textContent.includes(within)) return false;
    n.forEach((child, offset) => {
      if (found >= 0 || !child.isText || !child.text!.includes(needle)) return;
      found = pos + 1 + offset + child.text!.indexOf(needle) + needle.length;
    });
    return false;
  });
  if (found < 0) throw new Error(`no ${needle} in a block with ${within}`);
  return found;
}

function typeAt(c: ReviewController, text: string, at: number): void {
  const v = c.view;
  v.dispatch(v.state.tr.setSelection(TextSelection.create(v.state.doc, at)));
  v.dispatch(v.state.tr.insertText(text, at, at));
}

describe('task lists', () => {
  it('render as list items with only the fixed task attributes, and no input', async () => {
    const c = await open(GFM_FIXTURE);
    const dom = c.view.dom;
    expect(dom.querySelectorAll('input').length).toBe(0);
    const tasks = Array.from(dom.querySelectorAll('li[data-item-type="task"]'));
    expect(tasks.length).toBe(5);
    for (const li of tasks) {
      const names = Array.from(li.attributes).map((a) => a.name);
      for (const n of names) expect(['data-item-type', 'data-checked'], n).toContain(n);
      expect(li.getAttribute('data-checked') ?? 'false').toMatch(/^(true|false)$/);
    }
    const ticked = tasks
      .filter((li) => li.getAttribute('data-checked') === 'true')
      .map((li) => li.querySelector('p')?.textContent);
    expect(ticked).toEqual(['Review the draft with code', 'Nested done']);
    // Marks in the item text still render.
    expect(tasks[1].querySelector('strong')?.textContent).toBe('draft');
    expect(tasks[1].querySelector('code')?.textContent).toBe('code');
  });

  it('draw the checkbox in CSS with its state as alternative text', () => {
    const css = reviewEditorStyles.cssText.replace(/\s+/g, ' ');
    expect(css).toContain(".ProseMirror li[data-item-type='task'] { list-style: none; }");
    expect(css).toContain("content: '☐' / 'Not done: ';");
    expect(css).toContain(
      ".ProseMirror li[data-item-type='task'][data-checked='true']::before { content: '☑'; content: '☑' / 'Done: ';"
    );
    // Visible in forced colours.
    expect(css).toMatch(
      /@media \(forced-colors: active\) \{[^@]*li\[data-item-type='task'\]::before \{ color: CanvasText; \}/
    );
  });
});

describe('strikethrough', () => {
  it('renders as s.gfm-strike, inside a suggested deletion when both apply', async () => {
    const c = await open(GFM_FIXTURE);
    const dom = c.view.dom;
    const strikes = Array.from(dom.querySelectorAll('s')).map((s) => [s.className, s.textContent]);
    expect(strikes).toEqual([
      ['gfm-strike', 'tilde'],
      ['gfm-strike', 'struck words'],
      ['gfm-strike', 'struck deletion'],
      ['gfm-strike', 'struck '],
      ['gfm-strike', 'inner'],
      ['gfm-strike', ' text'],
    ]);
    // Every del is a suggestion; struck text inside a deletion, either way
    // round in the text, renders del.critic-del > s.gfm-strike.
    expect(dom.querySelectorAll('del:not(.critic-del):not(.critic-sub-old)').length).toBe(0);
    expect(
      Array.from(dom.querySelectorAll('del.critic-del > s.gfm-strike')).map((s) => s.textContent)
    ).toEqual(['struck deletion', 'inner']);
    expect(dom.querySelectorAll('s del, s ins, s mark').length).toBe(0);
    for (const s of Array.from(dom.querySelectorAll('s'))) {
      expect(Array.from(s.attributes).map((a) => a.name)).toEqual(['class']);
    }
  });

  it('is styled as a plain line through, and suggested deletions keep their own style', () => {
    const css = reviewEditorStyles.cssText.replace(/\s+/g, ' ');
    expect(css).toContain(
      's.gfm-strike { color: inherit; background: none; text-decoration: line-through; text-decoration-color: currentColor; }'
    );
    expect(css).toMatch(
      /del\.critic-del, del\.critic-sub-old \{ color: var\(--scion-review-del-text/
    );
  });
});

describe('footnotes', () => {
  it('show the reference as superscript and the definition block', async () => {
    const c = await open(GFM_FIXTURE);
    const dom = c.view.dom;
    expect(dom.querySelector('sup')?.textContent).toBe('1');
    expect(dom.querySelector('dl dd')?.textContent).toBe('The footnote text.');
    expect(dom.querySelectorAll('a').length).toBe(0);
  });
});

describe('saving the fixture', () => {
  it('saves unedited byte for byte', async () => {
    const c = await open(GFM_FIXTURE);
    expect(c.flush()).toBe(GFM_FIXTURE);
  });

  // One text edit per construct: the edit lands where its row says (the
  // block holding `within`, just after `needle`), and the saved text is the
  // fixture with only that insertion added, so every marker stays as written.
  const EDITS: [name: string, needle: string, within: string, lands: string][] = [
    ['heading', 'Tasks', 'Tasks, strike', '# Tasks{++X++}, strikethrough and footnotes\n'],
    ['unticked item', 'Write', 'Write the plan', '- [ ] Write{++X++} the plan\n'],
    ['ticked item with marks', 'Review', 'Review the', '- [x] Review{++X++} the **draft**'],
    ['parent task item', 'Parent', 'Parent task', '- [ ] Parent{++X++} task\n'],
    ['nested ticked item', 'Nested', 'Nested done', '  - [x] Nested{++X++} done\n'],
    ['nested unticked item', 'open', 'Nested open', '  - [ ] Nested open{++X++}\n'],
    ['text before strikethrough', 'Single', 'Single', 'Single{++X++} ~tilde~'],
    ['single-tilde strikethrough', 'til', 'Single', 'Single ~til{++X++}de~'],
    ['double-tilde strikethrough', 'struck w', 'Single', '~~struck w{++X++}ords~~'],
    [
      'text after a struck deletion',
      ' and',
      'struck deletion',
      '{--~~struck deletion~~--} and{++X++} ~~struck',
    ],
    [
      'struck text after a deletion inside it',
      ' tex',
      'struck deletion',
      '~~struck {--inner--} tex{++X++}t~~',
    ],
    ['footnote reference paragraph', 'claim', 'A claim', 'A claim{++X++} with a note.[^1]'],
    ['footnote definition', 'footnote', 'The footnote text', '[^1]: The footnote{++X++} text.'],
  ];

  it.each(EDITS)(
    'after one edit in the %s, keeps every marker',
    async (_name, needle, within, lands) => {
      const hints: HintKind[] = [];
      const c = await open(GFM_FIXTURE, hints);
      typeAt(c, 'X', after(c, needle, within));
      const out = c.flush();
      expect(hints).toEqual([]);
      expect(out).not.toBeNull();
      expect(out).toContain(lands);
      expect(onlyMarksChanged(out!, projectCritic(GFM_FIXTURE, 'clean'))).toBe(true);
      expect(out!.split('{++X++}').length).toBe(2);
      expect(out!.replace('{++X++}', '')).toBe(GFM_FIXTURE);
    }
  );

  it('has an edit for every textblock of the fixture', async () => {
    const c = await open(GFM_FIXTURE);
    const blocks: string[] = [];
    c.view.state.doc.descendants((n) => {
      if (!n.isTextblock) return true;
      blocks.push(n.textContent);
      return false;
    });
    expect(blocks.length).toBeGreaterThan(0);
    // The block each row edits: the first one holding its `within`, as after() finds it.
    const edited = new Set(
      EDITS.map(([, , within]) => blocks.findIndex((b) => b.includes(within)))
    );
    blocks.forEach((b, i) => expect(edited.has(i), b).toBe(true));
  });
});
