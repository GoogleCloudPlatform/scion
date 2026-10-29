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
import { REVIEW_CHANGE_DELAY_MS } from './review-editor.js';
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
