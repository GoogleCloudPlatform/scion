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
 * Markdown preview document of the artifact page: where images load from,
 * the frame's policy, and the URL forms shared with the hub's tests.
 */

import { readFileSync } from 'fs';
import { join } from 'path';
import { describe, expect, it } from 'vitest';

import {
  artifactLinkTarget,
  PREVIEW_CSP,
  PREVIEW_SANDBOX,
  imageSource,
  previewDocument,
  resolveBundlePath,
  rewriteImages,
} from './artifact-preview.js';
import type { ArtifactFile } from './artifacts.js';
import { getMarkdownRenderer } from '../utils/markdown.js';

const ID = '5f1c2d3e-0000-4000-8000-000000000001';
const REMOTE = `_remote/${'a'.repeat(64)}`;

function file(path: string, extra: Partial<ArtifactFile> = {}): ArtifactFile {
  return { path, size: 1, sha256: 'ab', mediaType: 'image/png', ...extra };
}

function remote(sourceUrl: string, fetchStatus = 'ok', path = REMOTE): ArtifactFile {
  return file(path, { origin: 'remote', sourceUrl, fetchStatus });
}

function imgs(htmlText: string): Element[] {
  const tpl = document.createElement('template');
  tpl.innerHTML = htmlText;
  return Array.from(tpl.content.querySelectorAll('img, .image-placeholder'));
}

describe('resolveBundlePath', () => {
  it('resolves against the entry folder', () => {
    expect(resolveBundlePath('docs/index.md', 'img/a.png')).toBe('docs/img/a.png');
    expect(resolveBundlePath('docs/index.md', './img/a.png?x=1#y')).toBe('docs/img/a.png');
    expect(resolveBundlePath('docs/index.md', '../top.png')).toBe('top.png');
    expect(resolveBundlePath('index.md', 'a%20b.png')).toBe('a b.png');
  });
  it('refuses paths that leave the bundle or are rooted', () => {
    expect(resolveBundlePath('index.md', '../a.png')).toBeNull();
    expect(resolveBundlePath('docs/index.md', '../../a.png')).toBeNull();
    expect(resolveBundlePath('index.md', '/api/v1/agents')).toBeNull();
    expect(resolveBundlePath('index.md', '\\a.png')).toBeNull();
    expect(resolveBundlePath('index.md', 'a%2fb.png')).toBeNull();
    expect(resolveBundlePath('index.md', '%E0%A4%A.png')).toBeNull();
    expect(resolveBundlePath('index.md', '')).toBeNull();
  });
});

describe('imageSource', () => {
  const files = [file('index.md'), file('img/a.png'), remote('https://h.example/a.png')];
  it('finds files of the version, never remote rows by path', () => {
    expect(imageSource('img/a.png', 'index.md', files)).toEqual({
      kind: 'file',
      path: 'img/a.png',
    });
    expect(imageSource('img/b.png', 'index.md', files)).toEqual({ kind: 'missing' });
    expect(imageSource(REMOTE, 'index.md', files)).toEqual({ kind: 'missing' });
  });
  it('matches absolute URLs to remote rows by the parsed URL', () => {
    expect(imageSource('https://h.example/a.png', 'index.md', files)).toEqual({
      kind: 'remote',
      path: REMOTE,
    });
    expect(imageSource('HTTPS://H.EXAMPLE:443/a.png', 'index.md', files)).toEqual({
      kind: 'remote',
      path: REMOTE,
    });
    expect(imageSource('https://h.example/b.png', 'index.md', files)).toEqual({
      kind: 'not-fetched',
    });
    expect(imageSource('javascript:alert(1)', 'index.md', files)).toEqual({ kind: 'not-fetched' });
    expect(imageSource('ftp://h.example/a.png', 'index.md', files)).toEqual({
      kind: 'not-fetched',
    });
  });
  it('reports a failed fetch', () => {
    expect(
      imageSource('https://h.example/x.png', 'index.md', [
        remote('https://h.example/x.png', 'failed'),
      ])
    ).toEqual({ kind: 'failed' });
  });
  it('keeps data images', () => {
    expect(imageSource('data:image/png;base64,AA==', 'index.md', files)).toEqual({ kind: 'data' });
  });
});

