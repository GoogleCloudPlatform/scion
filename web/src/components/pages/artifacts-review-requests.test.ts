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

import './artifacts.js';
import type { ScionPageArtifacts } from './artifacts.js';
import '../shared/artifact-list.js';
import type { ScionArtifactList } from '../shared/artifact-list.js';
import { ARTIFACTS_FLAG } from '../../client/artifacts.js';
import type { ArtifactListItem } from '../../client/artifacts.js';
import {
  ARTIFACTS_REVIEW_DRAFTS_FLAG,
  ARTIFACTS_REVIEW_REQUESTS_FLAG,
  resetServerFlagStateForTests,
} from '../../utils/feature-flags.js';

const ME = 'user-me';
const PROJECT_ID = '11111111-1111-1111-1111-111111111111';

function makeItem(id: string, overrides: Partial<ArtifactListItem> = {}): ArtifactListItem {
  return {
    id,
    ref: `scion://artifact/${id}`,
    scopeKind: 'project',
    scopeRef: PROJECT_ID,
    ownerKind: 'user',
    ownerRef: 'user-alice',
    title: `Artifact ${id}`,
    currentSeq: 1,
    createdAt: '2026-11-25T00:00:00Z',
    updatedAt: '2026-11-25T00:00:00Z',
    reviewOnLatest: false,
    reviewRequested: false,
    access: 'project',
    ...overrides,
  };
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function settle(el: HTMLElement & { updateComplete: Promise<boolean> }): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

describe('artifacts list & project artifact-list: review requests (P1)', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeEach(() => {
    resetServerFlagStateForTests();
    localStorage.clear();
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
    window.history.replaceState(null, '', '/artifacts');
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    document.body.innerHTML = '';
  });

  function mockList(items: ArtifactListItem[]): URL[] {
    const requestedUrls: URL[] = [];
    fetchMock.mockImplementation((input: string | URL) => {
      const urlStr = String(input);
      if (urlStr.startsWith('/api/v1/artifacts?')) {
        const u = new URL(urlStr, 'http://localhost');
        // Skip canPublishIn probe (limit=1 & scope=...)
        if (u.searchParams.get('limit') !== '1') {
          requestedUrls.push(u);
        }
        return Promise.resolve(jsonResponse({ artifacts: items }));
      }
      if (urlStr.startsWith('/api/v1/projects')) {
        return Promise.resolve(jsonResponse({ projects: [] }));
      }
      return Promise.resolve(jsonResponse({}));
    });
    return requestedUrls;
  }

  async function mountGlobal(opts: {
    search?: string;
    reviewRequestsOn: boolean;
  }): Promise<ScionPageArtifacts> {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_REQUESTS_FLAG]: opts.reviewRequestsOn,
    };
    window.history.replaceState(null, '', `/artifacts${opts.search ?? ''}`);
    const el = document.createElement('scion-page-artifacts') as ScionPageArtifacts;
    el.pageData = {
      path: '/artifacts',
      title: 'Artifacts',
      user: { id: ME, email: 'me@example.com', name: 'Me' },
    } as ScionPageArtifacts['pageData'];
    document.body.appendChild(el);
    await settle(el);
    return el;
  }

  it('hides Requested of me chip and ignores ?requested=1 when gate is off', async () => {
    const urls = mockList([makeItem('a1')]);
    const el = await mountGlobal({
      search: '?requested=1',
      reviewRequestsOn: false,
    });
    const chips = [...el.shadowRoot!.querySelectorAll('.filter-chip')];
    expect(chips.find((c) => c.textContent?.trim() === 'Requested of me')).toBeUndefined();
    expect(urls[0].searchParams.get('requested')).toBeNull();
    expect(urls[0].searchParams.get('q')).toBeNull();
  });

  it('shows primary "Review requested" badge, hydrates ?requested=1 into is:requested, and strips legacy param', async () => {
    const urls = mockList([makeItem('a1', { reviewRequested: true })]);
    const el = await mountGlobal({
      search: '?requested=1',
      reviewRequestsOn: true,
    });

    const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & { value: string };
    expect(box.value).toBe('is:requested');
    const afterParams = new URLSearchParams(window.location.search);
    expect(afterParams.get('q')).toBe('is:requested');
    expect(afterParams.get('requested')).toBeNull();
    expect(urls[0].searchParams.get('q')).toBe('is:requested');

    const badge = el.shadowRoot!.querySelector('tbody sl-badge[variant="primary"]');
    expect(badge?.textContent?.trim()).toBe('Review requested');

    const chip = [...el.shadowRoot!.querySelectorAll('.filter-chip')].find(
      (c) => c.textContent?.trim() === 'Requested of me'
    ) as HTMLButtonElement;
    expect(chip).toBeDefined();
    expect(chip.getAttribute('aria-pressed')).toBe('true');

    // Clicking the chip clears is:requested.
    chip.click();
    await settle(el);
    expect(box.value).toBe('');
    expect(chip.getAttribute('aria-pressed')).toBe('false');
  });

  it('renders "Review requested" badge and Requested of me chip in project artifact-list', async () => {
    const urls = mockList([makeItem('a1', { reviewRequested: true })]);
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_REQUESTS_FLAG]: true,
    };
    window.history.replaceState(null, '', `/projects/${PROJECT_ID}?requested=1`);

    const list = document.createElement('scion-artifact-list') as ScionArtifactList;
    list.projectId = PROJECT_ID;
    list.currentUserId = ME;
    document.body.appendChild(list);
    await settle(list);

    const badge = list.shadowRoot!.querySelector('tbody sl-badge[variant="primary"]');
    expect(badge?.textContent?.trim()).toBe('Review requested');

    const chip = [...list.shadowRoot!.querySelectorAll('.filter-chip')].find(
      (c) => c.textContent?.trim() === 'Requested of me'
    ) as HTMLButtonElement;
    expect(chip).toBeDefined();
    expect(chip.getAttribute('aria-pressed')).toBe('true');
    expect(urls[0].searchParams.get('q')).toBe('is:requested');
  });

  it('shows all three state chips (Review on latest, Requested of me, My drafts) when gates are on and hides gated ones when off across global and project lists', async () => {
    mockList([makeItem('a1')]);
    // Both gates on -> global list shows all three state chips.
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_REQUESTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    const globalEl = document.createElement('scion-page-artifacts') as ScionPageArtifacts;
    globalEl.pageData = {
      path: '/artifacts',
      title: 'Artifacts',
      user: { id: ME, email: 'me@example.com', name: 'Me' },
    } as ScionPageArtifacts['pageData'];
    document.body.appendChild(globalEl);
    await settle(globalEl);
    const globalLabels = [...globalEl.shadowRoot!.querySelectorAll('.filter-chip')].map((c) =>
      c.textContent?.trim()
    );
    expect(globalLabels).toContain('Review on latest');
    expect(globalLabels).toContain('Requested of me');
    expect(globalLabels).toContain('My drafts');
    globalEl.remove();

    // Both gates on -> project list shows all three state chips.
    const projOn = document.createElement('scion-artifact-list') as ScionArtifactList;
    projOn.projectId = PROJECT_ID;
    projOn.currentUserId = ME;
    document.body.appendChild(projOn);
    await settle(projOn);
    const projOnLabels = [...projOn.shadowRoot!.querySelectorAll('.filter-chip')].map((c) =>
      c.textContent?.trim()
    );
    expect(projOnLabels).toContain('Review on latest');
    expect(projOnLabels).toContain('Requested of me');
    expect(projOnLabels).toContain('My drafts');
    projOn.remove();

    // Both gates off -> project list keeps Review on latest and hides Requested of me and My drafts.
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_REQUESTS_FLAG]: false,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: false,
    };
    const projOff = document.createElement('scion-artifact-list') as ScionArtifactList;
    projOff.projectId = PROJECT_ID;
    projOff.currentUserId = ME;
    document.body.appendChild(projOff);
    await settle(projOff);
    const projOffLabels = [...projOff.shadowRoot!.querySelectorAll('.filter-chip')].map((c) =>
      c.textContent?.trim()
    );
    expect(projOffLabels).toContain('Review on latest');
    expect(projOffLabels).not.toContain('Requested of me');
    expect(projOffLabels).not.toContain('My drafts');
  });
});
