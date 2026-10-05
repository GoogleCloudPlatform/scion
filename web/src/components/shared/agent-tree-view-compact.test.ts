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
 * <scion-agent-tree-view> with compact list items and incomplete sets:
 * - the creator label reads the compact item's `creatorName`, then the
 *   applied config, then `createdBy`, on both the root node and the user
 *   node;
 * - a node whose parent agent is not in the loaded set renders as a root;
 *   with markMissingAncestors set (the host knows the set is incomplete)
 *   it carries an "ancestor not loaded" marker, and with it unset (the
 *   default, a complete set whose missing parent was deleted or is in
 *   another project) it has none; a node whose parent is loaded, or whose
 *   parent is the root user, never has one.
 */

// @vitest-environment happy-dom

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
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

function agent(id: string, ancestry: string[], extra: Partial<Agent> = {}): Agent {
  return {
    id,
    name: `agent ${id}`,
    ancestry,
    projectId: 'p1',
    template: 't',
    phase: 'running',
    ...extra,
  } as Agent;
}

let el: ScionAgentTreeView | null = null;

async function mount(agents: Agent[], markMissingAncestors = false): Promise<ScionAgentTreeView> {
  el = document.createElement('scion-agent-tree-view');
  el.agents = agents;
  el.markMissingAncestors = markMissingAncestors;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function node(view: ScionAgentTreeView, id: string): HTMLElement {
  const n = view.shadowRoot?.querySelector<HTMLElement>(`.node[data-agent-id="${id}"]`);
  if (!n) throw new Error(`no node ${id}`);
  return n;
}

afterEach(() => {
  el?.remove();
  el = null;
});

describe('scion-agent-tree-view creator', () => {
  it('a compact root item shows its creatorName', async () => {
    const view = await mount([agent('r1', ['user-1'], { creatorName: 'alice@example.com' })]);
    expect(node(view, 'r1').getAttribute('title')).toContain('created by alice@example.com');
  });

  it('creatorName wins over the applied config and createdBy', async () => {
    const view = await mount([
      agent('r1', ['user-1'], {
        creatorName: 'alice',
        appliedConfig: { creatorName: 'bob' },
        createdBy: 'user-1',
      }),
      agent('r2', ['user-1'], { appliedConfig: { creatorName: 'bob' }, createdBy: 'user-1' }),
      agent('r3', ['user-1'], { createdBy: 'user-1' }),
    ]);
    expect(node(view, 'r1').getAttribute('title')).toContain('created by alice');
    expect(node(view, 'r2').getAttribute('title')).toContain('created by bob');
    expect(node(view, 'r3').getAttribute('title')).toContain('created by user-1');
  });

  it('the user node is labelled from a compact item creatorName', async () => {
    const view = await mount([agent('r1', ['user-1'], { creatorName: 'alice' })]);
    (view as unknown as { showUsers: boolean }).showUsers = true;
    await view.updateComplete;
    const user = view.shadowRoot?.querySelector('.node.user');
    expect(user?.textContent).toContain('alice');
  });

  it('the user node label prefers creatorName over the applied config and createdBy', async () => {
    const view = await mount([
      agent('r1', ['user-1'], {
        creatorName: 'alice',
        appliedConfig: { creatorName: 'bob' },
        createdBy: 'carol',
      }),
    ]);
    (view as unknown as { showUsers: boolean }).showUsers = true;
    await view.updateComplete;
    const user = view.shadowRoot?.querySelector('.node.user');
    expect(user?.textContent).toContain('alice');
    expect(user?.textContent).not.toContain('bob');
    expect(user?.textContent).not.toContain('carol');
  });
});

describe('scion-agent-tree-view ancestor not loaded', () => {
  it('marks a node whose parent agent is not loaded, with an accessible label', async () => {
    const view = await mount([agent('c1', ['user-1', 'missing-parent'])], true);
    const marker = node(view, 'c1').querySelector('.ancestor-missing');
    expect(marker).not.toBeNull();
    expect(marker?.getAttribute('role')).toBe('img');
    expect(marker?.getAttribute('aria-label')).toBe('Ancestor not loaded');
    expect(marker?.textContent).toContain('ancestor not loaded');
  });

  it('has no marker when the parent is loaded', async () => {
    const view = await mount([agent('p1', ['user-1']), agent('c1', ['user-1', 'p1'])], true);
    expect(node(view, 'c1').querySelector('.ancestor-missing')).toBeNull();
    expect(node(view, 'p1').querySelector('.ancestor-missing')).toBeNull();
  });

  it('has no marker for a root whose parent is the user', async () => {
    const view = await mount([agent('r1', ['user-1'])], true);
    expect(node(view, 'r1').querySelector('.ancestor-missing')).toBeNull();
  });

  it('the marker goes away once the parent arrives', async () => {
    const view = await mount([agent('c1', ['user-1', 'p1'])], true);
    expect(node(view, 'c1').querySelector('.ancestor-missing')).not.toBeNull();
    view.agents = [agent('p1', ['user-1']), agent('c1', ['user-1', 'p1'])];
    await view.updateComplete;
    expect(node(view, 'c1').querySelector('.ancestor-missing')).toBeNull();
  });

  it('has no marker by default, for a parent in another project or a deleted parent', async () => {
    // c1's parent lives in another project the host filtered out; c2's
    // parent was deleted. The host has not said the set is incomplete.
    const view = await mount([
      agent('c1', ['user-1', 'other-project-parent']),
      agent('c2', ['user-1', 'deleted-parent']),
    ]);
    expect(view.markMissingAncestors).toBe(false);
    expect(node(view, 'c1').querySelector('.ancestor-missing')).toBeNull();
    expect(node(view, 'c2').querySelector('.ancestor-missing')).toBeNull();
  });

  it('marks the same nodes once the host sets markMissingAncestors, and clears them when unset', async () => {
    const view = await mount([
      agent('c1', ['user-1', 'other-project-parent']),
      agent('c2', ['user-1', 'deleted-parent']),
    ]);
    view.markMissingAncestors = true;
    await view.updateComplete;
    expect(node(view, 'c1').querySelector('.ancestor-missing')).not.toBeNull();
    expect(node(view, 'c2').querySelector('.ancestor-missing')).not.toBeNull();
    view.markMissingAncestors = false;
    await view.updateComplete;
    expect(node(view, 'c1').querySelector('.ancestor-missing')).toBeNull();
  });
});
