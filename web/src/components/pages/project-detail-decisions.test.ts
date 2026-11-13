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
 * Project detail header Decisions button & live badge (ptone/scion#7800, Mock P3-1).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { stateManager } from '../../client/state.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';
import {
  CHAT_CHOICES_FLAG,
  PROJECT_DECISIONS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';

class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
    setTimeout(() => {
      if (this.readyState === FakeEventSource.CLOSED) return;
      this.readyState = FakeEventSource.OPEN;
      this.onopen?.(new Event('open'));
    }, 0);
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

await import('./project-detail.js');
type ScionPageProjectDetail = import('./project-detail.js').ScionPageProjectDetail;

async function settle(el: ScionPageProjectDetail, ms = 10): Promise<void> {
  await new Promise((r) => setTimeout(r, ms));
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

describe('<scion-page-project-detail> Decisions button & badge (Mock P3-1)', () => {
  let decisionsUrls: string[] = [];
  let waitingOnYouCount = 3;

  beforeAll(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  beforeEach(() => {
    decisionsUrls = [];
    waitingOnYouCount = 3;
    resetHubProjectCapabilitiesCache();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();

    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL): Promise<Response> => {
        const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        if (url.pathname === '/api/v1/projects/p-1') {
          return jsonResponse({
            id: 'p-1',
            name: 'scion',
            slug: 'scion',
            _capabilities: { actions: ['read', 'update'] },
          });
        }
        if (url.pathname === '/api/v1/projects/p-1/agents') {
          return jsonResponse({
            agents: [],
            totalCount: 0,
            complete: true,
            stats: { total: 0, running: 0 },
            _capabilities: { actions: ['read'] },
          });
        }
        if (url.pathname === '/api/v1/projects') {
          return jsonResponse({ projects: [], _capabilities: { actions: [] } });
        }
        if (url.pathname === '/api/v1/projects/p-1/decisions') {
          decisionsUrls.push(url.pathname + url.search);
          return jsonResponse({
            counts: { waitingOnYou: waitingOnYouCount, waitingOnOthers: 1 },
            prompts: [],
            agents: [],
            answerers: [],
          });
        }
        if (url.pathname.endsWith('/metrics-summary') || url.pathname.endsWith('/session-metrics')) {
          return jsonResponse({ available: false });
        }
        return jsonResponse({}, 404);
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.unstubAllGlobals();
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  async function mount(): Promise<ScionPageProjectDetail> {
    const el = document.createElement('scion-page-project-detail');
    el.projectId = 'p-1';
    document.body.appendChild(el);
    await settle(el, 20);
    return el;
  }

  it('hides Decisions button and makes zero /decisions requests unless both flags are enabled', async () => {
    // Both off
    let el = await mount();
    expect(el.shadowRoot?.querySelector('[data-decisions-button]')).toBeNull();
    expect(decisionsUrls).toEqual([]);
    el.remove();

    // Only web.project_decisions on
    setFeatureFlag(PROJECT_DECISIONS_FLAG, true);
    setFeatureFlag(CHAT_CHOICES_FLAG, false);
    el = await mount();
    expect(el.shadowRoot?.querySelector('[data-decisions-button]')).toBeNull();
    expect(decisionsUrls).toEqual([]);
    el.remove();

    // Only web.chat_choices on
    setFeatureFlag(PROJECT_DECISIONS_FLAG, false);
    setFeatureFlag(CHAT_CHOICES_FLAG, true);
    el = await mount();
    expect(el.shadowRoot?.querySelector('[data-decisions-button]')).toBeNull();
    expect(decisionsUrls).toEqual([]);
  });

  it('renders Decisions button, fetches limit=0 count, hides badge at 0, and updates live on choice-updated', async () => {
    setFeatureFlag(PROJECT_DECISIONS_FLAG, true);
    setFeatureFlag(CHAT_CHOICES_FLAG, true);
    waitingOnYouCount = 3;

    const el = await mount();
    const link = el.shadowRoot?.querySelector<HTMLAnchorElement>('[data-decisions-link]');
    const btn = el.shadowRoot?.querySelector('[data-decisions-button]');
    expect(link?.getAttribute('href')).toBe('/projects/p-1/decisions');
    expect(btn).not.toBeNull();
    expect(decisionsUrls).toEqual(['/api/v1/projects/p-1/decisions?tab=pending&limit=0']);

    const badge = btn?.querySelector('sl-badge.decisions-badge');
    expect(badge?.textContent?.trim()).toBe('3');

    // Choice updated in another project is ignored
    stateManager.dispatchEvent(
      new CustomEvent('choice-updated', {
        detail: { data: { projectId: 'other-proj', messageId: 'm-other' } },
      })
    );
    await settle(el, 150);
    expect(decisionsUrls).toHaveLength(1);

    // Choice updated in this project decrements count to 1
    waitingOnYouCount = 1;
    stateManager.dispatchEvent(
      new CustomEvent('choice-updated', {
        detail: { data: { projectId: 'p-1', messageId: 'm-1' } },
      })
    );
    await settle(el, 150);
    expect(decisionsUrls).toHaveLength(2);
    expect(btn?.querySelector('sl-badge.decisions-badge')?.textContent?.trim()).toBe('1');

    // DM choice-updated (no projectId) decrements count to 0 -> badge hidden
    waitingOnYouCount = 0;
    stateManager.dispatchEvent(
      new CustomEvent('choice-updated', {
        detail: { data: { messageId: 'm-dm' } },
      })
    );
    await settle(el, 150);
    expect(decisionsUrls).toHaveLength(3);
    expect(btn?.querySelector('sl-badge.decisions-badge')).toBeNull();
  });
});
