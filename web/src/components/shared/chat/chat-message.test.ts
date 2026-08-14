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
 * Tests for clickable @mentions in <scion-chat-message>.
 *
 * Rendered mentions carry the slug in `data-mention` and report a click as a
 * composed `mention-click` event — the message cannot resolve a slug itself,
 * only the chat page knows the member roster.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { vi } from 'vitest';

// A trivial stand-in for marked + DOMPurify: paragraph-wraps the body so the
// mention post-processing has tags to work between.
vi.mock('../../../utils/markdown.js', () => ({
  getMarkdownRenderer: () =>
    Promise.resolve({
      render: (markdown: string) => `<p>${markdown}</p>`,
    }),
}));

await import('./chat-message.js');
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;

/** Mount a message and wait for the async markdown render to land. */
async function mount(body: string): Promise<ScionChatMessage> {
  const el = document.createElement('scion-chat-message') as ScionChatMessage;
  el.body = body;
  document.body.appendChild(el);
  await el.updateComplete;
  // renderContent() resolves the renderer promise, then re-renders.
  await Promise.resolve();
  await el.updateComplete;
  return el;
}

function mentions(el: ScionChatMessage): HTMLElement[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.md-content .mention') ?? []);
}

describe('scion-chat-message @mentions', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders mentions as clickable spans carrying the slug', async () => {
    const el = await mount('ping @native-chat-lead about this');
    const spans = mentions(el);

    expect(spans).toHaveLength(1);
    expect(spans[0].classList.contains('clickable')).toBe(true);
    expect(spans[0].getAttribute('data-mention')).toBe('native-chat-lead');
    expect(spans[0].textContent).toBe('@native-chat-lead');
  });

  it('emits a composed mention-click with the slug when a mention is clicked', async () => {
    const el = await mount('hello @coder');
    const seen: string[] = [];
    document.addEventListener('mention-click', (e) => {
      seen.push((e as CustomEvent<{ slug: string }>).detail.slug);
    });

    mentions(el)[0].dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(seen).toEqual(['coder']);
  });

  it('stays silent when the click misses a mention', async () => {
    const el = await mount('no mentions here');
    const listener = vi.fn();
    document.addEventListener('mention-click', listener);

    const content = el.shadowRoot?.querySelector('.md-content') as HTMLElement;
    content.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(listener).not.toHaveBeenCalled();
  });
});
