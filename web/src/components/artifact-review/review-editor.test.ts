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

import { afterEach, describe, expect, it } from 'vitest';

import './review-editor.js';
import { EARLIER_COMMENT_HINT, OWN_COMMENT_HINT, REVIEW_CHANGE_DELAY_MS } from './review-editor.js';
import type { ScionReviewEditor } from './review-editor.js';
import type { ReviewController } from './editor/controller.js';

afterEach(() => {
  document.body.replaceChildren();
});

async function mount(content: string): Promise<{ el: ScionReviewEditor; changes: string[] }> {
  const el = document.createElement('scion-review-editor') as ScionReviewEditor;
  el.content = content;
  el.entryPath = 'doc.md';
  const changes: string[] = [];
  el.addEventListener('review-change', (e) =>
    changes.push((e as CustomEvent<{ text: string }>).detail.text)
  );
  document.body.append(el);
  await el.updateComplete;
  await el.ready;
  return { el, changes };
}

const controllerOf = (el: ScionReviewEditor): ReviewController =>
  (el as unknown as { controller: ReviewController }).controller;

const pause = (): Promise<void> => new Promise((r) => setTimeout(r, REVIEW_CHANGE_DELAY_MS + 50));

/** A composition that replaces "dog" (refused: compositions may only insert). */
function refusedComposition(c: ReviewController): void {
  const view = c.view;
  let p = -1;
  view.state.doc.descendants((n, pos) => {
    if (p < 0 && n.isText && n.text!.includes('dog')) p = pos + n.text!.indexOf('dog');
  });
  c.beginComposition();
  const tr = view.state.tr.insertText('犬', p, p + 3);
  tr.setMeta('composition', 1);
  view.dispatch(tr);
  c.endComposition();
  c.settleComposition();
}

describe('scion-review-editor review-change', () => {
  it('is not sent for a refused composition in an unedited document', async () => {
    const { el, changes } = await mount('a dog\n');
    const c = controllerOf(el);
    refusedComposition(c);
    await pause();
    expect(c.dirty).toBe(false);
    expect(changes).toEqual([]);
  });

  it('is sent after an edit (positive control)', async () => {
    const { el, changes } = await mount('a dog\n');
    const c = controllerOf(el);
    const view = c.view;
    view.dispatch(view.state.tr.insertText('big ', 3));
    await pause();
    expect(changes).toEqual(['a {++big ++}dog\n']);
  });
});

