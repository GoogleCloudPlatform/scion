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
 * Tests for expanding a space in the rail and for the new-thread name entry.
 *
 * On desktop the rail sits beside the conversation, so clicking a collapsed
 * space can open #general as a shortcut. On mobile the rail is a screen of
 * its own and selecting a thread slides it away — which would hide the thread
 * list the click was asking to see — so there the click only expands the
 * space.
 *
 * The expand tests create the element without appending it, so
 * connectedCallback never runs. The name-entry tests do append it, because
 * focus needs a rendered input; they stub out the initial data load first so
 * no fetched state can overwrite the fixture.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { apiFetch } from '../../../client/api.js';
import { setPreferredTimeZone } from '../../../utils/time.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

const SPACE = {
  projectId: 'proj-1',
  projectName: 'Chat Test',
  projectSlug: 'chat-test',
  unreadCount: 0,
  hasUnreadMention: false,
};

const GENERAL = {
  id: 'topic-general',
  name: 'general',
  isGeneral: true,
  pinned: false,
  hasUnread: false,
  hasUnreadMention: false,
};

/** A rail holding one collapsed space with a #general thread in it. */
function createRail(): any {
  const el = document.createElement('scion-chat-space-rail') as any;
  el.spaces = [SPACE];
  el.threadsBySpace = new Map([[SPACE.projectId, [GENERAL]]]);
  el.collapsedSpaces = new Set([SPACE.projectId]);
  return el;
}

/** Click the collapsed space and report the thread-select it did or did not fire. */
function clickCollapsedSpace(el: any): CustomEvent | null {
  let selected: CustomEvent | null = null;
  el.addEventListener('thread-select', (e: Event) => {
    selected = e as CustomEvent;
  });
  el.handleCollapsedSpaceClick(SPACE);
  return selected;
}

beforeAll(async () => {
  await import('./chat-space-rail.js');
});

afterEach(() => {
  (window as any).innerWidth = 1024;
  vi.restoreAllMocks();
});

describe('space rail — expanding a space', () => {
  it('expands the space and opens #general on desktop', () => {
    (window as any).innerWidth = 1400;
    const el = createRail();

    const selected = clickCollapsedSpace(el);

    expect(el.collapsedSpaces.has(SPACE.projectId)).toBe(false);
    expect(selected?.detail).toMatchObject({
      conversationKey: 'topic-general',
      projectId: 'proj-1',
      projectSlug: 'chat-test',
      threadName: 'general',
    });
  });

  it('only expands the space on mobile, leaving the thread list on screen', () => {
    (window as any).innerWidth = 400;
    const el = createRail();

    const selected = clickCollapsedSpace(el);

    expect(el.collapsedSpaces.has(SPACE.projectId)).toBe(false);
    expect(selected).toBeNull();
  });

  it('expands on request without selecting a thread', () => {
    const el = createRail();
    let selected: CustomEvent | null = null;
    el.addEventListener('thread-select', (e: Event) => {
      selected = e as CustomEvent;
    });

    el.expandSpace(SPACE.projectId);

    expect(el.collapsedSpaces.has(SPACE.projectId)).toBe(false);
    expect(selected).toBeNull();
  });

  it('expands a space that has no #general to open', () => {
    (window as any).innerWidth = 1400;
    const el = createRail();
    el.threadsBySpace = new Map();

    const selected = clickCollapsedSpace(el);

    expect(el.collapsedSpaces.has(SPACE.projectId)).toBe(false);
    expect(selected).toBeNull();
  });
});

