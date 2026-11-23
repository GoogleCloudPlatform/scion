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
 * Suite-wide guard: DOMPurify must actively sanitize markup under the default
 * Vitest DOM setup (web/vitest.setup.ts) without any per-file fixture import.
 *
 * Under Happy DOM without the polyfills in web/vitest.setup.ts:
 * - Node.prototype.nodeName returns '' for elements, causing DOMPurify to stop
 *   walking early, leave disallowed markup in place, and skip hooks.
 * - Document.prototype.createNodeIterator delegates to TreeWalker without
 *   WHATWG pre-removing steps, so removing a disallowed element (e.g. <script>)
 *   detaches the iterator and leaves every subsequent sibling unsanitized.
 *
 * If this test ever fails after a DOMPurify or Happy DOM upgrade, sanitizer
 * unit tests across the suite are no longer exercising real sanitization.
 */

import DOMPurify from 'dompurify';
import { describe, expect, it } from 'vitest';
import { disallowedLeft } from './__fixtures__/disallowed-markup.js';
import { getMarkdownRenderer } from './markdown.js';

describe('vitest DOM sanitizer setup guard', () => {
  it('exposes element tag names through Node.prototype.nodeName', () => {
    const getter = Object.getOwnPropertyDescriptor(Node.prototype, 'nodeName')?.get;
    expect(getter).toBeTypeOf('function');
    expect(getter?.call(document.createElement('div'))).toBe('DIV');
    expect(getter?.call(document.createElement('script'))).toBe('SCRIPT');
    expect(getter?.call(document.createTextNode('hi'))).toBe('#text');
  });

  it('removes disallowed elements and attributes via DOMPurify.sanitize', () => {
    const dirty =
      '<p>safe</p><script>alert(1)</script><img src="/x.png" onerror="alert(1)"><a href="javascript:alert(1)">bad</a>';

    const cleanHtml = DOMPurify.sanitize(dirty);
    const tpl = document.createElement('template');
    tpl.innerHTML = cleanHtml;
    expect(disallowedLeft(tpl.content)).toEqual([]);
    expect(tpl.content.querySelector('p')?.textContent).toBe('safe');
    expect(tpl.content.querySelector('img')?.getAttribute('src')).toBe('/x.png');

    const fragment = DOMPurify.sanitize(dirty, { RETURN_DOM_FRAGMENT: true });
    expect(disallowedLeft(fragment)).toEqual([]);
    expect(fragment.querySelector('p')?.textContent).toBe('safe');
  });

  it('continues sanitizing siblings after a <template> element', () => {
    const afterTemplate = '<template><b>x</b></template><p onclick="alert(1)">t</p>';
    const cleanHtml = DOMPurify.sanitize(afterTemplate);
    const tpl = document.createElement('template');
    tpl.innerHTML = cleanHtml;
    expect(disallowedLeft(tpl.content)).toEqual([]);
    expect(tpl.content.querySelector('p')?.textContent).toBe('t');

    const removedAfterTemplate =
      '<template><b>x</b></template><script>alert(1)</script><img src="/x.png" onerror="alert(1)">';
    const fragment = DOMPurify.sanitize(removedAfterTemplate, { RETURN_DOM_FRAGMENT: true });
    expect(disallowedLeft(fragment)).toEqual([]);
    expect(fragment.querySelector('img')?.getAttribute('src')).toBe('/x.png');
  });

  it('executes DOMPurify hooks in getMarkdownRenderer without per-file opt-ins', async () => {
    const renderer = await getMarkdownRenderer();
    const fragment = renderer.renderFragment('[docs](https://example.com/docs)');
    const link = fragment.querySelector('a');
    expect(link?.getAttribute('href')).toBe('https://example.com/docs');
    expect(link?.getAttribute('target')).toBe('_blank');
    expect(link?.getAttribute('rel')).toBe('noopener noreferrer');
  });
});
