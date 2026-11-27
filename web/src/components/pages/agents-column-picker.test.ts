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
 * Hub-wide agents list column picker tests (ptone/scion#9577, web.agents_column_picker):
 * - Hidden when experiment is off (even if localStorage has hidden columns)
 * - Shown in list view only when experiment is on
 * - Hides toggled columns from both <thead> and <tbody>, keeps Name and Actions always shown
 * - Persists to localStorage under `scion-columns-agents` and restores on mount
 * - Ignores invalid / unknown / pinned stored keys gracefully
 * - Reset to defaults restores all columns and clears storage
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Agent, PageData } from '../../shared/types.js';
import type { ScionPageAgents } from './agents.js';
import { AGENT_COLUMNS_STORAGE_KEY } from '../../shared/agent-columns.js';
import {
  AGENTS_COLUMN_PICKER_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import { stateManager } from '../../client/state.js';

vi.mock('../../client/navigation.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/navigation.js')>();
  return {
    ...actual,
    navigateTo: vi.fn(),
    replaceSearch: vi.fn(),
  };
});

class FakeEventSource {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  readyState = 1;
  addEventListener(): void {}
  removeEventListener(): void {}
  close(): void {
    this.readyState = 2;
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'agent-1',
    name: 'worker-alpha',
    slug: 'worker-alpha',
    projectId: 'proj-1',
    project: 'Alpha',
    template: 'developer',
    phase: 'running',
    activity: 'working',
    suspendPolicy: 'auto',
    updated: '2026-11-27T21:00:00Z',
    taskSummary: 'Implementing column picker',
    _capabilities: { actions: ['read', 'update', 'lifecycle', 'delete', 'attach'] },
    ...overrides,
  };
}

function headerLabels(el: ScionPageAgents): string[] {
  return Array.from(el.shadowRoot!.querySelectorAll('table thead th')).map((th) =>
    (th.textContent ?? '').replace(/[▲▼]/g, '').trim()
  );
}

