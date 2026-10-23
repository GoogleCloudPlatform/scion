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
 * Agent badges on the graph node (web.agent_badges, ptone/scion#5503): with
 * the experiment on, each node's name line starts with `scion-agent-badges`
 * built from the agent's harnessVendor and modelSize; with it off the node
 * markup is unchanged (a bare name, no badges element).
 */

import { describe, it, expect, beforeAll, afterEach, vi } from 'vitest';
import './agent-tree-view.js';
import type { ScionAgentTreeView } from './agent-tree-view.js';
import type { Agent } from '../../shared/types.js';

beforeAll(() => {
  const store = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
});

function agent(id: string, overrides: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: 'p1',
    template: 't',
    phase: 'running',
    ancestry: ['user-1'],
    ...overrides,
  } as Agent;
}

async function mount(agents: Agent[]): Promise<ScionAgentTreeView> {
  const el = document.createElement('scion-agent-tree-view');
  el.agents = agents;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function node(el: ScionAgentTreeView, id: string): HTMLElement {
  return el.shadowRoot!.querySelector<HTMLElement>(`a.node[data-agent-id="${id}"]`)!;
}

describe('scion-agent-tree-view agent badges', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  it('puts the badges before the name on each node when on', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_badges': true };
    const el = await mount([
      agent('a1', { harnessVendor: 'anthropic', modelSize: 'medium' }),
      agent('a2', { harnessVendor: 'copilot' }),
    ]);

    const row = node(el, 'a1').querySelector('.name-row');
    expect(row).not.toBeNull();
    const badges = row!.firstElementChild as HTMLElement;
    expect(badges.tagName.toLowerCase()).toBe('scion-agent-badges');
    expect(badges.getAttribute('vendor')).toBe('anthropic');
    expect(badges.getAttribute('size')).toBe('medium');
    expect(row!.querySelector('.name')?.textContent).toBe('a1');

    const other = node(el, 'a2').querySelector('scion-agent-badges');
    expect(other?.getAttribute('vendor')).toBe('copilot');
    expect(other?.getAttribute('size')).toBe('');
  });

  it('updates a node in place when a live update changes its fields', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_badges': true };
    const el = await mount([agent('a1', { harnessVendor: 'google', modelSize: 'large' })]);
    const before = node(el, 'a1').querySelector('scion-agent-badges');

    el.agents = [agent('a1', { harnessVendor: 'google', modelSize: '' })];
    await el.updateComplete;

    const after = node(el, 'a1').querySelector('scion-agent-badges');
    expect(after).toBe(before);
    expect(after?.getAttribute('size')).toBe('');
  });

  it('leaves the node unchanged when off', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_badges': false };
    const el = await mount([agent('a1', { harnessVendor: 'anthropic', modelSize: 'large' })]);

    const n = node(el, 'a1');
    expect(n.querySelector('scion-agent-badges')).toBeNull();
    expect(n.querySelector('.name-row')).toBeNull();
    expect(n.querySelector(':scope > .name')?.textContent).toBe('a1');
  });
});
