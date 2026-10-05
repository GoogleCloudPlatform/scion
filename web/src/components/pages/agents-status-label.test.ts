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
 * The agent list's status badges must use the shared display label from
 * agent-state-display.ts (e.g. "waiting on others" for the `blocked` activity), not
 * the raw status key (ptone/scion#1571).
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { LitElement } from 'lit';
import './agents.js';
import { stateManager } from '../../client/state.js';
import type { Agent } from '../../shared/types.js';

/** happy-dom has no EventSource; setScope opens one. */
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

const store = new Map<string, string>();

/** Stubbed per test: afterEach's vi.unstubAllGlobals() removes it. */
function stubStorage(): void {
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
}

const blockedAgent = {
  id: 'a1',
  name: 'a1',
  projectId: 'p1',
  template: 't',
  phase: 'running',
  activity: 'blocked',
  created: '2026-01-01T00:00:00Z',
  updated: '2026-01-01T00:00:00Z',
  messageMode: 'project',
  _capabilities: { actions: ['read'] },
} as Agent;

async function flush(): Promise<void> {
  // state.ts flushes after 100ms when no animation frame runs (happy-dom).
  await new Promise((r) => setTimeout(r, 150));
}

describe('scion-page-agents status badge label', () => {
  beforeEach(() => {
    stubStorage();
    vi.stubGlobal('EventSource', FakeEventSource);
    // Force a real scope change so the dashboard scope reloads.
    stateManager.setScope({ type: 'brokers-list' });
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ agents: [blockedAgent], _capabilities: { actions: [] } }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-page-agents').forEach((n) => n.remove());
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    store.clear();
  });

  for (const view of ['grid', 'list'] as const) {
    it(`renders a blocked agent as "waiting on others" in the ${view} view`, async () => {
      store.set('scion-view-agents', view);
      const el = document.createElement('scion-page-agents') as LitElement;
      document.body.appendChild(el);
      await el.updateComplete;
      await flush();
      await el.updateComplete;
      // Only the list view renders a table.
      expect(el.shadowRoot?.querySelector('table') !== null).toBe(view === 'list');

      const badges = Array.from(el.shadowRoot?.querySelectorAll('scion-status-badge') ?? []).filter(
        (b) => (b as unknown as { status: string }).status === 'blocked'
      ) as Array<LitElement & { label: string }>;
      expect(badges).toHaveLength(1);
      const badge = badges[0];
      await badge.updateComplete;
      expect(badge.label).toBe('waiting on others');
      const text = badge.shadowRoot?.textContent ?? '';
      expect(text).toContain('waiting on others');
      expect(text).not.toContain('blocked');
    });
  }
});