describe('rewriteImages', () => {
  const ctx = {
    id: ID,
    seq: 2,
    entryPath: 'docs/index.md',
    files: [
      file('docs/index.md'),
      file('docs/img/a.png'),
      file('docs/other.md'),
      remote('https://h.example/ok.png'),
      remote('https://h.example/bad.png', 'failed', `_remote/${'b'.repeat(64)}`),
    ],
  };

  it('points images at the version files with stream=1, and replaces the rest', () => {
    const out = rewriteImages(
      '<p><img src="img/a.png" alt="a" srcset="https://evil.example/x.png 2x">' +
        '<img src="https://h.example/ok.png" alt="ok">' +
        '<img src="https://h.example/bad.png" alt="bad">' +
        '<img src="https://h.example/none.png" alt="none">' +
        '<img src="/api/v1/agents" alt="rooted">' +
        '<img src="data:image/png;base64,AA==" alt="d"></p>',
      ctx
    );
    const els = imgs(out);
    expect(els[0].getAttribute('src')).toBe(
      `/api/v1/artifacts/${ID}/versions/2/files/docs/img/a.png?stream=1`
    );
    expect(els[0].hasAttribute('srcset')).toBe(false);
    expect(els[1].getAttribute('src')).toBe(
      `/api/v1/artifacts/${ID}/versions/2/files/_remote/${'a'.repeat(64)}?stream=1`
    );
    expect(els[2].className).toBe('image-placeholder failed');
    expect(els[2].textContent).toBe('Image could not be fetched: bad');
    expect(els[3].className).toBe('image-placeholder not-fetched');
    expect(els[3].textContent).toBe('Image not fetched: none');
    expect(els[4].className).toBe('image-placeholder missing');
    expect(els[5].getAttribute('src')).toBe('data:image/png;base64,AA==');
  });

  it('points relative links at version files and disarms the others', () => {
    const tpl = document.createElement('template');
    tpl.innerHTML = rewriteImages(
      '<a href="other.md">o</a><a href="nope.md">n</a><a href="https://x.example/">x</a><a href="#s">s</a>',
      ctx
    );
    const links = Array.from(tpl.content.querySelectorAll('a'));
    expect(links[0].getAttribute('href')).toBe(
      `/api/v1/artifacts/${ID}/versions/2/files/docs/other.md`
    );
    expect(links[1].hasAttribute('href')).toBe(false);
    expect(links[2].getAttribute('href')).toBe('https://x.example/');
    expect(links[3].hasAttribute('href')).toBe(false);
  });
});

