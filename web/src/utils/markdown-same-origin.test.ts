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

  it('replaces off-origin http(s) images with openable links when asked', async () => {
    const r = await getMarkdownRenderer();
    const frag = r.renderFragment(
      '![remote alt](https://elsewhere.example/a.png) ![ok](/x.png) ![inline](data:image/png;base64,AA==)',
      { sameOriginImagesOnly: true }
    );
    const imgs = Array.from(frag.querySelectorAll('img')).map((img) => img.getAttribute('src'));
    expect(imgs).toEqual(['/x.png', 'data:image/png;base64,AA==']);

    const link = frag.querySelector('a');
    expect(link?.getAttribute('href')).toBe('https://elsewhere.example/a.png');
    expect(link?.getAttribute('target')).toBe('_blank');
    expect(link?.getAttribute('rel')).toBe('noopener noreferrer');
    expect(link?.textContent).toBe('remote alt');
  });

  it('falls back to the URL as link label when alt text is empty', async () => {
    const r = await getMarkdownRenderer();
    const frag = r.renderFragment('![](https://elsewhere.example/noalt.png)', {
      sameOriginImagesOnly: true,
    });
    expect(frag.querySelector('img')).toBeNull();
    const link = frag.querySelector('a');
    expect(link?.getAttribute('href')).toBe('https://elsewhere.example/noalt.png');
    expect(link?.getAttribute('target')).toBe('_blank');
    expect(link?.getAttribute('rel')).toBe('noopener noreferrer');
    expect(link?.textContent).toBe('https://elsewhere.example/noalt.png');
  });

  it('links protocol-relative and uppercase HTTPS off-origin images without keeping img tags', async () => {
    const r = await getMarkdownRenderer();
    const frag = r.renderFragment(
      '![proto](//host.example/x.png) ![upper](HTTPS://HOST.EXAMPLE/y.png)',
      { sameOriginImagesOnly: true }
    );
    expect(frag.querySelector('img')).toBeNull();
    const links = Array.from(frag.querySelectorAll('a'));
    expect(links).toHaveLength(2);
    expect(links[0].getAttribute('href')).toMatch(/^https?:\/\/host\.example\/x\.png$/);
    expect(links[0].getAttribute('target')).toBe('_blank');
    expect(links[0].getAttribute('rel')).toBe('noopener noreferrer');
    expect(links[0].textContent).toBe('proto');
    expect(links[1].getAttribute('href')).toBe('https://host.example/y.png');
    expect(links[1].getAttribute('target')).toBe('_blank');
    expect(links[1].getAttribute('rel')).toBe('noopener noreferrer');
    expect(links[1].textContent).toBe('upper');
  });

  it('falls back to plain text for non-http(s) image schemes', async () => {
    const r = await getMarkdownRenderer();
    const frag = r.renderFragment(
      '![js](javascript:alert(1)) ![vb](vbscript:msgbox) ![blob](blob:https://elsewhere.example/1)',
      { sameOriginImagesOnly: true }
    );
    expect(frag.querySelector('img')).toBeNull();
    expect(frag.querySelector('a')).toBeNull();
    expect(frag.textContent).toContain('js');
    expect(frag.textContent).toContain('vb');
    expect(frag.textContent).toContain('blob');
  });

  it('does not create nested anchors when an off-origin image is inside a link', async () => {
    const r = await getMarkdownRenderer();
    const frag = r.renderFragment(
      '[![badge](https://elsewhere.example/badge.svg)](https://example.com/build) [![](https://elsewhere.example/icon.png)](https://example.com/icon)',
      { sameOriginImagesOnly: true }
    );
    expect(frag.querySelector('img')).toBeNull();
    expect(frag.querySelector('a a')).toBeNull();
    const links = Array.from(frag.querySelectorAll('a'));
    expect(links).toHaveLength(2);
    expect(links[0].getAttribute('href')).toBe('https://example.com/build');
    expect(links[0].textContent).toBe('badge');
    expect(links[1].getAttribute('href')).toBe('https://example.com/icon');
    expect(links[1].textContent).toBe('https://elsewhere.example/icon.png');
  });
});