describe('Hub-wide agents list column picker (scion-page-agents)', () => {
  beforeAll(async () => {
    await import('./agents.js');
  });

  beforeEach(() => {
    localStorage.clear();
    localStorage.setItem('scion-view-agents', 'list');
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.stubGlobal('EventSource', FakeEventSource);
    stateManager.setScope({ type: 'project', projectId: 'reset' });
    stateManager.setScope({ type: 'dashboard' });

    const agent = makeAgent();
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse({
            agents: [agent],
            totalCount: 1,
            complete: true,
            stats: { total: 1, running: 1 },
            _capabilities: { actions: ['read', 'create'] },
          })
        )
      )
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  async function mountAgentsPage(): Promise<ScionPageAgents> {
    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    el.pageData = {
      path: '/agents',
      title: 'Agents',
      user: { id: 'u1', email: 'dev@example.com', name: 'Dev', role: 'admin' },
    } as PageData;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('table tbody tr')).not.toBeNull();
    });
    return el;
  }

  it('hides the picker, never reads scion-columns-agents, and renders all columns when web.agents_column_picker is off', async () => {
    localStorage.setItem(
      AGENT_COLUMNS_STORAGE_KEY,
      JSON.stringify({ hidden: ['template', 'task'] })
    );
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem');
    const el = await mountAgentsPage();

    expect(getItemSpy.mock.calls.some(([key]) => key === AGENT_COLUMNS_STORAGE_KEY)).toBe(false);
    expect(el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]')).toBeNull();
    expect(headerLabels(el)).toEqual([
      'Name',
      'Project',
      'Template',
      'Status',
      'Traits',
      'Updated',
      'Task',
      'Actions',
    ]);
    expect(el.shadowRoot!.querySelectorAll('table tbody tr td').length).toBe(8);

    // Late flag enable hydrates persisted hidden columns on demand
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    await el.updateComplete;
    expect(getItemSpy.mock.calls.some(([key]) => key === AGENT_COLUMNS_STORAGE_KEY)).toBe(true);
    expect(el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]')).not.toBeNull();
    expect(headerLabels(el)).toEqual(['Name', 'Project', 'Status', 'Traits', 'Updated', 'Actions']);
    getItemSpy.mockRestore();
  });

  it('renders the picker in list view when enabled with both Name and Actions pinned and disabled', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    const el = await mountAgentsPage();

    const trigger = el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]');
    expect(trigger).not.toBeNull();
    expect(trigger?.textContent?.trim()).toBe('Columns');
    expect(headerLabels(el)).toEqual([
      'Name',
      'Project',
      'Template',
      'Status',
      'Traits',
      'Updated',
      'Task',
      'Actions',
    ]);

    const nameToggle = el.shadowRoot?.querySelector('[data-testid="column-toggle-name"]');
    expect(nameToggle?.hasAttribute('disabled')).toBe(true);
    expect(nameToggle?.hasAttribute('checked')).toBe(true);
    const actionsToggle = el.shadowRoot?.querySelector('[data-testid="column-toggle-actions"]');
    expect(actionsToggle?.hasAttribute('disabled')).toBe(true);
    expect(actionsToggle?.hasAttribute('checked')).toBe(true);
  });

  it('hides toggled columns from header and body, persists to localStorage, and resets', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    const el = await mountAgentsPage();

    const menu = el.shadowRoot!.querySelector('.columns-picker sl-menu')!;
    menu.dispatchEvent(
      new CustomEvent('sl-select', {
        detail: { item: { value: 'template' } },
        bubbles: true,
        composed: true,
      })
    );
    menu.dispatchEvent(
      new CustomEvent('sl-select', {
        detail: { item: { value: 'task' } },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;

    expect(headerLabels(el)).toEqual(['Name', 'Project', 'Status', 'Traits', 'Updated', 'Actions']);
    expect(el.shadowRoot!.querySelectorAll('table tbody tr td').length).toBe(6);
    expect(localStorage.getItem(AGENT_COLUMNS_STORAGE_KEY)).toBe(
      JSON.stringify({ hidden: ['template', 'task'] })
    );
    expect(
      el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]')?.textContent?.trim()
    ).toBe('Columns (4/6)');

    // Attempting to toggle Name or Actions leaves them visible
    menu.dispatchEvent(
      new CustomEvent('sl-select', {
        detail: { item: { value: 'name' } },
        bubbles: true,
        composed: true,
      })
    );
    menu.dispatchEvent(
      new CustomEvent('sl-select', {
        detail: { item: { value: 'actions' } },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    expect(headerLabels(el)).toContain('Name');
    expect(headerLabels(el)).toContain('Actions');

    // Reset restores all columns and clears localStorage
    menu.dispatchEvent(
      new CustomEvent('sl-select', {
        detail: { item: { value: '__reset__' } },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    expect(headerLabels(el)).toEqual([
      'Name',
      'Project',
      'Template',
      'Status',
      'Traits',
      'Updated',
      'Task',
      'Actions',
    ]);
    expect(localStorage.getItem(AGENT_COLUMNS_STORAGE_KEY)).toBeNull();
  });

  it('restores persisted hidden columns on mount and ignores unknown or pinned keys', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    localStorage.setItem(
      AGENT_COLUMNS_STORAGE_KEY,
      JSON.stringify({ hidden: ['name', 'actions', 'unknown_col', 'traits', 'updated'] })
    );
    const el = await mountAgentsPage();

    expect(headerLabels(el)).toEqual(['Name', 'Project', 'Template', 'Status', 'Task', 'Actions']);
    expect(el.shadowRoot!.querySelector('[data-testid="traits-col-header"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('[data-testid="traits-cell"]')).toBeNull();
  });

  it('keeps the failed-deletion banner visible when the Status column is hidden', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    const failedAgent = makeAgent({
      deletion: {
        state: 'failed',
        code: 'broker_error',
        error: 'container busy',
        soft: false,
        startedAt: '2026-11-27T21:00:00Z',
        expiresAt: '2099-01-01T00:00:00Z',
      },
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse({
            agents: [failedAgent],
            totalCount: 1,
            complete: true,
            stats: { total: 1, running: 1 },
            _capabilities: { actions: ['read', 'create'] },
          })
        )
      )
    );
    const el = await mountAgentsPage();

    expect(el.shadowRoot!.querySelectorAll('scion-deletion-banner').length).toBe(1);

    const menu = el.shadowRoot!.querySelector('.columns-picker sl-menu')!;
    menu.dispatchEvent(
      new CustomEvent('sl-select', {
        detail: { item: { value: 'status' } },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;

    expect(headerLabels(el)).not.toContain('Status');
    const banners = el.shadowRoot!.querySelectorAll('scion-deletion-banner');
    expect(banners.length).toBe(1);
    expect(banners[0].closest('td')?.querySelector('.name-cell')).not.toBeNull();
  });

  it('does not render the picker in grid view', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    localStorage.setItem('scion-view-agents', 'grid');
    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    el.pageData = {
      path: '/agents',
      title: 'Agents',
      user: { id: 'u1', email: 'dev@example.com', name: 'Dev', role: 'admin' },
    } as PageData;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('.agent-card')).not.toBeNull();
    });
    expect(el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]')).toBeNull();
  });
});
