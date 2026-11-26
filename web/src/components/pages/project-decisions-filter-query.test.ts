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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ScionFilterBox } from '../shared/filter-box/filter-box.js';
import type { ScionDecisionsFilterPanel } from '../shared/decisions-filter-panel.js';
import type { ScionPageProjectDecisions } from './project-decisions.js';
import './project-decisions.js';
import { resetServerFlagStateForTests, setServerFlags } from '../../utils/feature-flags.js';

const PROJECT_ID = '11111111-1111-1111-1111-111111111111';
const AGENT_1_ID = '22222222-2222-2222-2222-222222222222';
const AGENT_2_ID = '33333333-3333-3333-3333-333333333333';
const VIEWER_ID = 'user-alice';
const SAM1_ID = 'user-sam-1';
const SAM2_ID = 'user-sam-2';
const BOB_ID = 'user-bob';

function makeResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function defaultFacets() {
  return {
    agents: [
      { id: AGENT_1_ID, name: 'deploy-frontend' },
      { id: AGENT_2_ID, name: 'deploy-frontend', hint: 'deleted · 3f9c' },
    ],
    answerers: [
      { id: VIEWER_ID, name: 'Alice', hint: 'alice@example.com' },
      { id: BOB_ID, name: 'Bob', hint: 'bob@example.com' },
      { id: SAM1_ID, name: 'Sam Taylor', hint: 'sam1@example.com' },
      { id: SAM2_ID, name: 'Sam Taylor', hint: 'sam2@example.com' },
    ],
    conversations: [
      { key: 'topic-ops', label: '#ops', projectId: PROJECT_ID },
      { key: 'dm:agent:a:user:u', label: 'DM with deploy', projectId: PROJECT_ID },
    ],
  };
}

async function flushRender(el: ScionPageProjectDecisions): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
    await el.updateComplete;
    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel | null;
    if (panel) {
      await panel.updateComplete;
      const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox | null;
      if (box) await box.updateComplete;
    }
  }
}

