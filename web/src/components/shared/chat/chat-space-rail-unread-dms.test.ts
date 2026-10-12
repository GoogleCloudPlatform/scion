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
 * The rail's Unread DMs section: under the Unread filter it lists every DM
 * the unread badge counts — including DMs with agents in other spaces, but
 * not DMs with deleted agents, which the badge leaves out too — so the
 * badge's number can always be found in the rail, and clicking a row asks
 * the page to open that DM.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import type { ChatDMListEntry } from '../../../client/chat-unread-dms.js';
import type { ChatSpace, DMSelectDetail } from './chat-space-rail.js';

/* eslint-disable @typescript-eslint/no-explicit-any -- `el` is the rail
   custom element accessed through its private fields, same as the sibling
   chat-space-rail-*.test.ts files. */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const apiFetchMock = vi.mocked(apiFetch);

const SPACE: ChatSpace = {
  projectId: 'proj-1',
  projectName: 'Chat Test',
  projectSlug: 'chat-test',
  unreadCount: 2,
  hasUnreadMention: false,
};

function dm(overrides: Partial<ChatDMListEntry>): ChatDMListEntry {
  return {
    conversationKey: 'dm:k',
    peerId: 'peer',
    peerKind: 'agent',
    peerName: 'peer',
    hasUnread: true,
    muted: false,
    ...overrides,
  };
}

const DMS: ChatDMListEntry[] = [
  dm({ conversationKey: 'dm:u', peerId: 'u1', peerKind: 'user', peerName: 'Alice' }),
  dm({ conversationKey: 'dm:other', peerId: 'a1', peerName: 'other-space-agent' }),
  // An agent the hub cannot name, not flagged deleted: listed as unknown.
  dm({ conversationKey: 'dm:gone', peerId: 'a2', peerName: '', peerSlug: '' }),
  dm({ conversationKey: 'dm:muted', peerId: 'a3', muted: true }),
  dm({ conversationKey: 'dm:read', peerId: 'a4', hasUnread: false }),
  // A deleted agent: kept by the hub, flagged, and not listed as unread.
  dm({ conversationKey: 'dm:deleted', peerId: 'a5', peerName: 'old-agent', peerDeleted: true }),
];

async function mount(spaces: ChatSpace[], dms: ChatDMListEntry[]): Promise<any> {
  const el = document.createElement('scion-chat-space-rail') as any;
  document.body.appendChild(el);
  await new Promise((resolve) => setTimeout(resolve, 0));
  el.spaces = spaces;
  el.threadsBySpace = new Map();
  el.collapsedSpaces = new Set<string>();
  el.loading = false;
  el.dms = dms;
  await el.updateComplete;
  return el;
}

interface CssBlock {
  prelude: string;
  body: string;
}

