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
 * The sanitizer inside renderFragment removes disallowed markup.
 *
 * marked shows raw HTML in markdown as text, so disallowed tags normally never reach
 * the sanitizer. Here marked is replaced by a pass-through, so each case goes
 * through the real renderFragment path (the real DOMPurify instance, its
 * configuration and the link hook) exactly as written. The nodeName fixture
 * is what lets DOMPurify work in the test DOM; markdown-sanitize-control
 * shows the same cases are NOT cleaned without it, so these results come
 * from the sanitizer and not from the test DOM.
 */

import './__fixtures__/sanitizer-node-name.js';
import { describe, it, expect, vi, beforeAll } from 'vitest';
import { DISALLOWED_CASES, disallowedLeft, roundTrip } from './__fixtures__/disallowed-markup.js';

vi.mock('marked', () => ({
  marked: {
    use: (): void => undefined,
    parse: (source: string): string => source,
  },
}));

import { getMarkdownRenderer, type MarkdownRenderer } from './markdown.js';

let renderer: MarkdownRenderer;

beforeAll(async () => {
  renderer = await getMarkdownRenderer();
});

describe('disallowedLeft', () => {
  function check(html: string): string[] {
    const tpl = document.createElement('template');
    tpl.innerHTML = html;
    return disallowedLeft(tpl.content);
  }

  it('reports a srcdoc attribute on any element', () => {
    expect(check('<div srcdoc="x"></div>')).toEqual(['div[srcdoc]']);
  });

  it('reports script and non-image data URLs in a data attribute on any element', () => {
    expect(check('<div data="javascript:alert(1)"></div>')).toEqual(['div[data=javascript:]']);
    expect(check('<div data="data:text/html,x"></div>')).toEqual(['div[data=data:]']);
  });

  it('leaves data-* attributes and ordinary content alone', () => {
    expect(
      check('<div data-x="javascript:alert(1)" data-y="data:text/html,x"><p>ok</p></div>')
    ).toEqual([]);
  });
});

describe('renderFragment removes disallowed markup', () => {
  for (const { name, html, mutation } of DISALLOWED_CASES) {
    it(name, () => {
      if (!mutation) {
        // The checker itself must see the case before sanitizing.
        const raw = document.createElement('template');
        raw.innerHTML = html;
        expect(disallowedLeft(raw.content), 'unsanitized input').not.toEqual([]);
      }

      const fragment = renderer.renderFragment(html);
      expect(disallowedLeft(fragment)).toEqual([]);
      // Still clean if the nodes are ever serialized and parsed again.
      expect(disallowedLeft(roundTrip(fragment)), 'after a round trip').toEqual([]);
    });
  }

  it('keeps ordinary content and sets link target and rel', () => {
    const fragment = renderer.renderFragment(
      '<p>ok <a href="https://example.com/x" target="_self">x</a> <img src="/x.png" alt="a"></p>'
    );
    const a = fragment.querySelector('a');
    expect(a?.getAttribute('href')).toBe('https://example.com/x');
    expect(a?.getAttribute('target')).toBe('_blank');
    expect(a?.getAttribute('rel')).toBe('noopener noreferrer');
    expect(fragment.querySelector('img')?.getAttribute('src')).toBe('/x.png');
  });

  it('drops disallowed images the same way with the image rule on', () => {
    const fragment = renderer.renderFragment(
      '<img src="https://elsewhere.example/a.png" onerror="alert(1)" alt="alt"><img src="/x.png" onerror="alert(1)">',
      { sameOriginImagesOnly: true }
    );
    expect(disallowedLeft(fragment)).toEqual([]);
    expect(Array.from(fragment.querySelectorAll('img')).map((i) => i.getAttribute('src'))).toEqual([
      '/x.png',
    ]);
  });
});
