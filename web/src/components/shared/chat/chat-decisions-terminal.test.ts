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
 * `<scion-chat-choices>` single-card replaced and interpreted states
 * (ptone/scion#7673, design §4.3): the replaced card's version, faded
 * disabled choices and jump link; an agent-interpreted answer read-only.
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import type { DecisionView } from '../../../shared/types.js';

vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

await import('./chat-choices.js');
type ScionChatChoices = import('./chat-choices.js').ScionChatChoices;

function view(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'r1',
    messageId: 'm1',
    key: '17',
    ref: '17',
    version: 1,
    status: 'pending',
    askerAgentId: 'a1',
    askerAgentName: 'agent-x',
    question: 'Which approach?',
    defaultChoiceId: 'a',
    choices: [
      { id: 'a', label: 'Extend input-needed' },
      { id: 'b', label: 'New waiting state' },
    ],
    ...over,
  };
}

function replacedView(): DecisionView {
  return view({
    status: 'replaced',
    replacedBy: { rowId: 'r1-v2', messageId: 'm2', version: 2, at: '2026-11-12T15:43:00Z' },
  });
}

function interpreted(by: { id: string; name: string }, over: Partial<DecisionView> = {}) {
  return view({
    status: 'decided',
    decision: {
      choiceId: 'b',
      label: 'New waiting state',
      answeredByUserId: by.id,
      answeredByName: by.name,
      provenance: 'agent_interpreted',
      interpretedByAgentId: 'a1',
      surface: 'web',
      responseMessageId: 'resp-1',
      at: '2026-11-12T15:42:00Z',
    },
    ...over,
  });
}

async function mount(d: DecisionView): Promise<ScionChatChoices> {
  const el = document.createElement('scion-chat-choices');
  el.messageId = 'm1';
  el.currentUserId = 'u-me';
  el.decisions = [d];
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const root = (el: ScionChatChoices): ShadowRoot => el.shadowRoot!;
const text = (n: Element | null | undefined): string =>
  n?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
const button = (el: ScionChatChoices, id: string): HTMLElement =>
  root(el).querySelector<HTMLElement>(`[data-choice="${id}"]`)!;

afterEach(() => {
  document.body.innerHTML = '';
});

describe('replaced single card', () => {
  it('shows the version, fades and disables every choice, and drops the Enter hint', async () => {
    const el = await mount(replacedView());
    expect(text(root(el).querySelector('.card-head .version'))).toBe('v1');
    expect(root(el).querySelector('.row.replaced')).not.toBeNull();
    for (const id of ['a', 'b']) {
      expect(button(el, id).hasAttribute('disabled')).toBe(true);
      expect(button(el, id).hasAttribute('outline')).toBe(false);
    }
    expect(root(el).querySelector('.hint')).toBeNull();
    expect(root(el).querySelector('.card')!.hasAttribute('aria-describedby')).toBe(false);
  });

  it('reads "Replaced by v2 · time · Jump to v2" and the link emits choice-jump', async () => {
    const el = await mount(replacedView());
    const foot = root(el).querySelector('.row-foot')!;
    expect(text(foot)).toMatch(/^Replaced by v2 · \d\d:\d\d · Jump to v2$/);
    expect(foot.querySelector('sl-icon')?.getAttribute('name')).toBe('arrow-repeat');

    const link = foot.querySelector<HTMLAnchorElement>('a.jump')!;
    expect(link.getAttribute('href')).toBe('#msg-m2');
    const onJump = vi.fn();
    document.body.addEventListener('choice-jump', onJump);
    const click = new MouseEvent('click', { bubbles: true, cancelable: true, composed: true });
    link.dispatchEvent(click);
    document.body.removeEventListener('choice-jump', onJump);
    expect(click.defaultPrevented).toBe(true);
    expect(onJump).toHaveBeenCalledTimes(1);
    expect((onJump.mock.calls[0]![0] as CustomEvent).detail).toEqual({ messageId: 'm2' });
  });

  it('shows the version of a newer card that is still pending', async () => {
    const el = await mount(view({ version: 2 }));
    expect(text(root(el).querySelector('.card-head .version'))).toBe('v2');
  });

  it('shows no version on a first version still pending', async () => {
    const el = await mount(view());
    expect(root(el).querySelector('.card-head .version')).toBeNull();
  });

  it('updates live from pending to replaced', async () => {
    const el = await mount(view());
    el.decisions = [replacedView()];
    await el.updateComplete;
    expect(text(root(el).querySelector('.row-foot'))).toMatch(/^Replaced by v2/);
  });
});

describe('interpreted answer', () => {
  it('marks the chosen answer read-only with the robot provenance line', async () => {
    const el = await mount(interpreted({ id: 'u-alice', name: 'alice' }));
    const foot = root(el).querySelector('.row-foot')!;
    expect(text(foot)).toMatch(/^Interpreted by agent-x from alice's reply · \d\d:\d\d$/);
    expect(foot.querySelector('sl-icon')?.getAttribute('name')).toBe('robot');
    expect(button(el, 'b').getAttribute('variant')).toBe('primary');
    expect(button(el, 'a').hasAttribute('disabled')).toBe(true);
    // No confirm/override controls (Phase 2).
    expect(root(el).querySelectorAll('sl-button').length).toBe(2);
    expect(text(root(el).querySelector('.card'))).not.toMatch(/confirm|override/i);
  });

  it('reads "your reply" for the viewer', async () => {
    const el = await mount(interpreted({ id: 'u-me', name: 'me' }));
    expect(text(root(el).querySelector('.row-foot'))).toMatch(
      /^Interpreted by agent-x from your reply/
    );
  });

  it('renders read-only in a set too', async () => {
    const el = document.createElement('scion-chat-choices');
    el.currentUserId = 'u-me';
    el.answerMode = 'set';
    el.decisions = [
      interpreted({ id: 'u-alice', name: 'alice' }),
      view({ rowId: 'r2', key: '18' }),
    ];
    document.body.appendChild(el);
    await el.updateComplete;
    const r1 = root(el).querySelector('.row[data-row="r1"]')!;
    expect(text(r1.querySelector('.row-foot'))).toMatch(/^Interpreted by agent-x/);
    expect(r1.querySelector('[data-choice="a"]')!.hasAttribute('disabled')).toBe(true);
    // Row 18's recommended choice is only outlined (ptone/scion#7842).
    expect(text(root(el).querySelector('.counter'))).toBe('1 remaining · 1 answered by alice');
  });
});