/** Split CSS text into its blocks at one nesting level: `prelude { body }`. */
function cssBlocks(css: string): CssBlock[] {
  const blocks: CssBlock[] = [];
  const text = css.replace(/\/\*[\s\S]*?\*\//g, '');
  let start = 0;
  let i = 0;
  while (i < text.length) {
    if (text[i] !== '{') {
      i++;
      continue;
    }
    let depth = 1;
    let j = i + 1;
    while (j < text.length && depth > 0) {
      if (text[j] === '{') depth++;
      else if (text[j] === '}') depth--;
      j++;
    }
    blocks.push({
      prelude: text.slice(start, i).replace(/\s+/g, ' ').trim(),
      body: text.slice(i + 1, j - 1),
    });
    start = i = j;
  }
  return blocks;
}

/**
 * The declarations of every rule among `blocks` whose selector list
 * includes `selector`, merged in source order (later ones win).
 */
function declarationsFor(blocks: CssBlock[], selector: string): Map<string, string> {
  const decls = new Map<string, string>();
  for (const block of blocks) {
    if (!block.prelude.split(',').some((sel) => sel.trim() === selector)) continue;
    for (const decl of block.body.split(';')) {
      const colon = decl.indexOf(':');
      if (colon < 0) continue;
      decls.set(
        decl.slice(0, colon).trim(),
        decl
          .slice(colon + 1)
          .replace(/\s+/g, ' ')
          .trim()
      );
    }
  }
  return decls;
}

function dmRows(el: any): HTMLElement[] {
  return Array.from(el.shadowRoot.querySelectorAll('.dm-item'));
}

function dmNames(el: any): string[] {
  return dmRows(el).map((r) => r.querySelector('.thread-name')?.textContent?.trim() ?? '');
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

beforeEach(() => {
  apiFetchMock.mockResolvedValue(new Response('{}', { status: 200 }));
});

afterEach(() => {
  vi.clearAllMocks();
  document.body.innerHTML = '';
});

describe('chat-space-rail Unread DMs section', () => {
  it('is hidden under the All filter', async () => {
    const el = await mount([SPACE], DMS);
    expect(dmRows(el)).toHaveLength(0);
  });

  it('lists every DM the badge counts under the Unread filter', async () => {
    const el = await mount([SPACE], DMS);
    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(dmNames(el)).toEqual(['Alice', 'other-space-agent', 'Unknown agent']);
    const badge = el.shadowRoot.querySelector('.dm-header .unread-badge');
    expect(badge?.textContent?.trim()).toBe('3');
    // The badge total is the space badges plus the DM count.
    const spaceBadges = Array.from(
      el.shadowRoot.querySelectorAll('.space-section:not(.dm-section) .unread-badge')
    ).reduce((n: number, b) => n + Number((b as HTMLElement).textContent), 0);
    expect(spaceBadges).toBe(SPACE.unreadCount);
  });

  it('holds a hidden copy of the menu button after the DM heading badge', async () => {
    const el = await mount([SPACE], DMS);
    el.spaceFilter = 'unread';
    await el.updateComplete;
    const dmActions = el.shadowRoot.querySelector('.dm-header .space-actions') as HTMLElement;
    // The badge comes first, then a spacer standing in for the menu button.
    const spacer = dmActions.lastElementChild as HTMLElement;
    expect(spacer.classList.contains('menu-spacer')).toBe(true);
    expect(spacer.previousElementSibling?.classList.contains('unread-badge')).toBe(true);
    // The spacer is decoration only: hidden from assistive tech, inert, and
    // its copy of the button is out of the tab order.
    expect(spacer.getAttribute('aria-hidden')).toBe('true');
    expect(spacer.hasAttribute('inert')).toBe(true);
    expect(spacer.textContent?.trim()).toBe('');

    // Its width comes from a copy of the real menu button: the same element
    // and icon, inside .space-actions, so the same rules size it.
    const spaceActions = el.shadowRoot.querySelector(
      '.space-section:not(.dm-section) .space-actions'
    ) as HTMLElement;
    const menuButton = spaceActions.querySelector('sl-dropdown sl-icon-button') as HTMLElement;
    const copy = spacer.querySelector('sl-icon-button') as HTMLElement;
    expect(spacer.childElementCount).toBe(1);
    expect(copy.getAttribute('name')).toBe(menuButton.getAttribute('name'));
    expect(copy.getAttribute('tabindex')).toBe('-1');
    expect(copy.hasAttribute('label')).toBe(false);

    // A space heading is unchanged: badge, then the menu, and no spacer.
    expect(spaceActions.querySelector('.menu-spacer')).toBeNull();
    expect(Array.from(spaceActions.children).map((c) => c.localName)).toEqual([
      'span',
      'sl-dropdown',
    ]);
    expect(spaceActions.firstElementChild?.classList.contains('unread-badge')).toBe(true);
  });

  it('keeps the space menu button sized as before the DM spacer existed', () => {
    // The menu button keeps its own size: its natural width on desktop and
    // the 44px touch target on mobile. The spacer follows it, not the
    // other way round.
    const Rail = customElements.get('scion-chat-space-rail') as unknown as {
      styles: { cssText: string } | { cssText: string }[];
    };
    const css = [Rail.styles]
      .flat()
      .map((s) => s.cssText)
      .join('\n');
    expect(css).not.toContain('--space-menu-width');
    const topLevel = cssBlocks(css);
    const iconButton = declarationsFor(topLevel, '.space-actions sl-icon-button::part(base)');
    expect(Object.fromEntries(iconButton)).toEqual({
      'font-size': 'var(--chat-fs-base)',
      padding: '0.125rem',
    });
    const mobile = topLevel
      .filter((b) => /^@media\s*\(\s*max-width\s*:\s*768px\s*\)$/.test(b.prelude))
      .flatMap((b) => cssBlocks(b.body));
    const mobileButton = declarationsFor(mobile, '.space-actions sl-icon-button::part(base)');
    expect(Object.fromEntries(mobileButton)).toEqual({ width: '44px', height: '44px' });

    // The spacer takes no height and is never painted.
    const spacerRule = declarationsFor(topLevel, '.space-actions .menu-spacer');
    expect(spacerRule.get('height')).toBe('0');
    expect(spacerRule.get('visibility')).toBe('hidden');
    expect(spacerRule.get('overflow')).toBe('hidden');
  });

  it('keeps the spacer when the DM heading has no badge', async () => {
    const el = await mount([SPACE], [DMS[4]]);
    el.selectedKey = 'dm:read';
    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(dmRows(el).map((r) => r.dataset.dmKey)).toEqual(['dm:read']);
    const dmActions = el.shadowRoot.querySelector('.dm-header .space-actions') as HTMLElement;
    expect(dmActions.querySelector('.unread-badge')).toBeNull();
    const spacer = dmActions.querySelector('.menu-spacer');
    expect(spacer?.getAttribute('aria-hidden')).toBe('true');
    expect(spacer?.querySelector('sl-icon-button')).not.toBeNull();
  });

  it('shows unread DMs when no space has unread', async () => {
    const el = await mount([{ ...SPACE, unreadCount: 0 }], DMS);
    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(dmRows(el)).toHaveLength(3);
    expect(el.shadowRoot.textContent).not.toContain('All caught up!');
  });

  it('is all caught up with no unread DMs or spaces', async () => {
    const el = await mount([{ ...SPACE, unreadCount: 0 }], [DMS[3], DMS[4]]);
    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(dmRows(el)).toHaveLength(0);
    expect(el.shadowRoot.textContent).toContain('All caught up!');
  });

  it('keeps the open DM once read, without counting it', async () => {
    const el = await mount([SPACE], [DMS[0], DMS[4]]);
    el.selectedKey = 'dm:read';
    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(dmRows(el).map((r) => r.dataset.dmKey)).toEqual(['dm:u', 'dm:read']);
    expect(dmRows(el)[1].classList.contains('selected')).toBe(true);
    expect(el.shadowRoot.querySelector('.dm-header .unread-badge')?.textContent?.trim()).toBe('1');
  });

  it('fires dm-select with the peer when a row is clicked', async () => {
    const el = await mount([SPACE], DMS);
    el.spaceFilter = 'unread';
    await el.updateComplete;
    const seen: DMSelectDetail[] = [];
    el.addEventListener('dm-select', (e: Event) =>
      seen.push((e as CustomEvent<DMSelectDetail>).detail)
    );
    dmRows(el)[2].click();
    expect(seen).toEqual([
      { conversationKey: 'dm:gone', peerId: 'a2', peerKind: 'agent', displayName: 'Unknown agent' },
    ]);
  });

  it('makes rows keyboard-reachable buttons with an accessible name', async () => {
    const el = await mount([SPACE], DMS);
    el.spaceFilter = 'unread';
    await el.updateComplete;
    const rows = dmRows(el);
    expect(rows.map((r) => [r.getAttribute('role'), r.getAttribute('tabindex')])).toEqual([
      ['button', '0'],
      ['button', '0'],
      ['button', '0'],
    ]);
    expect(rows.map((r) => r.getAttribute('aria-label'))).toEqual([
      'Direct message with user Alice, unread',
      'Direct message with agent other-space-agent, unread',
      'Direct message with agent Unknown agent, unread',
    ]);
    const seen: string[] = [];
    el.addEventListener('dm-select', (e: Event) =>
      seen.push((e as CustomEvent<DMSelectDetail>).detail.conversationKey)
    );
    rows[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    rows[1].dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true }));
    rows[2].dispatchEvent(new KeyboardEvent('keydown', { key: 'a', bubbles: true }));
    expect(seen).toEqual(['dm:u', 'dm:other']);
  });

  it('leaves out DMs with deleted agents', async () => {
    const el = await mount([SPACE], DMS);
    el.spaceFilter = 'unread';
    await el.updateComplete;
    expect(dmRows(el).map((r) => r.dataset.dmKey)).not.toContain('dm:deleted');
  });
});

