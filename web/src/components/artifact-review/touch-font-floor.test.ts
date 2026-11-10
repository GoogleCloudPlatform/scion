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
 * Every place text is typed during an artifact review is 16px or more on a
 * touch screen. iOS Safari zooms in on a field under 16px when it takes
 * focus and keeps that zoom after the keyboard closes; client/viewport.ts
 * stands down while the page is zoomed, so the pan is never undone and the
 * pinned review toolbar (with Comment) is left above the visible area.
 *
 * Each surface keeps its desktop size and adds a coarse-pointer rule that
 * floors it at 16px. e2e/artifacts-review/mobile.pw.ts checks the computed
 * sizes in a browser.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const read = (f: string): string => readFileSync(resolve(__dirname, f), 'utf8');
const flat = (s: string): string => s.replace(/\s+/g, ' ');

const SURFACES: [file: string, selector: string, desktop: string][] = [
  ['review-note-dialog.ts', 'textarea', '0.875rem'],
  ['review-editor-styles.ts', '.ProseMirror', '0.9375rem'],
  ['../shared/code-editor.ts', '.editor-container .cm-editor', '0.875rem'],
];

const escape = (s: string): string => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

describe('review text surfaces on a touch screen', () => {
  it.each(SURFACES)('%s floors %s at 16px', (file, selector, desktop) => {
    const src = flat(read(file));
    const sel = escape(selector);
    // The desktop size is unchanged...
    const base = new RegExp(`${sel} \\{[^}]*font-size: ${escape(desktop)};`).exec(src);
    expect(base, 'desktop rule').not.toBeNull();
    // ...and a coarse pointer raises it to at least 16px.
    const media = new RegExp(
      `@media \\(pointer: coarse\\) \\{ ${sel} \\{ font-size: max\\(16px, ${escape(desktop)}\\); \\}`
    ).exec(src);
    expect(media, 'coarse-pointer rule').not.toBeNull();
    // Same specificity, so the floor wins only by coming later.
    expect(media!.index).toBeGreaterThan(base!.index);
  });

  it("floors CodeMirror's own panel fields (70% of the editor in its base theme) at 16px", () => {
    const src = flat(read('../shared/code-editor.ts'));
    expect(src).toMatch(
      /@media \(pointer: coarse\) \{ [^@]*\.editor-container \.cm-editor \.cm-textfield, \.editor-container \.cm-editor \.cm-panel input \{ font-size: max\(16px, 70%\); \}/
    );
  });
});
