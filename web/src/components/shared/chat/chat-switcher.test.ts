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
 * Tests for <scion-chat-switcher> — the Cmd/Ctrl-K quick conversation switcher.
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';

await import('./chat-switcher.js');
type ScionChatSwitcher = import('./chat-switcher.js').ScionChatSwitcher;
type SwitcherConversation = import('./chat-switcher.js').SwitcherConversation;

const CONVERSATIONS: SwitcherConversation[] = [
  {
    conversationKey: 'topic-1',
    name: '#general',
    spaceName: 'Project Alpha',
    isDM: false,
    lastActivityAt: '2026-08-17T10:00:00Z',
  },
  {
    conversationKey: 'topic-2',
    name: '#dev',
    spaceName: 'Project Alpha',
    isDM: false,
    lastActivityAt: '2026-08-17T09:00:00Z',
  },
  {
    conversationKey: 'dm:agent:abc:user:xyz',
    name: 'Bot Helper',
    spaceName: 'Agent DM',
    isDM: true,
    lastActivityAt: '2026-08-17T08:00:00Z',
  },
];

async function mount(convs?: SwitcherConversation[]): Promise<ScionChatSwitcher> {
  const el = document.createElement('scion-chat-switcher') as ScionChatSwitcher;
  el.conversations = convs ?? CONVERSATIONS;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-chat-switcher', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders a search input and conversation items', async () => {
    const el = await mount();

    const input = el.shadowRoot?.querySelector('input');
    expect(input).not.toBeNull();

    const items = el.shadowRoot?.querySelectorAll('.item');
    expect(items?.length).toBe(3);
  });

  it('filters conversations by search term', async () => {
    const el = await mount();

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'general';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const items = el.shadowRoot?.querySelectorAll('.item');
    expect(items?.length).toBe(1);
    expect(items?.[0].querySelector('.item-name')?.textContent).toContain('#general');
  });

  it('filters by space name', async () => {
    const el = await mount();

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'Agent DM';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const items = el.shadowRoot?.querySelectorAll('.item');
    expect(items?.length).toBe(1);
    expect(items?.[0].querySelector('.item-name')?.textContent).toContain('Bot Helper');
  });

  it('shows empty state when no matches', async () => {
    const el = await mount();

    const input = el.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'nonexistent';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const empty = el.shadowRoot?.querySelector('.empty');
    expect(empty).not.toBeNull();
    expect(empty?.textContent).toContain('No matching conversations');
  });

  it('emits switcher-select on Enter', async () => {
    const el = await mount();
    let selectedKey = '';
    el.addEventListener('switcher-select', (e) => {
      selectedKey = (e as CustomEvent).detail.conversationKey;
    });

    // Press Enter to select the first item (sorted by most recent).
    el.shadowRoot?.querySelector('.overlay')?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })
    );

    expect(selectedKey).toBe('topic-1');
  });

  it('emits switcher-close on Escape', async () => {
    const el = await mount();
    let closed = false;
    el.addEventListener('switcher-close', () => {
      closed = true;
    });

    el.shadowRoot?.querySelector('.overlay')?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })
    );

    expect(closed).toBe(true);
  });

  it('navigates selection with arrow keys', async () => {
    const el = await mount();

    const overlay = el.shadowRoot?.querySelector('.overlay')!;

    // Initially first item is selected.
    let selected = el.shadowRoot?.querySelector('.item.selected .item-name');
    expect(selected?.textContent).toContain('#general');

    // Arrow down to second item.
    overlay.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;

    selected = el.shadowRoot?.querySelector('.item.selected .item-name');
    expect(selected?.textContent).toContain('#dev');

    // Arrow down to third item (DM).
    overlay.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;

    selected = el.shadowRoot?.querySelector('.item.selected .item-name');
    expect(selected?.textContent).toContain('Bot Helper');

    // Arrow up back to second.
    overlay.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    await el.updateComplete;

    selected = el.shadowRoot?.querySelector('.item.selected .item-name');
    expect(selected?.textContent).toContain('#dev');
  });

  it('shows DM badge for DM conversations', async () => {
    const el = await mount();

    const items = el.shadowRoot?.querySelectorAll('.item');
    const dmItem = items?.[2]; // Sorted by activity — DM is last.
    expect(dmItem?.querySelector('.dm-badge')).not.toBeNull();

    const threadItem = items?.[0]; // Thread — no DM badge.
    expect(threadItem?.querySelector('.dm-badge')).toBeNull();
  });

  it('sorts by most recent activity first', async () => {
    const el = await mount();

    const names = Array.from(el.shadowRoot?.querySelectorAll('.item-name') ?? []).map(
      (n) => n.textContent?.trim() ?? ''
    );
    // topic-1 (10:00) > topic-2 (09:00) > dm (08:00)
    expect(names[0]).toContain('#general');
    expect(names[1]).toContain('#dev');
    expect(names[2]).toContain('Bot Helper');
  });

  it('emits switcher-close when clicking the overlay backdrop', async () => {
    const el = await mount();
    let closed = false;
    el.addEventListener('switcher-close', () => {
      closed = true;
    });

    // Click the overlay itself (not the panel).
    const overlay = el.shadowRoot?.querySelector('.overlay') as HTMLElement;
    overlay.click();

    expect(closed).toBe(true);
  });
});