describe('Project Decisions page — filters.query_dialect (ptone/scion#9346)', () => {
  let requestedUrls: string[];

  beforeEach(() => {
    requestedUrls = [];
    window.history.replaceState({}, '', `/projects/${PROJECT_ID}/decisions`);
    vi.stubGlobal(
      'EventSource',
      class {
        addEventListener(): void {}
        removeEventListener(): void {}
        close(): void {}
      }
    );
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        requestedUrls.push(url);
        if (url.includes(`/api/v1/projects/${PROJECT_ID}/decisions`)) {
          const parsed = new URL(url, 'http://localhost');
          const tab = parsed.searchParams.get('tab');
          const facets = defaultFacets();
          if (tab === 'made') {
            return Promise.resolve(
              makeResponse({
                counts: { waitingOnYou: 0, waitingOnOthers: 0 },
                entries: [],
                ...facets,
              })
            );
          }
          return Promise.resolve(
            makeResponse({
              counts: { waitingOnYou: 0, waitingOnOthers: 0 },
              prompts: [],
              ...facets,
            })
          );
        }
        if (url.includes(`/api/v1/projects/${PROJECT_ID}`)) {
          return Promise.resolve(makeResponse({ id: PROJECT_ID, name: 'Demo', slug: 'demo' }));
        }
        return Promise.resolve(makeResponse({}));
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete (window as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.unstubAllGlobals();
  });

  async function mountPage(search = ''): Promise<ScionPageProjectDecisions> {
    window.history.replaceState({}, '', `/projects/${PROJECT_ID}/decisions${search}`);
    const el = document.createElement('scion-page-project-decisions') as ScionPageProjectDecisions;
    el.projectId = PROJECT_ID;
    el.pageData = {
      path: `/projects/${PROJECT_ID}/decisions${search}`,
      user: { id: VIEWER_ID, email: 'alice@example.com', name: 'Alice' },
    } as ScionPageProjectDecisions['pageData'];
    document.body.appendChild(el);
    await flushRender(el);
    return el;
  }

  it('keeps legacy filter controls when filters.query_dialect is off', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': false });
    const el = await mountPage();

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel | null;
    expect(panel).not.toBeNull();
    expect(panel?.shadowRoot?.querySelector('scion-filter-box')).toBeNull();
    expect(panel?.shadowRoot?.querySelector('[data-filter-panel-toggle]')).not.toBeNull();
  });

  it('renders <scion-filter-box view="decisions"> with local suggestions, hints, and collision email fallback when on', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage('?tab=made');

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox | null;
    expect(box).not.toBeNull();
    expect(box?.view).toBe('decisions');
    expect(box?.placeholder).toBe('Filter decisions… (try agent:deploy by:me)');

    // No summary tag bar in dialect mode.
    expect(panel.shadowRoot?.querySelector('[data-filter-panel-toggle]')).toBeNull();

    // Local value suggestions for `agent:` preserve colliding names with distinct hints.
    const agentSugs = box?.valueSuggestions?.('agent', '');
    expect(agentSugs).toEqual([
      { value: 'deploy-frontend' },
      { value: 'deploy-frontend', help: 'deleted · 3f9c' },
    ]);

    // Local value suggestions for `by:` offer pinned `me`, unique display name with email hint,
    // and insert emails for colliding display names ("Sam Taylor").
    const bySugs = box?.valueSuggestions?.('by', '');
    expect(bySugs).toEqual([
      { value: 'me', help: 'You · Alice', pinned: true },
      { value: 'Bob', help: 'bob@example.com' },
      { value: 'sam1@example.com', help: 'Sam Taylor' },
      { value: 'sam2@example.com', help: 'Sam Taylor' },
    ]);

    // Date range disclosure is present on Made tab with From/To inputs.
    const toggle = panel.shadowRoot?.querySelector('[data-date-range-toggle]') as HTMLButtonElement;
    expect(toggle).not.toBeNull();
    toggle.click();
    await panel.updateComplete;
    expect(panel.shadowRoot?.querySelector('[data-filter="from"]')).not.toBeNull();
    expect(panel.shadowRoot?.querySelector('[data-filter="to"]')).not.toBeNull();
  });

  it('round-trips q= in URL and warns on Made-only keys on Pending tab without dropping them', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage('?q=agent%3Adeploy-frontend+by%3Ame');

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box.value).toBe('agent:deploy-frontend by:me');
    expect(box.externalWarning).toContain('Made tab only');

    // Commit a new query via filter-commit and verify URL + API request.
    requestedUrls = [];
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = '-agent:deploy-frontend';
    input.dispatchEvent(new Event('input'));
    await box.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await flushRender(el);

    expect(new URLSearchParams(window.location.search).get('q')).toBe('-agent:deploy-frontend');
    expect(requestedUrls.some((u) => u.includes('q=-agent%3Adeploy-frontend'))).toBe(true);
  });

  it('shows old URL params as filter terms on load (preserving colliding agent ID) and replaces them with q= on commit', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage(
      `?tab=made&agent=${AGENT_1_ID}&conversation=topic-ops&user=${VIEWER_ID}&provenance=direct`
    );

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    // AGENT_1_ID shares the name "deploy-frontend" with AGENT_2_ID, so O2 keeps the exact UUID.
    expect(box.value).toBe(`conversation:#ops agent:${AGENT_1_ID} by:me provenance:direct`);

    // Committing an unrelated edit keeps AGENT_1_ID rather than widening to deploy-frontend.
    requestedUrls = [];
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = `agent:${AGENT_1_ID} by:me provenance:confirmed`;
    input.dispatchEvent(new Event('input'));
    await box.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await flushRender(el);

    const params = new URLSearchParams(window.location.search);
    expect(params.get('q')).toBe(`agent:${AGENT_1_ID} by:me provenance:confirmed`);
    expect(params.get('agent')).toBeNull();
    expect(params.get('conversation')).toBeNull();
    expect(params.get('user')).toBeNull();
    expect(params.get('provenance')).toBeNull();
    expect(requestedUrls.some((u) => u.includes(AGENT_1_ID))).toBe(true);
  });

  it('loads the unfiltered list with no 400 request and keeps raw text in the box for invalid q= URLs (O1)', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const longText = 'x'.repeat(220);
    for (const badQ of ['a', 'provenance:bogus', 'agent:', longText]) {
      document.body.innerHTML = '';
      requestedUrls = [];
      const el = await mountPage(`?tab=made&q=${encodeURIComponent(badQ)}`);

      const panel = el.shadowRoot?.querySelector(
        'scion-decisions-filter-panel'
      ) as ScionDecisionsFilterPanel;
      const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
      expect(box.value).toBe(badQ);
      // Every decisions request must omit `q` so the Hub never returns 400.
      const decisionReqs = requestedUrls.filter((u) =>
        u.includes(`/api/v1/projects/${PROJECT_ID}/decisions`)
      );
      expect(decisionReqs.length).toBeGreaterThan(0);
      for (const reqUrl of decisionReqs) {
        expect(new URL(reqUrl, 'http://localhost').searchParams.get('q')).toBeNull();
      }
    }
  });

  it('pins `me` first in by: suggestions even with >12 answerers sorting before "me" (O3)', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const manyAnswerers = [
      { id: VIEWER_ID, name: 'Alice', hint: 'alice@example.com' },
      ...Array.from({ length: 15 }, (_, i) => ({
        id: `user-a-${i}`,
        name: `Ans ${String(i).padStart(2, '0')}`,
        hint: `ans${i}@example.com`,
      })),
    ];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (url.includes(`/api/v1/projects/${PROJECT_ID}/decisions`)) {
          return Promise.resolve(
            makeResponse({
              counts: { waitingOnYou: 0, waitingOnOthers: 0 },
              prompts: [],
              entries: [],
              agents: [],
              answerers: manyAnswerers,
              conversations: [],
            })
          );
        }
        return Promise.resolve(makeResponse({ id: PROJECT_ID, name: 'Demo', slug: 'demo' }));
      })
    );

    const el = await mountPage('?tab=made');
    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'by:';
    input.setSelectionRange(3, 3);
    input.dispatchEvent(new Event('input'));
    await box.updateComplete;

    const options = Array.from(box.shadowRoot?.querySelectorAll('[role="option"]') ?? []);
    expect(options.length).toBe(12);
    expect(options[0].querySelector('.option-label')?.textContent).toBe('me');
    expect(options[0].querySelector('.option-help')?.textContent).toBe('You · Alice');
  });
});
