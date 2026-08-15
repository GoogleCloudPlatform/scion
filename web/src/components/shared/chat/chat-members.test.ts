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
 * Tests for the <scion-chat-members> status badge and wobble animation.
 *
 * An agent avatar wobbles for 3s whenever the agent moves into a non-terminal
 * state; moving into a terminal state (blocked, completed, stopped, ...) stops
 * the wobble instead, because the agent has just gone quiet.
 *
 * The wobble state lives in a `@state()` Set, which Lit compares by reference —
 * so it must be replaced, never mutated in place, or no re-render is scheduled
 * and the wobble is invisible.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import './chat-members.js';
import type { ScionChatMembers, ChatAgentMember } from './chat-members.js';

function agent(overrides: Partial<ChatAgentMember> = {}): ChatAgentMember {
  return {
    id: 'agent-1',
    kind: 'agent',
    displayName: 'Coder',
    slug: 'coder',
    phase: 'running',
    activity: 'working',
    ...overrides,
  };
}

async function mount(agents: ChatAgentMember[]): Promise<ScionChatMembers> {
  const el = document.createElement('scion-chat-members') as ScionChatMembers;
  el.agents = agents;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

/** Does the agent row's avatar carry the wobble class? */
function isWobbling(el: ScionChatMembers): boolean {
  return !!el.shadowRoot?.querySelector('.avatar-wrapper.active');
}

/** The status shown on the agent's badge. */
function badgeStatus(el: ScionChatMembers): string | null {
  return el.shadowRoot?.querySelector('scion-status-badge')?.getAttribute('status') ?? null;
}

describe('scion-chat-members status badge', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('shows the fine-grained activity while the agent is running', async () => {
    const el = await mount([agent({ phase: 'running', activity: 'thinking' })]);
    expect(badgeStatus(el)).toBe('thinking');
  });

  it('shows the phase when the agent is not running', async () => {
    const el = await mount([agent({ phase: 'provisioning', activity: 'working' })]);
    expect(badgeStatus(el)).toBe('provisioning');
  });

  it('falls back to the phase for an unrecognised activity', async () => {
    const el = await mount([agent({ phase: 'running', activity: 'napping' })]);
    expect(badgeStatus(el)).toBe('running');
  });
});

describe('scion-chat-members agent tooltip', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  /** The content bound onto the agent row's tooltip. */
  function tooltipContent(el: ScionChatMembers): string {
    const tip = el.shadowRoot?.querySelector('sl-tooltip') as { content?: string } | null;
    return tip?.content ?? '';
  }

  it('shows the status detail message, not the bare activity word', async () => {
    const el = await mount([
      agent({ activity: 'blocked', detailMessage: 'Waiting for user decision on c34' }),
    ]);
    expect(tooltipContent(el)).toContain('Waiting for user decision on c34');
    expect(tooltipContent(el).split('\n')[0]).toBe('Waiting for user decision on c34');
  });

  it('falls back to the activity when no detail message was reported', async () => {
    const el = await mount([agent({ activity: 'thinking' })]);
    expect(tooltipContent(el)).toBe('thinking');
  });

  it('shows the last activity event as the updated time', async () => {
    const tenMinAgo = new Date(Date.now() - 10 * 60_000).toISOString();
    const el = await mount([
      agent({ detailMessage: 'Running tests', lastActivityEvent: tenMinAgo, lastSeen: '' }),
    ]);
    expect(tooltipContent(el)).toBe('Running tests\nUpdated: 10 min ago');
  });

  it('ignores the heartbeat time — only the activity event drives "Updated"', async () => {
    const el = await mount([
      agent({ detailMessage: 'Running tests', lastSeen: new Date().toISOString() }),
    ]);
    expect(tooltipContent(el)).toBe('Running tests');
  });
});

describe('scion-chat-members wobble', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  it('re-renders with the wobble class when an agent activity changes', async () => {
    const el = await mount([agent()]);
    expect(isWobbling(el)).toBe(false);

    // Same agent, new activity — a new array so Lit sees the property change.
    el.agents = [agent({ activity: 'thinking' })];
    await el.updateComplete;
    // The state change is detected in updated(); the resulting Set replacement
    // schedules a second render.
    await el.updateComplete;

    expect(isWobbling(el)).toBe(true);
  });

  it('re-renders without the wobble class once the 3s timer elapses', async () => {
    const el = await mount([agent()]);

    el.agents = [agent({ activity: 'executing' })];
    await el.updateComplete;
    await el.updateComplete;
    expect(isWobbling(el)).toBe(true);

    vi.advanceTimersByTime(2999);
    await el.updateComplete;
    expect(isWobbling(el)).toBe(true);

    vi.advanceTimersByTime(1);
    await el.updateComplete;

    expect(isWobbling(el)).toBe(false);
  });

  it('does not wobble when the agent enters a terminal state', async () => {
    const el = await mount([agent()]);

    el.agents = [agent({ phase: 'stopped', activity: '' })];
    await el.updateComplete;
    await el.updateComplete;

    expect(isWobbling(el)).toBe(false);
  });

  it('stops an in-flight wobble as soon as a terminal state arrives', async () => {
    const el = await mount([agent()]);

    el.agents = [agent({ activity: 'thinking' })];
    await el.updateComplete;
    await el.updateComplete;
    expect(isWobbling(el)).toBe(true);

    vi.advanceTimersByTime(500);
    el.agents = [agent({ activity: 'blocked' })];
    await el.updateComplete;
    await el.updateComplete;

    expect(isWobbling(el)).toBe(false);
  });

  it('does not wobble on first render of a previously unseen agent', async () => {
    const el = await mount([agent()]);

    el.agents = [agent(), agent({ id: 'agent-2', displayName: 'Reviewer' })];
    await el.updateComplete;
    await el.updateComplete;

    expect(isWobbling(el)).toBe(false);
  });
});
