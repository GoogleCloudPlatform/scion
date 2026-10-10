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

import { describe, expect, it } from 'vitest';

import { getMarkdownRenderer, isSameOriginImageSrc } from './markdown.js';

const ORIGIN = 'https://hub.example';

describe('isSameOriginImageSrc', () => {
  it('accepts relative, same-origin and data images', () => {
    for (const src of [
      'img/a.png',
      '/api/v1/x.png',
      'https://hub.example/a.png',
      'data:image/png;base64,AA',
    ]) {
      expect(isSameOriginImageSrc(src, ORIGIN), src).toBe(true);
    }
  });

  it('rejects other hosts and schemes', () => {
    for (const src of [
      'https://elsewhere.example/a.png',
      'http://hub.example/a.png',
      '//elsewhere.example/a.png',
      'javascript:alert(1)',
      'data:text/html,<p>x</p>',
      '',
    ]) {
      expect(isSameOriginImageSrc(src, ORIGIN), src).toBe(false);
    }
  });
});

describe('renderer sameOriginImagesOnly', () => {
  it('keeps remote images by default (other previews are unchanged)', async () => {
    const r = await getMarkdownRenderer();
    expect(r.render('![a](https://elsewhere.example/a.png)')).toContain(
      'src="https://elsewhere.example/a.png"'
    );
  });

  it('replaces off-origin images with their alt text when asked', async () => {
    const r = await getMarkdownRenderer();
    const out = r.render('![remote alt](https://elsewhere.example/a.png) ![ok](/x.png)', {
      sameOriginImagesOnly: true,
    });
    expect(out).not.toContain('elsewhere.example');
    expect(out).toContain('remote alt');
    expect(out).toContain('src="/x.png"');
  });
});
