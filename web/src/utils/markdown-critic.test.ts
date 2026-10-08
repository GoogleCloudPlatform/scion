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
 * The order of the CriticMarkup pipeline in the shared renderer: marks
 * become elements first, and DOMPurify sanitizes the result, so whatever a
 * mark contains goes through the sanitizer. DOMPurify does not sanitize
 * under happy-dom, so this pins the order by watching its input.
 */

import { describe, expect, it, vi } from 'vitest';

const seen = vi.hoisted(() => ({ inputs: [] as string[] }));

vi.mock('dompurify', () => {
  const purify = {
    addHook: vi.fn(),
    sanitize: vi.fn((html: string) => {
      seen.inputs.push(html);
      return `<!--sanitized-->${html}`;
    }),
  };
  return { default: purify };
});

import { getMarkdownRenderer } from './markdown.js';

describe('markdown renderer with CriticMarkup', () => {
  it('sanitizes the HTML after marks became elements, and returns what the sanitizer made', async () => {
    const r = await getMarkdownRenderer();
    const out = r.render('a {++b++} {~~c~>d~~}{>>note<<}', { criticMarks: true });
    const input = seen.inputs.at(-1) ?? '';
    expect(input).toContain('<ins class=critic-ins>b</ins>');
    expect(input).toContain('<del class=critic-del>c</del>');
    expect(input).toContain('<span class=critic-note>');
    expect(input).not.toMatch(/[\uE000-\uE007]/);
    expect(out.startsWith('<!--sanitized-->')).toBe(true);
  });
});
