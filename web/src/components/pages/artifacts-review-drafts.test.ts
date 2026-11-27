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
    createdAt: '2026-11-26T00:00:00Z',
    updatedAt: '2026-11-26T00:00:00Z',
    reviewPending: false,
    reviewInDraft: false,
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

describe('artifacts list & project artifact-list: review drafts (P2c)', () => {
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
        requestedUrls.push(u);
        return Promise.resolve(jsonResponse({ artifacts: items }));
      }
      if (urlStr.startsWith('/api/v1/projects')) {
        return Promise.resolve(jsonResponse({ projects: [] }));
      }
      return Promise.resolve(jsonResponse({}));
    });
    return requestedUrls;
  }

  it('renders neutral Review in draft badge and My drafts chip when drafts gate is on', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    const urls = mockList([makeItem('a1', { reviewInDraft: true }), makeItem('a2')]);

    const el = document.createElement('scion-page-artifacts') as ScionPageArtifacts;
    el.pageData = { user: { id: ME, email: 'me@test', name: 'Me', role: 'member' } } as never;
    document.body.appendChild(el);
    await settle(el);

    const badges = Array.from(el.shadowRoot!.querySelectorAll('sl-badge')).filter((b) =>
      b.textContent?.includes('Review in draft')
    );
    expect(badges).toHaveLength(1);
    expect(badges[0].getAttribute('variant')).toBe('neutral');

    const chip = Array.from(el.shadowRoot!.querySelectorAll('button.filter-chip')).find((b) =>
      b.textContent?.includes('My drafts')
    ) as HTMLButtonElement | undefined;
    expect(chip).toBeDefined();
    expect(chip!.getAttribute('aria-pressed')).toBe('false');

    chip!.click();
    await settle(el);
    expect(chip!.getAttribute('aria-pressed')).toBe('true');
    expect(urls.some((u) => u.searchParams.get('q') === 'is:draft')).toBe(true);
  });

  it('hides My drafts chip and ignores drafts=1 hydration when drafts gate is off', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: false,
    };
    window.history.replaceState(null, '', '/artifacts?drafts=1');
    const urls = mockList([makeItem('a1')]);

    const el = document.createElement('scion-page-artifacts') as ScionPageArtifacts;
    el.pageData = { user: { id: ME, email: 'me@test', name: 'Me', role: 'member' } } as never;
    document.body.appendChild(el);
    await settle(el);

    const chip = Array.from(el.shadowRoot!.querySelectorAll('button.filter-chip')).find((b) =>
      b.textContent?.includes('My drafts')
    );
    expect(chip).toBeUndefined();
    expect(urls.some((u) => u.searchParams.get('q')?.includes('is:draft'))).toBe(false);
  });

  it('hydrates drafts=1 link into is:draft on global list when drafts gate is on', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    window.history.replaceState(null, '', '/artifacts?drafts=1');
    const urls = mockList([makeItem('a1', { reviewInDraft: true })]);

    const el = document.createElement('scion-page-artifacts') as ScionPageArtifacts;
    el.pageData = { user: { id: ME, email: 'me@test', name: 'Me', role: 'member' } } as never;
    document.body.appendChild(el);
    await settle(el);

    expect(urls.some((u) => u.searchParams.get('q') === 'is:draft')).toBe(true);
    expect(window.location.search).toContain('q=is%3Adraft');
    expect(window.location.search).not.toContain('drafts=1');
  });

  it('renders Review in draft badge, My drafts chip, and hydrates drafts=1 on project artifact-list', async () => {
    window.__SCION_FEATURES__ = {
      [ARTIFACTS_FLAG]: true,
      [ARTIFACTS_REVIEW_DRAFTS_FLAG]: true,
    };
    window.history.replaceState(null, '', `/projects/${PROJECT_ID}?tab=artifacts&drafts=1`);
    const urls = mockList([makeItem('a1', { reviewInDraft: true })]);

    const el = document.createElement('scion-artifact-list') as ScionArtifactList;
    el.projectId = PROJECT_ID;
    el.currentUserId = ME;
    document.body.appendChild(el);
    await settle(el);

    expect(urls.some((u) => u.searchParams.get('q') === 'is:draft')).toBe(true);

    const badges = Array.from(el.shadowRoot!.querySelectorAll('sl-badge')).filter((b) =>
      b.textContent?.includes('Review in draft')
    );
    expect(badges).toHaveLength(1);
    expect(badges[0].getAttribute('variant')).toBe('neutral');

    const chip = Array.from(el.shadowRoot!.querySelectorAll('button.filter-chip')).find((b) =>
      b.textContent?.includes('My drafts')
    );
    expect(chip).toBeDefined();
    expect(chip!.getAttribute('aria-pressed')).toBe('true');
  });
});