describe('artifactLinkTarget (shared by the preview and the review editor)', () => {
  const ctx = { id: ID, seq: 2, entryPath: 'docs/index.md', files: [file('docs/other.md')] };
  const none = [
    'javascript:alert(1)',
    'JaVaScRiPt:alert(1)',
    'java\tscript:alert(1)',
    ' javascript:alert(1)',
    'data:text/html,x',
    'vbscript:x',
    'blob:https://h.example/x',
    'file:///etc/passwd',
    '//evil.example/x',
    '\\\\evil.example\\x',
    'tel:+15551234',
    'ftp://h.example/f',
    'nope.md',
    '../../outside.md',
    '#',
    '',
  ];
  for (const href of none) {
    it(`gives ${JSON.stringify(href)} no target`, () => {
      expect(artifactLinkTarget(href, ctx)).toEqual({ kind: 'none' });
    });
  }
  it('keeps http, https and mailto, opens version files, and reads fragments', () => {
    expect(artifactLinkTarget('https://x.example', ctx)).toEqual({
      kind: 'external',
      href: 'https://x.example/',
    });
    expect(artifactLinkTarget('HTTP://x.example/a', ctx)).toEqual({
      kind: 'external',
      href: 'http://x.example/a',
    });
    expect(artifactLinkTarget('mailto:a@b.example', ctx)).toEqual({
      kind: 'external',
      href: 'mailto:a@b.example',
    });
    expect(artifactLinkTarget('other.md', ctx)).toEqual({
      kind: 'file',
      href: `/api/v1/artifacts/${ID}/versions/2/files/docs/other.md`,
    });
    expect(artifactLinkTarget('#part-two', ctx)).toEqual({ kind: 'fragment', id: 'part-two' });
  });
  it('strips what the URL parser strips, and nothing more', () => {
    // C0 controls and spaces around the value, tabs and line breaks inside.
    expect(artifactLinkTarget('\u0001 \x1fhttps://x.example/a\tb\u0000 ', ctx)).toEqual({
      kind: 'external',
      href: 'https://x.example/ab',
    });
    // Other whitespace is part of the value, as for the browser: a relative
    // link to a file the version does not have.
    expect(artifactLinkTarget('\u00a0https://x.example/', ctx)).toEqual({ kind: 'none' });
    expect(artifactLinkTarget('\ufeffjavascript:alert(1)', ctx)).toEqual({ kind: 'none' });
    expect(new URL('\u00a0https://x.example/', 'https://base.example/').host).toBe('base.example');
  });
  it('the preview shows tel: and ftp: links and fragments as text, and keeps https', () => {
    const tpl = document.createElement('template');
    tpl.innerHTML = rewriteImages(
      '<a href="tel:+15551234">t</a><a href="ftp://h.example/f">f</a><a href="#s">s</a><a href="https://x.example/">x</a>',
      ctx
    );
    const links = Array.from(tpl.content.querySelectorAll('a'));
    expect(links.map((a) => a.getAttribute('href'))).toEqual([
      null,
      null,
      null,
      'https://x.example/',
    ]);
  });
  it('the preview keeps any data image (the review editor shows raster types only, on purpose)', () => {
    const out = rewriteImages('<img src="data:image/svg+xml;base64,PHN2Zy8+" alt="s">', ctx);
    expect(imgs(out)[0].getAttribute('src')).toBe('data:image/svg+xml;base64,PHN2Zy8+');
  });
});

describe('previewDocument', () => {
  it('carries the policy and keeps theme values from breaking out of the style', () => {
    const doc = previewDocument('<p>hi</p>', { text: 'red;}</style><script>', link: '#123456' });
    expect(doc).toContain(`<meta http-equiv="Content-Security-Policy" content="${PREVIEW_CSP}">`);
    expect(doc).not.toContain('<script>');
    expect(doc).toContain('color:#1e293b');
    expect(doc).toContain('color:#123456');
  });
  it('allows images from the hub only and never scripts', () => {
    expect(PREVIEW_CSP).toBe("default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'");
    expect(PREVIEW_SANDBOX.split(' ')).not.toContain('allow-scripts');
  });
});

/**
 * The URL forms the hub accepts, with the URL it fetched each as. The hub
 * stores that URL as sourceUrl; the preview must find the row from the
 * image as the browser reads it.
 */
describe('remote image URL forms shared with the hub', () => {
  interface Case {
    raw: string;
    kind: 'markdown' | 'attribute';
    want: string;
  }
  const cases = JSON.parse(
    readFileSync(join(__dirname, '../../../pkg/artifacts/testdata/remote_image_urls.json'), 'utf-8')
  ) as Case[];
  const accepted = cases.filter((c) => c.want !== '');

  it('has cases of both kinds', () => {
    expect(accepted.some((c) => c.kind === 'markdown')).toBe(true);
    expect(accepted.some((c) => c.kind === 'attribute')).toBe(true);
  });

  for (const c of accepted) {
    it(`${c.kind} ${JSON.stringify(c.raw)}`, async () => {
      const ctx = {
        id: ID,
        seq: 1,
        entryPath: 'index.md',
        files: [file('index.md'), remote(c.want)],
      };
      let htmlText: string;
      if (c.kind === 'markdown') {
        const renderer = await getMarkdownRenderer();
        htmlText = renderer.render(`![x](${c.raw})`);
      } else {
        htmlText = `<img alt="x" src="${c.raw}">`;
      }
      const [el] = imgs(rewriteImages(htmlText, ctx));
      expect(el.getAttribute('src')).toBe(
        `/api/v1/artifacts/${ID}/versions/1/files/${REMOTE}?stream=1`
      );
    });
  }
});