describe('scion-review-editor scrollInset', () => {
  type Sides = { top: number; right: number; bottom: number; left: number };
  const props = (el: ScionReviewEditor): { margin: Sides; threshold: Sides } => {
    const view = controllerOf(el).view;
    return {
      margin: view.someProp('scrollMargin') as Sides,
      threshold: view.someProp('scrollThreshold') as Sides,
    };
  };

  it('keeps a caret scrolled into view clear of the pinned toolbar', async () => {
    const { el } = await mount('a dog\n');
    el.scrollInset = 100;
    await el.updateComplete;
    // A caret under the toolbar counts as out of view, and lands below it.
    expect(props(el).margin.top).toBe(108);
    expect(props(el).threshold.top).toBe(108);
    el.scrollInset = 40.4;
    await el.updateComplete;
    expect(props(el).margin.top).toBe(49);
    expect(props(el).margin.bottom).toBe(5);
  });

  it('keeps focused comment badges and links clear of the pinned toolbar', async () => {
    const { el } = await mount('a {==dog==}{>>Why?<<} and [a link](https://example.com)\n');
    el.scrollInset = 100;
    await el.updateComplete;
    // Focus scrolling reads scroll-margin, which reads this height.
    const root = el.shadowRoot!.querySelector<HTMLElement>('.editor-root')!;
    expect(root.style.getPropertyValue('--review-scroll-inset')).toBe('108px');
    const badge = root.querySelector<HTMLElement>('.critic-comment')!;
    expect(badge.getAttribute('tabindex')).toBe('0');
    const css = Array.from(el.shadowRoot!.adoptedStyleSheets ?? [])
      .flatMap((sheet) => Array.from(sheet.cssRules))
      .map((r) => r.cssText)
      .join('\n');
    expect(css).toMatch(
      /\.editor-root \[tabindex\],\s*\.editor-root a\[href\]\s*\{\s*scroll-margin-top: var\(--review-scroll-inset, 0px\);/
    );
    // 0, nothing pinned: today's behaviour, ProseMirror's defaults and a 0
    // scroll-margin (the CSS default).
    el.scrollInset = 0;
    await el.updateComplete;
    // Always set here, so an inherited value cannot act as a second input.
    expect(root.style.getPropertyValue('--review-scroll-inset')).toBe('0px');
    expect(props(el).margin).toEqual({ top: 5, right: 5, bottom: 5, left: 5 });
    expect(props(el).threshold).toEqual({ top: 0, right: 0, bottom: 0, left: 0 });
  });

  it('treats a negative, NaN or infinite inset as nothing pinned', async () => {
    const { el } = await mount('a dog\n');
    const root = el.shadowRoot!.querySelector<HTMLElement>('.editor-root')!;
    for (const bad of [-40, Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
      el.scrollInset = 100;
      await el.updateComplete;
      el.scrollInset = bad;
      await el.updateComplete;
      expect(root.style.getPropertyValue('--review-scroll-inset'), String(bad)).toBe('0px');
      expect(props(el).margin.top, String(bad)).toBe(5);
      expect(props(el).threshold.top, String(bad)).toBe(0);
    }
  });

  it('applies an inset set before the editor started', async () => {
    const el = document.createElement('scion-review-editor') as ScionReviewEditor;
    el.content = 'a dog\n';
    el.entryPath = 'doc.md';
    el.scrollInset = 60;
    document.body.append(el);
    await el.updateComplete;
    await el.ready;
    expect(props(el).margin.top).toBe(68);
  });
});

/**
 * The comment card (ptone/scion#6850): a read-only tooltip whose last line
 * says how the reviewer's own comment is edited, or that an earlier one is
 * read-only.
 */
describe('scion-review-editor comment card hint', () => {
  async function cardFor(el: ScionReviewEditor, index: number): Promise<Element | null> {
    const badge = controllerOf(el).view.dom.querySelectorAll('.critic-comment')[index];
    expect(badge).toBeDefined();
    badge.dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
    await el.updateComplete;
    return el.shadowRoot!.querySelector('sl-popup .card');
  }

  it('says an earlier comment is read-only, and the own one is edited by click or Enter', async () => {
    const { el } = await mount('one two {>>earlier<<}\n');
    const earlier = await cardFor(el, 0);
    expect(earlier?.querySelector('.card-note')?.textContent).toBe('earlier');
    expect(earlier?.querySelector('.card-hint')?.textContent).toBe(EARLIER_COMMENT_HINT);

    // Add the reviewer's own comment on "two" through the note dialog.
    const dialog = el.shadowRoot!.querySelector('scion-review-note-dialog') as unknown as {
      ask: () => Promise<string>;
    };
    dialog.ask = () => Promise.resolve('mine');
    const c = controllerOf(el);
    let p = -1;
    c.view.state.doc.descendants((n, pos) => {
      if (p < 0 && n.isText && n.text!.includes('two')) p = pos + n.text!.indexOf('two');
    });
    // The editor's own selection class (no ProseMirror import outside the editor).
    type Sel = Parameters<typeof c.view.state.tr.setSelection>[0];
    const TextSel = c.view.state.selection.constructor as unknown as {
      create(doc: unknown, from: number, to: number): Sel;
    };
    c.view.dispatch(c.view.state.tr.setSelection(TextSel.create(c.view.state.doc, p, p + 3)));
    await el.comment();
    await el.updateComplete;
    expect(el.flush()).toBe('one {==two==}{>>mine<<} {>>earlier<<}\n');
    const own = await cardFor(el, 0);
    expect(own?.querySelector('.card-note')?.textContent).toBe('mine');
    expect(own?.querySelector('.card-hint')?.textContent).toBe(OWN_COMMENT_HINT);
    // The card stays a read-only tooltip: no control in it.
    expect(el.shadowRoot!.querySelector('sl-popup')?.getAttribute('role')).toBe('tooltip');
    expect(own?.querySelector('button, sl-button, a, input')).toBeNull();
  });
});

/**
 * An edit made from a comment badge (ptone/scion#6850) is a document change
 * like any other: the element sends review-change with the new text.
 */
describe('scion-review-editor review-change after an edit from a badge', () => {
  /** Mounts "one two three" with the reviewer's own comment "old" on "two". */
  async function withOwnComment(): Promise<{
    el: ScionReviewEditor;
    changes: string[];
    answer: { note: string };
  }> {
    const { el, changes } = await mount('one two three\n');
    const answer = { note: 'old' };
    const dialog = el.shadowRoot!.querySelector('scion-review-note-dialog') as unknown as {
      ask: () => Promise<string>;
    };
    dialog.ask = () => Promise.resolve(answer.note);
    const c = controllerOf(el);
    let p = -1;
    c.view.state.doc.descendants((n, pos) => {
      if (p < 0 && n.isText && n.text!.includes('two')) p = pos + n.text!.indexOf('two');
    });
    // The editor's own selection class (no ProseMirror import outside the editor).
    type Sel = Parameters<typeof c.view.state.tr.setSelection>[0];
    const TextSel = c.view.state.selection.constructor as unknown as {
      create(doc: unknown, from: number, to: number): Sel;
    };
    c.view.dispatch(c.view.state.tr.setSelection(TextSel.create(c.view.state.doc, p, p + 3)));
    await el.comment();
    await pause();
    expect(changes.at(-1)).toBe('one {==two==}{>>old<<} three\n');
    return { el, changes, answer };
  }

  /** Clicks the comment badge as ProseMirror does: handleClickOn on the atom. */
  function clickBadge(el: ScionReviewEditor): boolean {
    const view = controllerOf(el).view;
    let at = -1;
    view.state.doc.descendants((n, pos) => {
      if (at < 0 && n.type.name === 'critic_comment') at = pos;
    });
    expect(at).toBeGreaterThanOrEqual(0);
    const node = view.state.doc.nodeAt(at)!;
    const e = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    return !!view.someProp('handleClickOn', (f) => f(view, at, node, at, e, true));
  }

  it('is sent with the changed note', async () => {
    const { el, changes, answer } = await withOwnComment();
    const before = changes.length;
    answer.note = 'new';
    expect(clickBadge(el)).toBe(true);
    await pause();
    expect(changes.length).toBeGreaterThan(before);
    expect(changes.at(-1)).toBe('one {==two==}{>>new<<} three\n');
  });

  it('is sent without the comment and its highlight after an empty save', async () => {
    const { el, changes, answer } = await withOwnComment();
    const before = changes.length;
    answer.note = '';
    expect(clickBadge(el)).toBe(true);
    await pause();
    expect(changes.length).toBeGreaterThan(before);
    expect(changes.at(-1)).toBe('one two three\n');
  });
});
