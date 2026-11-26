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
import type { ScionPageProfileDecisions } from './profile-decisions.js';
import './profile-decisions.js';
import { resetProfileDecisionProbeCacheForTests } from '../../client/profile-decisions.js';
import { resetServerFlagStateForTests, setServerFlags } from '../../utils/feature-flags.js';

const PROJ_ALPHA_ID = '11111111-1111-1111-1111-111111111111';
const PROJ_BETA_ID = '44444444-4444-4444-4444-444444444444';
const AGENT_1_ID = '22222222-2222-2222-2222-222222222222';
const AGENT_2_ID = '33333333-3333-3333-3333-333333333333';
const VIEWER_ID = 'user-alice';
const BOB_ID = 'user-bob';

function makeResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function defaultProfileFacets(): {
  projects: { id: string; name: string; slug: string }[];
  agents: { id: string; name: string; projectId: string }[];
  answerers: { id: string; name: string; hint: string }[];
  conversations: { key: string; label: string; projectId: string }[];
  truncatedProjects: boolean;
} {
  return {
    projects: [
      { id: PROJ_ALPHA_ID, name: 'Alpha App', slug: 'alpha-app' },
      { id: PROJ_BETA_ID, name: 'beta-svc', slug: 'beta-svc' },
    ],
    agents: [
      { id: AGENT_1_ID, name: 'deploy-frontend', projectId: PROJ_ALPHA_ID },
      { id: AGENT_2_ID, name: 'beta-bot', projectId: PROJ_BETA_ID },
    ],
    answerers: [
      { id: VIEWER_ID, name: 'Alice', hint: 'alice@example.com' },
      { id: BOB_ID, name: 'Bob', hint: 'bob@example.com' },
    ],
    conversations: [
      { key: 'topic-ops', label: '#ops', projectId: PROJ_ALPHA_ID },
      { key: 'topic-beta', label: '#beta', projectId: PROJ_BETA_ID },
    ],
    truncatedProjects: false,
  };
}

