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
 * Tests for the <scion-chat-members> wobble animation.
 *
 * An agent avatar wobbles for 2s whenever the agent's phase/activity changes.
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
    el.agents = [agent({ activity: 'blocked' })];
    await el.updateComplete;
    // The state change is detected in updated(); the resulting Set replacement
    // schedules a second render.
    await el.updateComplete;

    expect(isWobbling(el)).toBe(true);
  });

  it('re-renders without the wobble class once the 2s timer elapses', async () => {
    const el = await mount([agent()]);

    el.agents = [agent({ phase: 'stopped' })];
    await el.updateComplete;
    await el.updateComplete;
    expect(isWobbling(el)).toBe(true);

    vi.advanceTimersByTime(2000);
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
