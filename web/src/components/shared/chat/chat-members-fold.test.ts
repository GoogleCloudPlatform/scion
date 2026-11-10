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
 * Tests for folding the People and Agents sections of <scion-chat-members>.
 *
 * A folded section keeps only its heading (with the count), remembers the
 * fold per user in localStorage, and shows an unread dot while folded so new
 * messages are never hidden.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

vi.mock('../../../client/main.js', () => import('../../../client/__fixtures__/main-stub.js'));

import './chat-members.js';
import type { ScionChatMembers, ChatAgentMember, ChatHumanMember } from './chat-members.js';

const STORAGE_KEY = 'scion-chat-members-collapse:me';

function human(id: string, displayName: string): ChatHumanMember {
  return { id, kind: 'user', displayName };
}

function agent(id: string, displayName: string): ChatAgentMember {
  return { id, kind: 'agent', displayName, slug: displayName.toLowerCase(), phase: 'running' };
}

async function mount(opts: { unreadFromIds?: string[] } = {}): Promise<ScionChatMembers> {
  const el = document.createElement('scion-chat-members') as ScionChatMembers;
  el.currentUserId = 'me';
  el.humans = [human('u1', 'Ada'), human('u2', 'Grace')];
  el.agents = [agent('a1', 'Coder'), agent('a2', 'Reviewer')];
  el.unreadFromIds = opts.unreadFromIds ?? [];
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function toggle(el: ScionChatMembers, section: 'people' | 'agents'): HTMLButtonElement {
  const b = el.shadowRoot?.querySelector<HTMLButtonElement>(
    `.section-toggle[data-section="${section}"]`
  );
  if (!b) throw new Error(`no ${section} toggle`);
  return b;
}

function names(el: ScionChatMembers): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.member-name') ?? []).map(
    (n) => n.textContent?.trim() ?? ''
  );
}