async function flushRender(el: ScionPageProfileDecisions): Promise<void> {
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

describe('Profile Decisions page — filters.query_dialect (ptone/scion#9348)', () => {
  let requestedUrls: string[];

  beforeEach(() => {
    requestedUrls = [];
    resetProfileDecisionProbeCacheForTests();
    window.history.replaceState({}, '', '/profile/decisions');
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
        if (url.includes('/api/v1/users/me/decisions')) {
          const parsed = new URL(url, 'http://localhost');
          const tab = parsed.searchParams.get('tab');
          const sort = parsed.searchParams.get('sort');
          const facets = defaultProfileFacets();
          if (tab === 'made') {
            if (sort === 'project') {
              return Promise.resolve(
                makeResponse({
                  counts: { waitingOnYou: 0, waitingOnOthers: 0 },
                  groups: [
                    {
                      project: { id: PROJ_ALPHA_ID, name: 'Alpha App', slug: 'alpha-app' },
                      entries: [],
                      more: true,
                    },
                  ],
                  ...facets,
                })
              );
            }
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
        return Promise.resolve(makeResponse({}));
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete (window as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__;
    resetServerFlagStateForTests();
    resetProfileDecisionProbeCacheForTests();
    vi.unstubAllGlobals();
  });

  async function mountPage(search = ''): Promise<ScionPageProfileDecisions> {
    window.history.replaceState({}, '', `/profile/decisions${search}`);
    const el = document.createElement('scion-page-profile-decisions') as ScionPageProfileDecisions;
    el.pageData = {
      path: `/profile/decisions${search}`,
      user: { id: VIEWER_ID, email: 'alice@example.com', name: 'Alice' },
    } as ScionPageProfileDecisions['pageData'];
    document.body.appendChild(el);
    await flushRender(el);
    return el;
  }

  it('keeps legacy filter controls and Project combobox when filters.query_dialect is off', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': false });
    const el = await mountPage();

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel | null;
    expect(panel).not.toBeNull();
    expect(panel?.shadowRoot?.querySelector('scion-filter-box')).toBeNull();
    expect(panel?.shadowRoot?.querySelector('[data-filter-panel-toggle]')).not.toBeNull();
    expect(panel?.querySelector('[data-filter="project"]')).not.toBeNull();
  });

  it('renders <scion-filter-box view="profile-decisions"> with project: suggestions and From/To disclosure when on', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage('?tab=made');

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox | null;
    expect(box).not.toBeNull();
    expect(box?.view).toBe('profile-decisions');
    expect(box?.placeholder).toBe('Filter decisions… (try agent:deploy by:me)');

    // Sort bar is slotted into dialect-leading; legacy summary bar and project combobox are absent.
    expect(panel.querySelector('[data-sort-bar][slot="dialect-leading"]')).not.toBeNull();
    expect(panel.querySelector('[data-filter="project"]')).toBeNull();
    expect(panel.shadowRoot?.querySelector('[data-filter-panel-toggle]')).toBeNull();

    // Local value suggestions for `project:` use slug with display name hint when different.
    const projectSugs = box?.valueSuggestions?.('project', '');
    expect(projectSugs).toEqual([{ value: 'alpha-app', help: 'Alpha App' }, { value: 'beta-svc' }]);

    // Agent and conversation suggestions carry project display name hints.
    const agentSugs = box?.valueSuggestions?.('agent', '');
    expect(agentSugs).toEqual([
      { value: 'deploy-frontend', help: 'Alpha App' },
      { value: 'beta-bot', help: 'beta-svc' },
    ]);

    // `by:` suggestions pin `me` first.
    const bySugs = box?.valueSuggestions?.('by', '');
    expect(bySugs?.[0]).toEqual({ value: 'me', help: 'You · Alice', pinned: true });

    // Date range disclosure is present on Made tab with From/To inputs.
    const toggle = panel.shadowRoot?.querySelector('[data-date-range-toggle]') as HTMLButtonElement;
    expect(toggle).not.toBeNull();
    toggle.click();
    await panel.updateComplete;
    expect(panel.shadowRoot?.querySelector('[data-filter="from"]')).not.toBeNull();
    expect(panel.shadowRoot?.querySelector('[data-filter="to"]')).not.toBeNull();
  });

  it('round-trips q= with project: in URL and warns on Made-only keys on Pending tab', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage('?q=project%3Aalpha-app+by%3Ame');

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box.value).toBe('project:alpha-app by:me');
    expect(box.externalWarning).toContain('Made tab only');

    // Commit a new query via filter-commit and verify URL + API request.
    requestedUrls = [];
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'project:alpha-app,beta-svc -agent:beta-bot';
    input.dispatchEvent(new Event('input'));
    await box.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await flushRender(el);

    expect(new URLSearchParams(window.location.search).get('q')).toBe(
      '-agent:beta-bot project:alpha-app,beta-svc'
    );
    expect(
      requestedUrls.some((u) => u.includes('q=-agent%3Abeta-bot+project%3Aalpha-app%2Cbeta-svc'))
    ).toBe(true);
  });

  it('shows old URL params (including project=) as filter terms on load and replaces them with q= on commit', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage(
      `?tab=made&project=${PROJ_ALPHA_ID}&agent=${AGENT_1_ID}&conversation=topic-ops&user=${VIEWER_ID}&provenance=direct`
    );

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box.value).toBe(
      'project:alpha-app conversation:#ops agent:deploy-frontend by:me provenance:direct'
    );

    // Committing replaces all legacy separate params (including project=) with q=.
    const input = box.shadowRoot?.querySelector('input') as HTMLInputElement;
    input.value = 'project:beta-svc by:me';
    input.dispatchEvent(new Event('input'));
    await box.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await flushRender(el);

    const params = new URLSearchParams(window.location.search);
    expect(params.get('q')).toBe('by:me project:beta-svc');
    expect(params.get('project')).toBeNull();
    expect(params.get('agent')).toBeNull();
    expect(params.get('conversation')).toBeNull();
    expect(params.get('user')).toBeNull();
    expect(params.get('provenance')).toBeNull();
  });

  it('drilling down a project on Made sort=project writes project:<slug> into q=', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage('?tab=made&sort=project');

    const drillLink = el.shadowRoot?.querySelector(
      `[data-see-all-project="${PROJ_ALPHA_ID}"]`
    ) as HTMLAnchorElement | null;
    expect(drillLink).not.toBeNull();
    drillLink?.click();
    await flushRender(el);

    const params = new URLSearchParams(window.location.search);
    expect(params.get('sort')).toBe('recent');
    expect(params.get('q')).toBe('project:alpha-app');
    expect(params.get('project')).toBeNull();
  });

  it('drill-down replaces positive project: terms, keeps negations and other terms, and matches href (O2)', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });

    // Case 1: existing positive OR project:alpha-app,beta-svc -> replaced by single project:alpha-app
    const el1 = await mountPage('?tab=made&sort=project&q=project%3Aalpha-app%2Cbeta-svc');
    const link1 = el1.shadowRoot?.querySelector(
      `[data-see-all-project="${PROJ_ALPHA_ID}"]`
    ) as HTMLAnchorElement | null;
    expect(link1).not.toBeNull();
    const href1 = link1?.getAttribute('href') ?? '';
    link1?.click();
    await flushRender(el1);
    expect(`?${window.location.search.replace(/^\?/, '')}`).toBe(href1);
    const params1 = new URLSearchParams(window.location.search);
    expect(params1.get('q')).toBe('project:alpha-app');
    expect(params1.get('project')).toBeNull();
    const panel1 = el1.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box1 = panel1.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box1.shadowRoot?.querySelector('.feedback.warning')).toBeNull();
    el1.remove();

    // Case 2: negated -project:gamma + agent:x -> keeps both and adds single positive project:alpha-app
    const el2 = await mountPage('?tab=made&sort=project&q=-project%3Agamma+agent%3Ax');
    const link2 = el2.shadowRoot?.querySelector(
      `[data-see-all-project="${PROJ_ALPHA_ID}"]`
    ) as HTMLAnchorElement | null;
    expect(link2).not.toBeNull();
    const href2 = link2?.getAttribute('href') ?? '';
    link2?.click();
    await flushRender(el2);
    expect(`?${window.location.search.replace(/^\?/, '')}`).toBe(href2);
    const params2 = new URLSearchParams(window.location.search);
    expect(params2.get('q')).toBe('agent:x project:alpha-app -project:gamma');
    expect(params2.get('project')).toBeNull();
    const panel2 = el2.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box2 = panel2.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box2.shadowRoot?.querySelector('.feedback.warning')).toBeNull();
  });

  it('shows invalid profile q= URL inline without sending a 400 API request', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage('?tab=made&q=project%3Aa+OR+project%3Ab');

    // No API request should include the invalid q= param.
    expect(requestedUrls.some((u) => u.includes('q='))).toBe(false);

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box.value).toBe('project:a OR project:b');
    expect(box.shadowRoot?.querySelector('.feedback.error')).not.toBeNull();
  });

  it('clears the filter box for sub-minimum plain text q=a on profile load without sending a 400 request', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const el = await mountPage('?tab=made&q=a');

    expect(requestedUrls.some((u) => u.includes('q='))).toBe(false);

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box.value).toBe('');
  });

  it('keeps exact project ID for colliding or absent legacy project= param', async () => {
    setServerFlags({ 'web.decisions': true, 'filters.query_dialect': true });
    const unknownProjId = '99999999-9999-9999-9999-999999999999';
    const el = await mountPage(`?tab=made&project=${unknownProjId}`);

    const panel = el.shadowRoot?.querySelector(
      'scion-decisions-filter-panel'
    ) as ScionDecisionsFilterPanel;
    const box = panel.shadowRoot?.querySelector('scion-filter-box') as ScionFilterBox;
    expect(box.value).toBe(`project:${unknownProjId}`);
  });
});
