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

import { beforeAll, describe, expect, it } from 'vitest';

import type { ScionArtifactMarkdownFrame } from './artifact-markdown-frame.js';

/** The body of a frame document, without the styles. */
function body(doc: string): string {
  return doc.slice(doc.indexOf('<body'));
}

async function frameDoc(
  content: string,
  critic: string,
  props: Partial<ScionArtifactMarkdownFrame> = {}
): Promise<string> {
  const el = document.createElement('scion-artifact-markdown-frame') as ScionArtifactMarkdownFrame;
  el.content = content;
  el.artifactId = 'a';
  el.seq = 1;
  el.entryPath = 'plan.md';
  el.critic = critic as ScionArtifactMarkdownFrame['critic'];
  Object.assign(el, props);
  document.body.appendChild(el);
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
  const doc = el.shadowRoot!.querySelector('iframe')!.srcdoc;
  el.remove();
  return doc;
}

describe('artifact markdown frame: CriticMarkup', () => {
  beforeAll(async () => {
    const hd = (
      window as unknown as { happyDOM?: { settings: { disableIframePageLoading: boolean } } }
    ).happyDOM;
    if (hd) hd.settings.disableIframePageLoading = true;
    await import('./artifact-markdown-frame.js');
  });

  const src = 'We {~~ship~>launch~~} in Q3.{>>date?<<} {++Soon.++}';

  it('renders marks as elements and comments as numbered notes', async () => {
    const doc = await frameDoc(src, 'marks');
    expect(doc).toContain('<del class="critic-del">ship</del><ins class="critic-ins">launch</ins>');
    expect(doc).toContain('<sup class="critic-ref">1</sup>');
    expect(doc).toContain(
      '<span class="critic-note"><span class="critic-note-n">1</span> date?</span>'
    );
    expect(doc).toContain('ins.critic-ins');
  });

  it('shows the clean and accepted projections without marks', async () => {
    const clean = await frameDoc(src, 'clean');
    expect(clean).toContain('We ship in Q3.');
    expect(body(clean)).not.toMatch(/launch|Soon|date\?|critic-ref/);
    const accept = await frameDoc(src, 'accept');
    expect(accept).toContain('We launch in Q3. Soon.');
    expect(body(accept)).not.toMatch(/ship|date\?|critic-ref/);
  });

  it('leaves marks as written when off', async () => {
    const doc = await frameDoc(src, 'off');
    expect(doc).toContain('{&gt;&gt;date?&lt;&lt;} {++Soon.++}');
    expect(doc).not.toMatch(/class="critic-(ins|del|ref|note)"/);
  });

  it('keeps raw HTML inside marks as text', async () => {
    // Marks become elements before the shared sanitizer runs; raw HTML in
    // their content is escaped like raw HTML anywhere in the markdown.
    const doc = await frameDoc('{++<img src=x onerror=alert(1)>++}', 'marks');
    expect(doc).toContain('<ins class="critic-ins">&lt;img src=x onerror=alert(1)&gt;</ins>');
    expect(doc).not.toContain('<img');
  });

  it('keeps marks in link targets, titles and image alt text out of the markup', async () => {
    const doc = body(
      await frameDoc(
        '[li{++nk++}](https://example.com/a{++b++} "t{>>c<<}") ![al{--t--}](img.png)',
        'marks'
      )
    );
    // No element is opened inside an attribute value, and no sentinel
    // character is left in one.
    for (const m of doc.matchAll(/="([^"]*)"/g)) {
      expect(m[1]).not.toMatch(/<|[\uE000-\uE007]/);
    }
    expect(doc).toContain('href="https://example.com/ab"');
    expect(doc).toContain('li<ins class="critic-ins">nk</ins>');
  });

  it('shows notes with their author, a side note, and the margin layout when asked', async () => {
    const wide = body(
      await frameDoc('a{>>c<<}', 'marks', {
        noteAuthor: 'Alex <R>',
        sideNote: 'Side & note',
        marginNotes: true,
      })
    );
    expect(wide).toContain('<body class="notes-margin">');
    expect(wide).toContain('<aside class="critic-side">Side &amp; note</aside>');
    expect(wide).toContain('1 · Alex &lt;R&gt;');
    const narrow = body(
      await frameDoc('a{>>c<<}', 'marks', { sideNote: 'x', sideNoteEmpty: true })
    );
    expect(narrow).toContain('<body><aside class="critic-side empty">x</aside>');
    // Published versions (critic off) show neither.
    const off = body(await frameDoc('a', 'off', { sideNote: 'x', marginNotes: true }));
    expect(off).not.toContain('critic-side');
    expect(off).not.toContain('notes-margin');
  });
});