describe('scion-chat-members section folding', () => {
  beforeEach(() => {
    localStorage.clear();
  });
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
    localStorage.clear();
  });

  it('shows both sections expanded by default', async () => {
    const el = await mount();
    expect(toggle(el, 'people').getAttribute('aria-expanded')).toBe('true');
    expect(toggle(el, 'agents').getAttribute('aria-expanded')).toBe('true');
    expect(names(el)).toEqual(['Ada', 'Grace', 'Coder', 'Reviewer']);
  });

  it('folds People down to its heading and keeps Agents visible', async () => {
    const el = await mount();
    toggle(el, 'people').click();
    await el.updateComplete;
    expect(toggle(el, 'people').getAttribute('aria-expanded')).toBe('false');
    expect(toggle(el, 'people').textContent).toContain('People — 2');
    expect(names(el)).toEqual(['Coder', 'Reviewer']);
  });

  it('folds Agents independently and unfolds on a second click', async () => {
    const el = await mount();
    toggle(el, 'agents').click();
    await el.updateComplete;
    expect(names(el)).toEqual(['Ada', 'Grace']);
    expect(toggle(el, 'agents').textContent).toContain('Agents — 2');
    toggle(el, 'agents').click();
    await el.updateComplete;
    expect(names(el)).toEqual(['Ada', 'Grace', 'Coder', 'Reviewer']);
  });

  it('remembers the fold per user across remounts', async () => {
    const first = await mount();
    toggle(first, 'people').click();
    await first.updateComplete;
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]')).toEqual(['people']);
    document.body.innerHTML = '';

    const second = await mount();
    expect(toggle(second, 'people').getAttribute('aria-expanded')).toBe('false');
    expect(names(second)).toEqual(['Coder', 'Reviewer']);
  });

  for (const section of ['people', 'agents'] as const) {
    it(`keeps the ${section} heading button and its focus across fold and unfold`, async () => {
      const el = await mount();
      const button = toggle(el, section);
      button.focus();
      button.click();
      await el.updateComplete;
      expect(toggle(el, section)).toBe(button);
      expect(button.getAttribute('aria-expanded')).toBe('false');
      expect(el.shadowRoot?.activeElement).toBe(button);
      button.click();
      await el.updateComplete;
      expect(toggle(el, section)).toBe(button);
      expect(button.getAttribute('aria-expanded')).toBe('true');
      expect(el.shadowRoot?.activeElement).toBe(button);
    });
  }

  it('restores the fold from the new user when currentUserId changes', async () => {
    localStorage.setItem('scion-chat-members-collapse:other', JSON.stringify(['agents']));
    const el = await mount();
    expect(names(el)).toEqual(['Ada', 'Grace', 'Coder', 'Reviewer']);

    el.currentUserId = 'other';
    await el.updateComplete;
    expect(toggle(el, 'agents').getAttribute('aria-expanded')).toBe('false');
    expect(toggle(el, 'people').getAttribute('aria-expanded')).toBe('true');

    toggle(el, 'people').click();
    await el.updateComplete;
    expect(JSON.parse(localStorage.getItem('scion-chat-members-collapse:other') ?? '[]')).toEqual([
      'agents',
      'people',
    ]);
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();

    el.currentUserId = 'me';
    await el.updateComplete;
    expect(names(el)).toEqual(['Ada', 'Grace', 'Coder', 'Reviewer']);
  });

  it('ignores corrupt stored state', async () => {
    localStorage.setItem(STORAGE_KEY, '{not json');
    const el = await mount();
    expect(names(el)).toEqual(['Ada', 'Grace', 'Coder', 'Reviewer']);
  });

  it('ignores a stored value that is not an array', async () => {
    localStorage.setItem(STORAGE_KEY, '{"people":true}');
    const el = await mount();
    expect(names(el)).toEqual(['Ada', 'Grace', 'Coder', 'Reviewer']);
  });

  it('keeps only known section names from stored state', async () => {
    localStorage.setItem(STORAGE_KEY, '["agents","bogus"]');
    const el = await mount();
    expect(toggle(el, 'agents').getAttribute('aria-expanded')).toBe('false');
    expect(toggle(el, 'people').getAttribute('aria-expanded')).toBe('true');
    expect(names(el)).toEqual(['Ada', 'Grace']);
  });

  it('starts unfolded when reading storage throws', async () => {
    const getItem = vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new Error('storage unavailable');
    });
    const el = await mount();
    expect(getItem).toHaveBeenCalled();
    expect(names(el)).toEqual(['Ada', 'Grace', 'Coder', 'Reviewer']);
  });

  it('still folds for the session when writing storage throws', async () => {
    const setItem = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded');
    });
    const el = await mount();
    // A click listener that throws is reported (window error event and
    // console.error), not rethrown, so watch both to prove the save swallows
    // the storage error.
    const reported: unknown[] = [];
    const onError = (e: ErrorEvent): void => {
      reported.push(e.error ?? e.message);
    };
    window.addEventListener('error', onError);
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      toggle(el, 'people').click();
      await el.updateComplete;
    } finally {
      window.removeEventListener('error', onError);
    }
    expect(setItem).toHaveBeenCalled();
    expect(reported).toEqual([]);
    expect(consoleError).not.toHaveBeenCalled();
    expect(toggle(el, 'people').getAttribute('aria-expanded')).toBe('false');
    expect(names(el)).toEqual(['Coder', 'Reviewer']);
  });

  function notes(el: ScionChatMembers): string[] {
    return Array.from(el.shadowRoot?.querySelectorAll('.empty-note') ?? []).map(
      (n) => n.textContent?.trim() ?? ''
    );
  }

  it('drops the empty notes while folded', async () => {
    const el = await mount();
    el.humans = [];
    el.agents = [];
    await el.updateComplete;
    expect(notes(el)).toEqual(['No members', 'No agents']);
    toggle(el, 'people').click();
    toggle(el, 'agents').click();
    await el.updateComplete;
    expect(notes(el)).toEqual([]);
  });

  it('drops the bounded-list note while People is folded', async () => {
    const el = await mount();
    el.humansTruncated = true;
    await el.updateComplete;
    expect(notes(el).some((n) => n.startsWith('Showing up to'))).toBe(true);
    toggle(el, 'people').click();
    await el.updateComplete;
    expect(notes(el)).toEqual([]);
  });

  it('shows an unread dot on a folded section only when one of its members has unread', async () => {
    const el = await mount({ unreadFromIds: ['a2'] });
    expect(toggle(el, 'agents').querySelector('.section-unread-dot')).toBeNull();
    expect(toggle(el, 'people').querySelector('.section-unread-dot')).toBeNull();
    toggle(el, 'people').click();
    toggle(el, 'agents').click();
    await el.updateComplete;
    expect(toggle(el, 'agents').querySelector('.section-unread-dot')).not.toBeNull();
    expect(toggle(el, 'people').querySelector('.section-unread-dot')).toBeNull();
  });

  it("names the unread state in the folded heading's accessible text", async () => {
    const el = await mount({ unreadFromIds: ['a2'] });
    toggle(el, 'agents').click();
    await el.updateComplete;
    const button = toggle(el, 'agents');
    expect(button.querySelector('.section-unread-dot')?.getAttribute('aria-hidden')).toBe('true');
    expect(button.querySelector('.sr-only')?.textContent).toContain('unread messages');
  });

  it('lights the People dot for an unread person outside the search', async () => {
    const el = await mount({ unreadFromIds: ['u2'] });
    el.humansTruncated = true;
    await el.updateComplete;
    const input = el.shadowRoot?.querySelector<HTMLInputElement>('.people-search input');
    if (!input) throw new Error('no people search');
    input.value = 'ada';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    toggle(el, 'people').click();
    await el.updateComplete;
    expect(toggle(el, 'people').querySelector('.section-unread-dot')).not.toBeNull();
  });

  it('does not light the People dot for the current user', async () => {
    const el = await mount({ unreadFromIds: ['me'] });
    el.humans = [...el.humans, human('me', 'Me')];
    toggle(el, 'people').click();
    await el.updateComplete;
    expect(toggle(el, 'people').querySelector('.section-unread-dot')).toBeNull();
  });

  it('counts every other person in the folded People heading, not the search matches', async () => {
    const el = await mount();
    el.humansTruncated = true;
    await el.updateComplete;
    const input = el.shadowRoot?.querySelector<HTMLInputElement>('.people-search input');
    if (!input) throw new Error('no people search');
    input.value = 'ada';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    expect(toggle(el, 'people').textContent).toContain('People — 1');
    toggle(el, 'people').click();
    await el.updateComplete;
    expect(toggle(el, 'people').textContent).toContain('People — 2');
    toggle(el, 'people').click();
    await el.updateComplete;
    expect(toggle(el, 'people').textContent).toContain('People — 1');
    expect(names(el)).toEqual(['Ada', 'Coder', 'Reviewer']);
  });

  it('hides the People search while People is folded', async () => {
    const el = await mount();
    el.humansTruncated = true;
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.people-search input')).not.toBeNull();
    toggle(el, 'people').click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.people-search input')).toBeNull();
    toggle(el, 'people').click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.people-search input')).not.toBeNull();
  });
});
