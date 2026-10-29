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
 * renderFragment: rendered markdown as nodes, sanitized in one parse, with
 * the same rules as the string renderer.
 */

import './__fixtures__/sanitizer-node-name.js';
import { describe, it, expect, vi, beforeAll } from 'vitest';

const sanitizeCalls = vi.hoisted(() => [] as unknown[][]);

// The real DOMPurify, with each sanitize call's arguments recorded.
vi.mock('dompurify', async (importOriginal) => {
  const mod = await importOriginal<typeof import('dompurify')>();
  const purify = mod.default;
  const sanitize = purify.sanitize.bind(purify);
  purify.sanitize = ((...args: unknown[]) => {
    sanitizeCalls.push(args);
    return (sanitize as (...a: unknown[]) => unknown)(...args);
  }) as typeof purify.sanitize;
  return mod;
});

import { getMarkdownRenderer, type MarkdownRenderer } from './markdown.js';
import { disallowedLeft } from './__fixtures__/disallowed-markup.js';

let renderer: MarkdownRenderer;

beforeAll(async () => {
  renderer = await getMarkdownRenderer();
});

/** The fragment as markup, for comparing with the string renderer. */
function serialize(fragment: DocumentFragment): string {
  const tpl = document.createElement('template');
  tpl.content.append(fragment);
  return tpl.innerHTML;
}

const SAMPLES = [
  '# Title\n\nSome **bold** and _em_ text.',
  '- one\n- two\n\n1. a\n2. b',
  '```go\nfunc main() {}\n```\n\nand `inline` code',
  '| a | b |\n|---|---|\n| 1 | 2 |',
  '[docs](https://example.com/docs) and <https://example.com/auto>',
  '![remote](https://elsewhere.example/a.png) ![local](/x.png)',
  'raw <template>tags</template> stay text, <b>so does this</b>',
  '> quoted\n\nafter',
  '   leading spaces',
  '[x](javascript:alert(1)) and [y](JaVaScRiPt:alert(1))',
  '[d](data:text/html,<b>x</b>) and ![i](data:text/html,x)',
  '[v](vbscript:msgbox) and [ok](mailto:a@example.com)',
  '<a href="javascript:alert(1)">raw</a> <img src=x onerror=alert(1)>',
];

describe('renderFragment', () => {
  it('produces the same markup as render', () => {
    for (const md of SAMPLES) {
      expect(serialize(renderer.renderFragment(md)), md).toBe(renderer.render(md));
    }
  });

  it('produces the same markup as render with critic marks', () => {
    const md = 'Keep {++this++} and {--that--}, {==see==}{>>a note<<}';
    for (const criticAuthor of ['Ada', 'A "quoted" <b>name</b> & co', "it's <img src=x>"]) {
      const options = { criticMarks: true, criticAuthor };
      expect(serialize(renderer.renderFragment(md, options)), criticAuthor).toBe(
        renderer.render(md, options)
      );
    }
  });

  it('returns nodes owned by this document', () => {
    const fragment = renderer.renderFragment('hello **there**');
    expect(fragment.ownerDocument).toBe(document);
    expect(fragment.querySelector('strong')?.ownerDocument).toBe(document);
  });

  it('opens links in a new tab without an opener or referrer', () => {
    const a = renderer.renderFragment('[docs](https://example.com/docs)').querySelector('a');
    expect(a?.getAttribute('target')).toBe('_blank');
    expect(a?.getAttribute('rel')).toBe('noopener noreferrer');
  });

  it('keeps only same-origin images when asked, alt text in place of the rest', () => {
    const fragment = renderer.renderFragment(
      '![remote alt](https://elsewhere.example/a.png) ![ok](/x.png) ![data](data:image/png;base64,AA==)',
      { sameOriginImagesOnly: true }
    );
    const srcs = Array.from(fragment.querySelectorAll('img')).map((img) => img.getAttribute('src'));
    expect(srcs).toEqual(['/x.png', 'data:image/png;base64,AA==']);
    expect(fragment.textContent).toContain('remote alt');
  });

  it('keeps remote images by default', () => {
    const img = renderer
      .renderFragment('![a](https://elsewhere.example/a.png)')
      .querySelector('img');
    expect(img?.getAttribute('src')).toBe('https://elsewhere.example/a.png');
  });

  it('render with sameOriginImagesOnly still drops off-origin images', () => {
    const out = renderer.render('![remote alt](https://elsewhere.example/a.png) ![ok](/x.png)', {
      sameOriginImagesOnly: true,
    });
    expect(out).not.toContain('elsewhere.example');
    expect(out).toContain('remote alt');
    expect(out).toContain('src="/x.png"');
  });

  it('leaves nothing disallowed from markdown that tries it', () => {
    const md = [
      '[a](javascript:alert(1)) [b](JaVaScRiPt:alert(1)) [c](data:text/html,x)',
      '![d](javascript:alert(1)) ![e](https://elsewhere.example/e.png "x\\" onerror=\\"alert(1)")',
      '<script>alert(1)</script> <img src=x onerror=alert(1)> <a href="javascript:alert(1)">f</a>',
      '<javascript:alert(1)> [g](<javascript:alert(1)>)',
    ].join('\n\n');
    for (const options of [{}, { sameOriginImagesOnly: true }, { criticMarks: true }]) {
      expect(disallowedLeft(renderer.renderFragment(md, options)), JSON.stringify(options)).toEqual(
        []
      );
    }
  });

  it('sanitizes once, with the default configuration plus the node result', () => {
    sanitizeCalls.length = 0;
    renderer.renderFragment('one **two**', { sameOriginImagesOnly: true });
    expect(sanitizeCalls).toHaveLength(1);
    expect(typeof sanitizeCalls[0][0]).toBe('string');
    expect(sanitizeCalls[0][1]).toEqual({ RETURN_DOM_FRAGMENT: true });

    sanitizeCalls.length = 0;
    renderer.render('one **two**');
    expect(sanitizeCalls).toHaveLength(1);
    expect(sanitizeCalls[0]).toHaveLength(1);
  });
});
