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

import { describe, it, expect } from 'vitest';
import { copyableLinkHref } from './message-link.js';

const ORIGIN = window.location.origin;

/** A host holding `<a href>` > span, and the composed path from the span. */
function linkPath(href: string | null): { path: EventTarget[]; host: HTMLElement } {
  const host = document.createElement('div');
  const a = document.createElement('a');
  if (href !== null) a.setAttribute('href', href);
  const span = document.createElement('span');
  a.appendChild(span);
  host.appendChild(a);
  return { path: [span, a, host, document.body, document, window], host };
}

describe('copyableLinkHref', () => {
  it.each(['https://example.com/docs?q=1#h', 'http://example.com/', 'mailto:someone@example.com'])(
    'returns the address of a %s link',
    (href) => {
      const { path, host } = linkPath(href);
      expect(copyableLinkHref(path, host)).toBe(new URL(href).href);
    }
  );

  it('resolves a relative app link against the page, as the browser shows it', () => {
    const { path, host } = linkPath('/chat/space/p1');
    expect(copyableLinkHref(path, host)).toBe(`${ORIGIN}/chat/space/p1`);
  });

  it.each(['javascript:alert(1)', 'data:text/html,hi', 'ftp://example.com/f'])(
    'offers nothing for a %s link',
    (href) => {
      const { path, host } = linkPath(href);
      expect(copyableLinkHref(path, host)).toBeNull();
    }
  );

  it('offers nothing for a same-origin blob: link', () => {
    const { path, host } = linkPath(`blob:${ORIGIN}/0f0e0d0c-0b0a-4908-8706-050403020100`);
    expect(new URL(`blob:${ORIGIN}/x`).origin).toBe(ORIGIN);
    expect(copyableLinkHref(path, host)).toBeNull();
  });

  it('does not depend on the page origin: javascript: and data: are refused outright', () => {
    // Their URL origin is the opaque "null"; no page origin can admit them.
    for (const href of ['javascript:void(0)', 'data:text/plain,hi']) {
      expect(new URL(href).origin).toBe('null');
      const { path, host } = linkPath(href);
      expect(copyableLinkHref(path, host)).toBeNull();
    }
  });

  it.each(['#', '', ' # '])('offers nothing for a bare %j href (the file-path links)', (href) => {
    const { path, host } = linkPath(href);
    expect(copyableLinkHref(path, host)).toBeNull();
  });

  it('offers nothing for an anchor without href or a target outside any link', () => {
    const bare = linkPath(null);
    expect(copyableLinkHref(bare.path, bare.host)).toBeNull();
    const host = document.createElement('div');
    const p = document.createElement('p');
    host.appendChild(p);
    expect(copyableLinkHref([p, host, document.body], host)).toBeNull();
  });

  it('ignores links beyond the host', () => {
    const outer = document.createElement('a');
    outer.setAttribute('href', 'https://example.com/outer');
    const host = document.createElement('div');
    const p = document.createElement('p');
    host.appendChild(p);
    outer.appendChild(host);
    expect(copyableLinkHref([p, host, outer, document.body], host)).toBeNull();
  });
});
