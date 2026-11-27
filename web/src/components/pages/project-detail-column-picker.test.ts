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
 * Project-specific agents list column picker tests (ptone/scion#9577, web.agents_column_picker):
 * - Hidden when experiment is off (even if localStorage has hidden columns)
 * - Shown in list view only when experiment is on
 * - Hides toggled columns from both <thead> and <tbody>, keeps Name and Actions pinned
 * - Persists to localStorage under `scion-columns-project-agents` independently of `scion-columns-agents`
 * - Ignores invalid / unknown / hub-only / pinned stored keys gracefully
 * - Reset to defaults restores all columns and clears storage
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Agent, PageData } from '../../shared/types.js';
import type { ScionPageProjectDetail } from './project-detail.js';
import {
  AGENT_COLUMNS_STORAGE_KEY,
  PROJECT_AGENT_COLUMNS_STORAGE_KEY,
} from '../../shared/agent-columns.js';
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

function requestUrl(input: RequestInfo | URL): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.pathname + input.search;
  return input.url;
}

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'agent-1',
    name: 'worker-alpha',
    slug: 'worker-alpha',
    projectId: 'proj-1',
    project: 'Alpha',
    template: 'developer',
    runtimeBrokerId: 'broker-1',
    runtimeBrokerName: 'local-docker',
    phase: 'running',
    activity: 'working',
    suspendPolicy: 'auto',
    updated: '2026-11-27T21:00:00Z',
    taskSummary: 'Implementing column picker',
    _capabilities: { actions: ['read', 'update', 'lifecycle', 'delete', 'attach'] },
    ...overrides,
  };
}

function headerLabels(el: ScionPageProjectDetail): string[] {
  return Array.from(el.shadowRoot!.querySelectorAll('.agent-table-container table thead th')).map(
    (th) => (th.textContent ?? '').replace(/[▲▼]/g, '').trim()
  );
}

describe('Project agents list column picker (scion-page-project-detail)', () => {
  beforeAll(async () => {
    await import('./project-detail.js');
  });

  beforeEach(() => {
    localStorage.clear();
    localStorage.setItem('scion-view-project-agents', 'list');
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    window.history.pushState({}, '', '/projects/proj-1');
    vi.stubGlobal('EventSource', FakeEventSource);
    stateManager.setScope({ type: 'project', projectId: 'proj-1' });

    const agent = makeAgent();
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL): Promise<Response> => {
        const url = requestUrl(input);
        if (url.includes('/api/v1/projects/proj-1/agents')) {
          return Promise.resolve(
            jsonResponse({
              agents: [agent],
              totalCount: 1,
              complete: true,
              stats: { total: 1, running: 1 },
            })
          );
        }
        if (url.endsWith('/api/v1/projects/proj-1')) {
          return Promise.resolve(
            jsonResponse({
              id: 'proj-1',
              name: 'Alpha',
              slug: 'alpha',
              status: 'active',
              _capabilities: { actions: ['read', 'update'] },
            })
          );
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    document.body.innerHTML = '';
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  async function mountProjectDetail(): Promise<ScionPageProjectDetail> {
    const el = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    el.pageData = {
      path: '/projects/proj-1',
      title: 'Alpha',
      user: { id: 'u1', email: 'dev@example.com', name: 'Dev', role: 'admin' },
    } as PageData;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('.agent-table-container table tbody tr')).not.toBeNull();
    });
    return el;
  }

  it('hides the picker, never reads scion-columns-project-agents, and renders all 7 columns when web.agents_column_picker is off', async () => {
    localStorage.setItem(
      PROJECT_AGENT_COLUMNS_STORAGE_KEY,
      JSON.stringify({ hidden: ['broker', 'task'] })
    );
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem');
    const el = await mountProjectDetail();

    expect(getItemSpy.mock.calls.some(([key]) => key === PROJECT_AGENT_COLUMNS_STORAGE_KEY)).toBe(
      false
    );
    expect(el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]')).toBeNull();
    expect(headerLabels(el)).toEqual([
      'Name',
      'Template',
      'Broker',
      'Status',
      'Updated',
      'Task',
      'Actions',
    ]);
    expect(el.shadowRoot!.querySelectorAll('.agent-table-container table tbody tr td').length).toBe(
      7
    );

    // Late flag enable hydrates persisted hidden columns on demand
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    await el.updateComplete;
    expect(getItemSpy.mock.calls.some(([key]) => key === PROJECT_AGENT_COLUMNS_STORAGE_KEY)).toBe(
      true
    );
    expect(el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]')).not.toBeNull();
    expect(headerLabels(el)).toEqual(['Name', 'Template', 'Status', 'Updated', 'Actions']);
    getItemSpy.mockRestore();
  });

  it('renders the picker in list view when enabled with both Name and Actions pinned and disabled', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    const el = await mountProjectDetail();

    const trigger = el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]');
    expect(trigger).not.toBeNull();
    expect(trigger?.textContent?.trim()).toBe('Columns');
    expect(headerLabels(el)).toEqual([
      'Name',
      'Template',
      'Broker',
      'Status',
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

  it('hides toggled columns, persists under scion-columns-project-agents, keeps Name and Actions pinned, and resets', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    const el = await mountProjectDetail();

    const menu = el.shadowRoot!.querySelector('.columns-picker sl-menu')!;
    menu.dispatchEvent(
      new CustomEvent('sl-select', {
        detail: { item: { value: 'broker' } },
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

    expect(headerLabels(el)).toEqual(['Name', 'Template', 'Status', 'Updated', 'Actions']);
    expect(el.shadowRoot!.querySelectorAll('.agent-table-container table tbody tr td').length).toBe(
      5
    );
    expect(localStorage.getItem(PROJECT_AGENT_COLUMNS_STORAGE_KEY)).toBe(
      JSON.stringify({ hidden: ['broker', 'task'] })
    );
    expect(localStorage.getItem(AGENT_COLUMNS_STORAGE_KEY)).toBeNull();
    expect(
      el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]')?.textContent?.trim()
    ).toBe('Columns (3/5)');

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

    // Reset restores all 7 columns and clears localStorage
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
      'Template',
      'Broker',
      'Status',
      'Updated',
      'Task',
      'Actions',
    ]);
    expect(localStorage.getItem(PROJECT_AGENT_COLUMNS_STORAGE_KEY)).toBeNull();
  });

  it('restores persisted hidden columns on mount and ignores unknown, hub-only, or pinned keys', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    localStorage.setItem(
      PROJECT_AGENT_COLUMNS_STORAGE_KEY,
      JSON.stringify({ hidden: ['name', 'actions', 'project', 'traits', 'unknown', 'template'] })
    );
    const el = await mountProjectDetail();

    expect(headerLabels(el)).toEqual(['Name', 'Broker', 'Status', 'Updated', 'Task', 'Actions']);
    expect(el.shadowRoot!.querySelectorAll('.agent-table-container table tbody tr td').length).toBe(
      6
    );
  });

  it('does not render the picker in grid view', async () => {
    setFeatureFlag(AGENTS_COLUMN_PICKER_FLAG, true);
    localStorage.setItem('scion-view-project-agents', 'grid');
    const el = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    el.pageData = {
      path: '/projects/proj-1',
      title: 'Alpha',
      user: { id: 'u1', email: 'dev@example.com', name: 'Dev', role: 'admin' },
    } as PageData;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('.agent-card')).not.toBeNull();
    });
    expect(el.shadowRoot?.querySelector('[data-testid="columns-picker-trigger"]')).toBeNull();
  });
});
