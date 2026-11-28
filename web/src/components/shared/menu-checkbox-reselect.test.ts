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
 * Regression tests for single-select `<sl-menu-item type="checkbox">` menus
 * (ptone/scion#6545).
 *
 * Shoelace toggles `item.checked` before emitting `sl-select`. Lit's
 * `?checked=${expr}` only writes the attribute when `expr` changes, so
 * re-clicking an already-active item leaves no item checked unless the
 * `sl-select` handler reconciles `checked` via `syncSingleSelectMenuChecked`.
 */

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import { syncSingleSelectMenuChecked, type SlCheckboxMenuItem } from '../../utils/menu-checkbox.js';
import { stateManager } from '../../client/state.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';
import { resetServerFlagStateForTests } from '../../utils/feature-flags.js';

class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/**
 * Simulate Shoelace 2.x `sl-menu.handleClick` for a `type="checkbox"` item:
 * Shoelace flips `item.checked` (reflected to the `checked` attribute) before
 * dispatching `sl-select`.
 */
function simulateShoelaceSelect(menu: Element, item: SlCheckboxMenuItem): void {
  const next = !item.hasAttribute('checked');
  item.value = item.getAttribute('value') ?? '';
  item.checked = next;
  item.toggleAttribute('checked', next);
  menu.dispatchEvent(
    new CustomEvent('sl-select', {
      detail: { item },
      bubbles: true,
      composed: true,
    })
  );
}

function checkedValues(menu: Element): string[] {
  return Array.from(menu.querySelectorAll<SlCheckboxMenuItem>('sl-menu-item[type="checkbox"]'))
    .filter((item) => item.hasAttribute('checked'))
    .map((item) => item.getAttribute('value') ?? '');
}

function itemByValue(menu: Element, value: string): SlCheckboxMenuItem {
  const item = menu.querySelector<SlCheckboxMenuItem>(
    `sl-menu-item[type="checkbox"][value="${value}"]`
  );
  if (!item) throw new Error(`sl-menu-item[value="${value}"] not found`);
  return item;
}

describe('syncSingleSelectMenuChecked (unit)', () => {
  it('sets both property and attribute for true/false and ignores nullish items', () => {
    const item = document.createElement('sl-menu-item') as SlCheckboxMenuItem;
    syncSingleSelectMenuChecked(item, true);
    expect(item.checked).toBe(true);
    expect(item.hasAttribute('checked')).toBe(true);

    syncSingleSelectMenuChecked(item, false);
    expect(item.checked).toBe(false);
    expect(item.hasAttribute('checked')).toBe(false);

    expect(() => syncSingleSelectMenuChecked(null, true)).not.toThrow();
    expect(() => syncSingleSelectMenuChecked(undefined, false)).not.toThrow();
  });
});

describe('single-select checkbox menus keep active item checked on re-click', () => {
  beforeAll(async () => {
    const store = new Map<string, string>();
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, String(v)),
      removeItem: (k: string) => void store.delete(k),
      clear: () => store.clear(),
    });
    vi.stubGlobal('EventSource', FakeEventSource);

    await Promise.all([
      import('./header.js'),
      import('./chat/chat-composer.js'),
      import('../pages/agents.js'),
      import('../pages/project-detail.js'),
      import('../pages/skills.js'),
    ]);
  });

  beforeEach(() => {
    localStorage.clear();
    stateManager.setScope({ type: 'brokers-list' });
    resetServerFlagStateForTests();
    resetHubProjectCapabilitiesCache();
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('header mode menu: re-clicking active mode keeps it checked; switching settles to one', async () => {
    const el = document.createElement('scion-header') as HTMLElement & {
      currentPath: string;
      updateComplete: Promise<boolean>;
    };
    el.currentPath = '/chat';
    document.body.appendChild(el);
    await el.updateComplete;

    const menu = el.shadowRoot!.querySelector('.compact-mode-dropdown sl-menu')!;
    expect(checkedValues(menu)).toEqual(['chat']);

    // Re-click active mode ("chat")
    const chatItem = itemByValue(menu, 'chat');
    simulateShoelaceSelect(menu, chatItem);
    await el.updateComplete;
    expect(chatItem.checked).toBe(true);
    expect(checkedValues(menu)).toEqual(['chat']);

    // Click a different mode ("dashboard") — stays unchecked until route changes
    const dashboardItem = itemByValue(menu, 'dashboard');
    simulateShoelaceSelect(menu, dashboardItem);
    await el.updateComplete;
    expect(checkedValues(menu)).toEqual(['chat']);

    // Once navigation updates currentPath, only "dashboard" is checked
    el.currentPath = '/';
    await el.updateComplete;
    expect(checkedValues(menu)).toEqual(['dashboard']);
  });

  it('chat-composer default-agent menu: re-clicking active agent or No agent keeps it checked', async () => {
    const el = document.createElement('scion-chat-composer') as HTMLElement & {
      conversationMode: string;
      projectId: string;
      members: unknown[];
      defaultAgent: string;
      updateComplete: Promise<boolean>;
    };
    el.conversationMode = 'thread';
    el.projectId = 'proj-1';
    el.members = [
      { id: 'agent-1', name: 'Code Reviewer', email: '', slug: 'code-reviewer', kind: 'agent' },
      { id: 'agent-2', name: 'Other Agent', email: '', slug: 'other-agent', kind: 'agent' },
    ];
    el.defaultAgent = 'agent-1';
    document.body.appendChild(el);
    await el.updateComplete;

    const menu = el.shadowRoot!.querySelector('sl-menu')!;
    expect(checkedValues(menu)).toEqual(['Code Reviewer']);

    // Re-click active default agent
    const activeItem = itemByValue(menu, 'Code Reviewer');
    simulateShoelaceSelect(menu, activeItem);
    await el.updateComplete;
    expect(activeItem.checked).toBe(true);
    expect(checkedValues(menu)).toEqual(['Code Reviewer']);

    // Click a different agent — doesn't leave two checked before parent updates
    const otherItem = itemByValue(menu, 'Other Agent');
    simulateShoelaceSelect(menu, otherItem);
    await el.updateComplete;
    expect(checkedValues(menu)).toEqual(['Code Reviewer']);

    // Once parent updates defaultAgent, only the new agent is checked
    el.defaultAgent = 'Other Agent';
    await el.updateComplete;
    expect(checkedValues(menu)).toEqual(['Other Agent']);

    // Clear default agent (switches chip template branch, mounting a new menu)
    // and re-click "No agent".
    el.defaultAgent = '';
    await el.updateComplete;
    const clearMenu = el.shadowRoot!.querySelector('sl-menu')!;
    expect(checkedValues(clearMenu)).toEqual(['__clear__']);

    const clearItem = itemByValue(clearMenu, '__clear__');
    simulateShoelaceSelect(clearMenu, clearItem);
    await el.updateComplete;
    expect(clearItem.checked).toBe(true);
    expect(checkedValues(clearMenu)).toEqual(['__clear__']);
  });

  it('agents page sort menu: re-clicking active field flips direction and keeps it checked', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ agents: [], _capabilities: { actions: [] } })))
    );

    const el = document.createElement('scion-page-agents') as HTMLElement & {
      viewMode: string;
      sortField: string;
      sortDir: string;
      updateComplete: Promise<boolean>;
    };
    document.body.appendChild(el);
    el.viewMode = 'grid';
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('sl-menu')).not.toBeNull();
    });
    await el.updateComplete;

    const menu = el.shadowRoot!.querySelector('sl-menu')!;
    const initialField = el.sortField;
    const initialDir = el.sortDir;
    expect(checkedValues(menu)).toEqual([initialField]);

    // Re-click active field to flip sort direction
    const activeItem = itemByValue(menu, initialField);
    simulateShoelaceSelect(menu, activeItem);
    await el.updateComplete;
    expect(el.sortField).toBe(initialField);
    expect(el.sortDir).not.toBe(initialDir);
    expect(activeItem.checked).toBe(true);
    expect(checkedValues(menu)).toEqual([initialField]);

    // Click a different field ("status")
    const otherField = initialField === 'status' ? 'name' : 'status';
    const otherItem = itemByValue(menu, otherField);
    simulateShoelaceSelect(menu, otherItem);
    await el.updateComplete;
    expect(el.sortField).toBe(otherField);
    expect(checkedValues(menu)).toEqual([otherField]);

    // Re-click the newly active field
    simulateShoelaceSelect(menu, otherItem);
    await el.updateComplete;
    expect(otherItem.checked).toBe(true);
    expect(checkedValues(menu)).toEqual([otherField]);
  });

  it('project-detail page sort menu: re-clicking active field flips direction and keeps it checked', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (url.includes('/api/v1/projects/proj-1/agents')) {
          return Promise.resolve(
            jsonResponse({
              agents: [
                {
                  id: 'a1',
                  name: 'a1',
                  projectId: 'proj-1',
                  template: 't',
                  phase: 'running',
                  created: '2026-01-01T00:00:00Z',
                  updated: '2026-01-01T00:00:00Z',
                  messageMode: 'project',
                  _capabilities: { actions: ['read'] },
                },
              ],
              totalCount: 1,
              complete: true,
              stats: { total: 1, running: 1, agents: [['a1', 'running']] },
              _capabilities: { actions: [] },
            })
          );
        }
        if (url.endsWith('/api/v1/projects/proj-1')) {
          return Promise.resolve(
            jsonResponse({
              id: 'proj-1',
              name: 'Demo',
              slug: 'demo',
              _capabilities: { actions: ['read'] },
            })
          );
        }
        return Promise.resolve(jsonResponse({ projects: [] }));
      })
    );

    const el = document.createElement('scion-page-project-detail') as HTMLElement & {
      projectId: string;
      pageData?: unknown;
      viewMode: string;
      sortField: string;
      sortDir: string;
      updateComplete: Promise<boolean>;
    };
    el.projectId = 'proj-1';
    el.pageData = {
      path: '/projects/proj-1',
      title: 'Demo',
      user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
    };
    document.body.appendChild(el);
    el.viewMode = 'grid';
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('sl-menu')).not.toBeNull();
    });
    await el.updateComplete;

    const menu = el.shadowRoot!.querySelector('sl-menu')!;
    const initialField = el.sortField;
    const initialDir = el.sortDir;
    expect(checkedValues(menu)).toEqual([initialField]);

    // Re-click active field
    const activeItem = itemByValue(menu, initialField);
    simulateShoelaceSelect(menu, activeItem);
    await el.updateComplete;
    expect(el.sortField).toBe(initialField);
    expect(el.sortDir).not.toBe(initialDir);
    expect(activeItem.checked).toBe(true);
    expect(checkedValues(menu)).toEqual([initialField]);

    // Click a different field
    const otherField = initialField === 'status' ? 'name' : 'status';
    const otherItem = itemByValue(menu, otherField);
    simulateShoelaceSelect(menu, otherItem);
    await el.updateComplete;
    expect(el.sortField).toBe(otherField);
    expect(checkedValues(menu)).toEqual([otherField]);

    // Re-click the newly active field
    simulateShoelaceSelect(menu, otherItem);
    await el.updateComplete;
    expect(otherItem.checked).toBe(true);
    expect(checkedValues(menu)).toEqual([otherField]);
  });

  it('skills page sort menu: re-clicking active field flips direction and keeps it checked', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ skills: [], nextCursor: '' })))
    );

    const el = document.createElement('scion-page-skills') as HTMLElement & {
      viewMode: string;
      sortField: string;
      sortDir: string;
      loading: boolean;
      updateComplete: Promise<boolean>;
    };
    document.body.appendChild(el);
    await vi.waitFor(() => expect(el.loading).toBe(false));
    el.viewMode = 'grid';
    await el.updateComplete;

    const menu = el.shadowRoot!.querySelector('sl-menu')!;
    const initialField = el.sortField;
    const initialDir = el.sortDir;
    expect(checkedValues(menu)).toEqual([initialField]);

    // Re-click active field
    const activeItem = itemByValue(menu, initialField);
    simulateShoelaceSelect(menu, activeItem);
    await el.updateComplete;
    expect(el.sortField).toBe(initialField);
    expect(el.sortDir).not.toBe(initialDir);
    expect(activeItem.checked).toBe(true);
    expect(checkedValues(menu)).toEqual([initialField]);

    // Click a different field
    const otherField = initialField === 'created' ? 'name' : 'created';
    const otherItem = itemByValue(menu, otherField);
    simulateShoelaceSelect(menu, otherItem);
    await el.updateComplete;
    expect(el.sortField).toBe(otherField);
    expect(checkedValues(menu)).toEqual([otherField]);

    // Re-click the newly active field
    simulateShoelaceSelect(menu, otherItem);
    await el.updateComplete;
    expect(otherItem.checked).toBe(true);
    expect(checkedValues(menu)).toEqual([otherField]);
  });
});
