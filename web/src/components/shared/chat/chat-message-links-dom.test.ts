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
 * The message link passes (artifact references, @mentions, entity references,
 * GitHub refs) build their links over the text nodes of the rendered markup
 * (ptone/scion#3745). These tests use the real markdown renderer, so the
 * markup is what marked and DOMPurify actually produce.
 */

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { ARTIFACTS_FLAG } from '../../../client/artifacts.js';

await import('./chat-message.js');
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;

declare global {
  interface Window {
    __SCION_FEATURES__?: Record<string, boolean>;
  }
}

const UUID = '123e4567-e89b-12d3-a456-426614174000';

/** Mount an agent's message and wait for its markdown to render. */
async function mount(body: string): Promise<HTMLElement> {
  const el = document.createElement('scion-chat-message') as ScionChatMessage;
  el.body = body;
  el.fromAgent = true;
  el.senderIsAgent = true;
  document.body.appendChild(el);
  let content: HTMLElement | null = null;
  for (let i = 0; i < 50 && !content; i++) {
    await el.updateComplete;
    content = el.shadowRoot?.querySelector<HTMLElement>('.md-content') ?? null;
    if (!content) await new Promise((resolve) => setTimeout(resolve, 5));
  }
  if (!content) throw new Error('the message did not render its markdown');
  return content;
}

function mentionSlugs(root: Element): string[] {
  return Array.from(root.querySelectorAll('.mention')).map((m) => m.getAttribute('data-mention')!);
}

beforeEach(() => {
  document.body.innerHTML = '';
  window.__SCION_FEATURES__ = { 'web.gcs_links': true, [ARTIFACTS_FLAG]: true };
});

afterEach(() => {
  document.body.innerHTML = '';
  delete window.__SCION_FEATURES__;
});

describe('scion-chat-message links inside markup', () => {
  it('links references inside emphasis, list and table markup', async () => {
    const content = await mount(
      '**@alice** _/workspace/a.md_ *commit abc1234*\n\n- ptone/scion#4\n\n| x |\n|---|\n| @bob gs://bkt/t.md |'
    );

    expect(content.querySelector('strong .mention')?.getAttribute('data-mention')).toBe('alice');
    expect(content.querySelector('em .path-link')?.getAttribute('data-file-path')).toBe(
      '/workspace/a.md'
    );
    expect(content.querySelector('em a[href="/commits/abc1234"]')?.textContent).toBe('abc1234');
    expect(content.querySelector('li .gh-ref-link')?.getAttribute('href')).toBe(
      'https://github.com/ptone/scion/issues/4'
    );
    expect(content.querySelector('td .mention')?.getAttribute('data-mention')).toBe('bob');
    expect(content.querySelector('td .gcs-link')?.getAttribute('data-gcs-uri')).toBe(
      'gs://bkt/t.md'
    );
  });

  it("styles an @name in a link's text but does not link a path or ref inside it again", async () => {
    const content = await mount('[@alice /workspace/a.md ptone/scion#1](https://example.com/x)');

    const link = content.querySelector('a[href="https://example.com/x"]')!;
    expect(mentionSlugs(link)).toEqual(['alice']);
    expect(link.querySelector('a')).toBeNull();
    expect(link.textContent).toBe('@alice /workspace/a.md ptone/scion#1');
  });

  it('leaves code alone, except that a path in inline code still links', async () => {
    const content = await mount(
      '`@alice /workspace/a.ts ptone/scion#2`\n\n```\n@bob /workspace/b.ts sess_abcd1234\n```'
    );

    const inline = Array.from(content.querySelectorAll('code')).find((c) => !c.closest('pre'))!;
    expect(inline.querySelector('.mention, .gh-ref-link')).toBeNull();
    expect(inline.querySelector('.path-link')?.getAttribute('data-file-path')).toBe(
      '/workspace/a.ts'
    );
    expect(content.querySelector('pre')!.querySelector('a, span')).toBeNull();
  });

  it('keeps a version suffix part of the artifact link, not a mention', async () => {
    const content = await mount(`see scion://artifact/${UUID}@2 and @alice`);

    const link = content.querySelector('.artifact-link')!;
    expect(link.textContent).toBe(`scion://artifact/${UUID}@2`);
    expect(link.getAttribute('data-artifact-seq')).toBe('2');
    expect(link.querySelector('.mention')).toBeNull();
    expect(mentionSlugs(content)).toEqual(['alice']);
  });

  it('leaves a link title and an image alt that contain ">" and references as they are', async () => {
    const content = await mount(
      '[t](https://example.com "a>@bob sess_abcd1234") ' +
        '![alt >@dan /workspace/i.md](/i.png "t>@erin") @carol'
    );

    const link = content.querySelector('a[href="https://example.com"]')!;
    expect(link.getAttribute('title')).toBe('a>@bob sess_abcd1234');
    expect(link.textContent).toBe('t');
    const img = content.querySelector('img')!;
    expect(img.getAttribute('alt')).toBe('alt >@dan /workspace/i.md');
    expect(img.getAttribute('title')).toBe('t>@erin');
    // Only the mention in the text is styled, and nothing else was linked.
    expect(mentionSlugs(content)).toEqual(['carol']);
    expect(content.querySelectorAll('.entity-link')).toHaveLength(0);
  });
});

describe('scion-chat-message text that looks like HTML', () => {
  it('shows tag-like text as text and still links the references in it', async () => {
    const content = await mount(
      '<b>@alice</b> <a href="https://example.com">/workspace/a.md</a> <span class="mention">x</span>'
    );

    expect(content.querySelector('b, span.mention:not([data-mention])')).toBeNull();
    expect(content.querySelector('a[href="https://example.com"]')).toBeNull();
    expect(content.textContent).toContain('<b>@alice</b>');
    expect(content.textContent).toContain('<span class="mention">x</span>');
    expect(mentionSlugs(content)).toEqual(['alice']);
    expect(content.querySelector('.path-link')?.getAttribute('data-file-path')).toBe(
      '/workspace/a.md'
    );
  });

  it('reads entity and ampersand text as the text it is', async () => {
    const content = await mount(
      'a &amp; b @x&y /workspace/a&b.md /workspace/c.md&d gs://bkt/o&q gs://bkt/p& r'
    );

    // The markdown entity is text by now: one '&', not markup.
    expect(content.textContent).toContain('a & b');
    expect(mentionSlugs(content)).toEqual(['x']);
    expect(
      Array.from(content.querySelectorAll('.path-link')).map((a) =>
        a.getAttribute('data-file-path')
      )
    ).toEqual(['/workspace/c.md']);
    // An object followed by '&' and more text is a param-like continuation,
    // never linked; '&' followed by a space is not.
    expect(
      Array.from(content.querySelectorAll('.gcs-link')).map((a) => a.getAttribute('data-gcs-uri'))
    ).toEqual(['gs://bkt/p']);
  });

  it('links a commit or session id after a no-break space', async () => {
    const content = await mount(`commit\u00a0abc1234 and session\u00a0${UUID}`);

    expect(content.querySelector('a[href="/commits/abc1234"]')?.textContent).toBe('abc1234');
    expect(content.querySelector(`a[href="/sessions/${UUID}"]`)?.textContent).toBe(UUID);
  });
});