describe('chat-space-rail spaces without unread tracking', () => {
  const thread = {
    id: 'topic-x',
    name: 'thread-x',
    isGeneral: false,
    pinned: false,
    muted: false,
    hasUnread: false,
    hasUnreadMention: false,
    lastMessageId: 'm1',
  };

  async function mountUntracked(tracked: boolean): Promise<any> {
    const el = document.createElement('scion-chat-space-rail') as any;
    document.body.appendChild(el);
    await new Promise((resolve) => setTimeout(resolve, 0));
    el.spaces = [{ ...SPACE, unreadCount: 0, unreadTracked: tracked }];
    el.threadsBySpace = new Map([[SPACE.projectId, [thread]]]);
    el.collapsedSpaces = new Set<string>();
    el.loading = false;
    await el.updateComplete;
    return el;
  }

  it('does not mark a thread unread locally, nor offer Mark unread', async () => {
    const el = await mountUntracked(false);
    el.markThreadUnread('topic-x');
    await el.updateComplete;
    expect(el.threadsBySpace.get(SPACE.projectId)[0].hasUnread).toBe(false);
    expect(el.spaces[0].unreadCount).toBe(0);
    const ids = el.threadMenuActions(thread, SPACE.projectId).map((a: any) => a.id);
    expect(ids).not.toContain('mark-unread');
  });

  it('does not restore an unread dot from a pending change whose rollup already has it', async () => {
    for (const tracked of [false, true]) {
      const el = await mountUntracked(tracked);
      try {
        // The list predates the change, the spaces rollup does not.
        el._spacesStartedAt = 100;
        el._pendingReadState.set('topic-x', { kind: 'unread', at: 50 });
        el.applyPendingReadState(SPACE.projectId, [thread], 10);
        await el.updateComplete;
        expect(el.threadsBySpace.get(SPACE.projectId)[0].hasUnread).toBe(tracked);
        expect(el.spaces[0].unreadCount).toBe(0);
      } finally {
        // Unmount before the next iteration, even when an assertion fails.
        document.body.innerHTML = '';
      }
    }
  });

  it('still does both in a tracked space', async () => {
    const el = await mountUntracked(true);
    const ids = el.threadMenuActions(thread, SPACE.projectId).map((a: any) => a.id);
    expect(ids).toContain('mark-unread');
    el.markThreadUnread('topic-x');
    await el.updateComplete;
    expect(el.threadsBySpace.get(SPACE.projectId)[0].hasUnread).toBe(true);
    expect(el.spaces[0].unreadCount).toBe(1);
  });
});