describe('space rail — new thread name entry', () => {
  /** Mount an expanded rail whose space holds #general, a pinned thread and a plain one. */
  async function mount(): Promise<any> {
    const el = document.createElement('scion-chat-space-rail') as any;
    // connectedCallback starts loadData, which would later replace the
    // fixture below with whatever the mocked API returns.
    el.loadData = () => Promise.resolve();
    document.body.appendChild(el);
    el.spaces = [SPACE];
    el.threadsBySpace = new Map([
      [
        SPACE.projectId,
        [
          GENERAL,
          { ...GENERAL, id: 'topic-pinned', name: 'pinned-one', isGeneral: false, pinned: true },
          { ...GENERAL, id: 'topic-plain', name: 'plain-one', isGeneral: false },
        ],
      ],
    ]);
    el.collapsedSpaces = new Set<string>();
    el.loading = false;
    await el.updateComplete;
    return el;
  }

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders the entry row above every thread in the space', async () => {
    const el = await mount();

    el.startCreateThread(SPACE.projectId);
    await el.updateComplete;

    const list = el.shadowRoot.querySelector('.thread-list') as HTMLElement;
    const row = list.querySelector('.create-thread') as HTMLElement;
    const firstThread = list.querySelector('.thread-item') as HTMLElement;
    expect(row).not.toBeNull();
    expect(firstThread).not.toBeNull();
    expect(
      row.compareDocumentPosition(firstThread) & Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
    expect(list.firstElementChild).toBe(row);
  });

  it('expands a collapsed space so the entry row is visible, and focuses it', async () => {
    const el = await mount();
    el.collapsedSpaces = new Set([SPACE.projectId]);
    await el.updateComplete;

    el.startCreateThread(SPACE.projectId);
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 0));

    const input = el.shadowRoot.querySelector('.thread-list .create-thread sl-input');
    expect(input).not.toBeNull();
    // activeElement retargets to the sl-input host even when focus lands on
    // the native input inside its shadow root.
    expect(el.shadowRoot.activeElement).toBe(input);
  });

  it('refocuses the open row, keeping its text, when New thread is asked for again', async () => {
    const el = await mount();
    el.startCreateThread(SPACE.projectId);
    await el.updateComplete;
    el.newThreadName = 'half-typed';
    await el.updateComplete;
    // Wait for the first request's focus to land, then move focus away, as
    // the menu that issues a repeat New thread would.
    await new Promise((resolve) => setTimeout(resolve, 0));
    const input = el.shadowRoot.querySelector('.thread-list .create-thread sl-input');
    expect(input).not.toBeNull();
    input.blur();
    expect(el.shadowRoot.activeElement).not.toBe(input);

    el.startCreateThread(SPACE.projectId);
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(el.shadowRoot.querySelector('.thread-list .create-thread sl-input')).toBe(input);
    expect(el.shadowRoot.activeElement).toBe(input);
    expect(el.newThreadName).toBe('half-typed');
  });

  describe('filing the new thread into a group', () => {
    const GROUP = { id: 'group-1', name: 'Group One', threadIds: [] as string[] };

    /** Mount the rail with one group, and answer thread creation with a new thread. */
    async function mountWithGroup(): Promise<any> {
      const el = await mount();
      el.prefs = { ...el.prefs, threadGroups: { [SPACE.projectId]: [GROUP] } };
      el.savePrefs = () => Promise.resolve();
      vi.mocked(apiFetch).mockResolvedValueOnce(
        new Response(JSON.stringify({ id: 'topic-new', name: 'new-one' }), { status: 200 })
      );
      await el.updateComplete;
      return el;
    }

    /** Choose New thread from the group's context menu. */
    async function newThreadFromGroupMenu(el: any): Promise<void> {
      el.groupContextMenuTarget = { group: GROUP, projectId: SPACE.projectId };
      await el.updateComplete;
      const item = el.shadowRoot.querySelector('.context-menu .context-menu-item') as HTMLElement;
      expect(item.textContent).toContain('New thread');
      item.click();
      await el.updateComplete;
    }

    /** Choose New thread from the space's actions menu. */
    async function newThreadFromSpaceMenu(el: any): Promise<void> {
      const item = el.shadowRoot.querySelector('sl-menu-item[value="new-thread"]') as HTMLElement;
      item
        .closest('sl-menu')!
        .dispatchEvent(
          new CustomEvent('sl-select', { detail: { item }, bubbles: true, composed: true })
        );
      await el.updateComplete;
    }

    function pressKey(el: any, key: string): void {
      const input = el.shadowRoot.querySelector('.create-thread sl-input') as HTMLElement;
      input.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, composed: true }));
    }

    /** Type a name into the open row and press Enter, then let the creation settle. */
    async function submitName(el: any, name: string): Promise<void> {
      el.newThreadName = name;
      await el.updateComplete;
      pressKey(el, 'Enter');
      await new Promise((resolve) => setTimeout(resolve, 0));
      await el.updateComplete;
    }

    it('files a thread started from the group menu into that group', async () => {
      const el = await mountWithGroup();
      const move = vi.spyOn(el, 'moveThreadToGroup');

      await newThreadFromGroupMenu(el);
      await submitName(el, 'new-one');

      expect(move).toHaveBeenCalledWith('topic-new', GROUP.id, SPACE.projectId);
    });

    it('does not file into a group whose entry was dismissed before the space menu was used', async () => {
      const el = await mountWithGroup();
      const move = vi.spyOn(el, 'moveThreadToGroup');

      await newThreadFromGroupMenu(el);
      pressKey(el, 'Escape');
      await el.updateComplete;
      expect(el.shadowRoot.querySelector('.create-thread')).toBeNull();
      expect(el._createThreadGroupId).toBeNull();
      await newThreadFromSpaceMenu(el);
      await submitName(el, 'new-one');

      expect(vi.mocked(apiFetch)).toHaveBeenCalledWith(
        `/api/v1/chat/spaces/${SPACE.projectId}/threads`,
        expect.objectContaining({ method: 'POST' })
      );
      expect(move).not.toHaveBeenCalled();
    });

    it('drops the group target when New thread comes from the space menu while the row is open', async () => {
      const el = await mountWithGroup();
      const move = vi.spyOn(el, 'moveThreadToGroup');

      await newThreadFromGroupMenu(el);
      el.newThreadName = 'half-typed';
      await el.updateComplete;
      await newThreadFromSpaceMenu(el);
      expect(el.newThreadName).toBe('half-typed');
      await submitName(el, 'new-one');

      expect(move).not.toHaveBeenCalled();
    });

    it('drops the group target when an empty entry is dismissed by blur', async () => {
      const el = await mountWithGroup();

      await newThreadFromGroupMenu(el);
      const input = el.shadowRoot.querySelector('.create-thread sl-input') as HTMLElement;
      input.dispatchEvent(new Event('sl-blur'));
      await el.updateComplete;

      expect(el.shadowRoot.querySelector('.create-thread')).toBeNull();
      expect(el._createThreadGroupId).toBeNull();
    });
  });
});

describe('thread markdown export in the display zone (tz-refactor task 21)', () => {
  afterEach(() => {
    setPreferredTimeZone('');
    vi.useRealTimers();
  });

  it('formats the export and message times 24-hour in the display zone, naming the zone', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-09-23T15:00:00Z'));
    // vitest pins the browser zone to UTC; 15:00Z is midnight in Tokyo.
    setPreferredTimeZone('Asia/Tokyo');
    const el = document.createElement('scion-chat-space-rail') as any;
    const md: string = el.formatThreadAsMarkdown({ name: 'general' }, [
      { sender: 'user:alice', msg: 'hi', createdAt: '2026-09-23T15:05:00Z' },
    ]);
    expect(md).toContain('Exported: Sep 24, 2026, 00:00 (Asia/Tokyo)');
    expect(md).toContain('**alice** (Sep 24, 2026, 00:05 (Asia/Tokyo)):');
  });
});
