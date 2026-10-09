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
 * Tests for the composer's thread-default destination chip.
 *
 * A thread's default agent may be stored as an agent ID (a DM promoted to a
 * thread stores the UUID) or as a slug. The chip must show the agent's name
 * either way, never the raw ID.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));
vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

const AGENT_ID = '3f2a9c1e-7b4d-4e8a-9c2f-1a2b3c4d5e6f';

const MEMBERS = [
  { id: 'user-1', name: 'Ada', email: 'ada@example.com', kind: 'user' },
  { id: AGENT_ID, name: 'Code Reviewer', email: '', slug: 'code-reviewer', kind: 'agent' },
  { id: 'agent-2', name: 'Other Agent', email: '', slug: 'other-agent', kind: 'agent' },
];

beforeAll(async () => {
  await import('./chat-composer.js');
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function renderComposer(defaultAgent: string, members: unknown[] = MEMBERS): Promise<any> {
  const el = document.createElement('scion-chat-composer') as any;
  el.conversationMode = 'thread';
  el.projectId = 'proj-1';
  el.members = members;
  el.defaultAgent = defaultAgent;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function chipName(el: any): string {
  const name = el.shadowRoot.querySelector('.destination-chip .agent-name');
  return name?.textContent?.trim() ?? '';
}

function checkedItems(el: any): string[] {
  return [...el.shadowRoot.querySelectorAll('sl-menu-item[checked]')].map((i: Element) =>
    i.getAttribute('value')
  );
}

describe('composer — thread default chip', () => {
  it('shows the agent name when the default is stored as an agent ID', async () => {
    const el = await renderComposer(AGENT_ID);
    expect(chipName(el)).toBe('Code Reviewer');
    expect(el.shadowRoot.innerHTML).not.toContain(AGENT_ID);
    expect(checkedItems(el)).toEqual(['Code Reviewer']);
  });

  it('shows the agent name when the default is stored as a slug', async () => {
    const el = await renderComposer('code-reviewer');
    expect(chipName(el)).toBe('Code Reviewer');
    expect(checkedItems(el)).toEqual(['Code Reviewer']);
  });

  it('marks the menu item checked when the default is stored as a display name', async () => {
    const el = await renderComposer('Code Reviewer');
    expect(chipName(el)).toBe('Code Reviewer');
    expect(checkedItems(el)).toEqual(['Code Reviewer']);
  });

  it("picks the slug match when the slug equals another agent's name", async () => {
    const members = [
      { id: 'agent-a', name: 'helper', email: '', slug: 'agent-a-slug', kind: 'agent' },
      { id: 'agent-b', name: 'Helper Prime', email: '', slug: 'helper', kind: 'agent' },
    ];
    const el = await renderComposer('helper', members);
    expect(chipName(el)).toBe('Helper Prime');
    expect(checkedItems(el)).toEqual(['Helper Prime']);
  });

  it('falls back to the stored value when the agent is not a member', async () => {
    const el = await renderComposer('gone-agent');
    expect(chipName(el)).toBe('gone-agent');
  });

  it('does not report a change when the current default is picked again', async () => {
    const el = await renderComposer(AGENT_ID);
    const changes: unknown[] = [];
    el.addEventListener('default-agent-change', (e: CustomEvent) => changes.push(e.detail));
    const item = document.createElement('sl-menu-item');
    item.setAttribute('value', 'Code Reviewer');
    el.handleAgentMenuSelect(new CustomEvent('sl-select', { detail: { item } }));
    expect(changes).toEqual([]);

    const other = document.createElement('sl-menu-item');
    other.setAttribute('value', 'Other Agent');
    el.handleAgentMenuSelect(new CustomEvent('sl-select', { detail: { item: other } }));
    expect(changes).toEqual([{ defaultAgent: 'Other Agent' }]);
  });
});
