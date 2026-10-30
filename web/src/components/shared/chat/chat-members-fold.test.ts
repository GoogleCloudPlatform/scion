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

  it('ignores corrupt stored state', async () => {
    localStorage.setItem(STORAGE_KEY, '{not json');
    const el = await mount();
    expect(names(el)).toEqual(['Ada', 'Grace', 'Coder', 'Reviewer']);
  });

  it('shows an unread dot on a folded section only when one of its members has unread', async () => {
    const el = await mount({ unreadFromIds: ['a2'] });
    toggle(el, 'people').click();
    toggle(el, 'agents').click();
    await el.updateComplete;
    expect(toggle(el, 'agents').querySelector('.section-unread-dot')).not.toBeNull();
    expect(toggle(el, 'people').querySelector('.section-unread-dot')).toBeNull();
  });
});
