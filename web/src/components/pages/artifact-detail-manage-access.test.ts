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
 * Artifact page, Manage access (experiment web.artifact_manage_access,
 * ptone/scion#7107, approved mock v1): with the experiment on, one Manage
 * access button, shown exactly for canShare or canGrantReview, replaces
 * Share, Grant review access and Retention…, and opens one dialog in
 * manage mode with the page's capabilities. Move… and Delete… are
 * unchanged. With it off, the page is as before.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ArtifactResponse } from '../../client/artifacts.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import type { ScionArtifactShareDialog } from '../shared/artifact-share-dialog.js';
import { resetPrincipalNames } from '../../client/principal-names.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

const { showToast } = vi.hoisted(() => ({ showToast: vi.fn() }));
vi.mock('../../utils/toast.js', () => ({ showToast }));

const ID = '5f1c2d3e-0000-4000-8000-0000000000bb';
const AGENT = '9d0e1f2a-3b4c-4d5e-8f6a-7b8c9d0e1f2b';

function meta(
  extra: Partial<ArtifactResponse> = {},
  art: Record<string, unknown> = {}
): ArtifactResponse {
  return {
    artifact: {
      id: ID,
      ref: `scion://artifact/${ID}`,
      scopeKind: 'project',
      scopeRef: 'p-1',
      ownerKind: 'agent',
      ownerRef: AGENT,
      title: 'Q3 rollout plan',
      currentSeq: 1,
      createdAt: '2026-10-05T12:00:00Z',
      updatedAt: '2026-10-05T12:00:00Z',
      ...art,
    },
    version: {
      seq: 1,
      ref: `scion://artifact/${ID}@1`,
      kind: 'publish',
      entryPath: 'notes.md',
      totalBytes: 5,
      fileCount: 1,
      createdAt: '2026-10-05T12:00:00Z',
      state: 'ready',
      files: [{ path: 'notes.md', size: 5, sha256: 'ab', mediaType: 'text/markdown' }],
    },
    ...extra,
  };
}

/** Mocks the hub: the artifact GET answers m; cross-project sharing is on. */
function mockFetch(m: ArtifactResponse): string[] {
  const urls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      urls.push(method === 'GET' ? url : `${method} ${url}`);
      const json = (body: unknown, status = 200): Promise<Response> =>
        Promise.resolve(new Response(JSON.stringify(body), { status }));
      if (url.endsWith('/links')) return json({ links: [] });
      if (url.endsWith('/grants')) return json({ grants: [], crossProjectSharing: true });
      if (/\/versions(\?|$)/.test(url)) {
        return json({ versions: [{ ...m.version, files: [] }], discarded: [] });
      }
      if (url.includes('/files/')) return Promise.resolve(new Response('# Hi', { status: 200 }));
      if (url.startsWith('/api/v1/agents/')) return json({ error: { code: 'not_found' } }, 404);
      if (url.startsWith(`/api/v1/artifacts/${ID}`)) return json(m);
      return json({ error: { code: 'not_found' } }, 404);
    })
  );
  return urls;
}

async function settle(el: ScionPageArtifactDetail): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(flagOn: boolean): Promise<ScionPageArtifactDetail> {
  window.__SCION_FEATURES__ = {
    'hub.artifacts': true,
    'web.artifact_manage_access': flagOn,
  };
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.pageData = { path: `/projects/p-1/artifacts/${ID}` } as ScionPageArtifactDetail['pageData'];
  document.body.appendChild(el);
  await settle(el);
  return el;
}

function buttons(el: ScionPageArtifactDetail): string[] {
  return Array.from(el.shadowRoot!.querySelectorAll('.header-actions sl-button')).map((b) =>
    b.textContent!.replace(/\s+/g, ' ').trim()
  );
}

function dialogs(el: ScionPageArtifactDetail): ScionArtifactShareDialog[] {
  return Array.from(el.shadowRoot!.querySelectorAll('scion-artifact-share-dialog'));
}

/** The access and retirement buttons of the header, in order. */
const ACCESS_LABELS = [
  'Manage access',
  'Share',
  'Grant review access',
  'Retention…',
  'Move…',
  'Delete…',
];
function accessButtons(el: ScionPageArtifactDetail): string[] {
  return buttons(el).filter((b) => ACCESS_LABELS.includes(b));
}

/** The roles of the design's matrix (section 4), with the flags the hub returns for each. */
const ROLES: Array<{
  role: string;
  flags: Partial<ArtifactResponse>;
  art?: Record<string, unknown>;
  on: string[];
}> = [
  {
    role: 'R1 owner',
    flags: { canManage: true, canShare: true, canRetire: true },
    art: { ownerKind: 'user', ownerRef: 'u-1' },
    on: ['Manage access', 'Move…', 'Delete…'],
  },
  {
    role: 'R2 admin grantee',
    flags: { canManage: true, canShare: true, canRetire: true },
    on: ['Manage access', 'Move…', 'Delete…'],
  },
  {
    role: 'R3 steward, project owner',
    flags: {
      canShare: true,
      shareAuthority: 'project_owner',
      canGrantReview: true,
      canRetire: true,
    },
    on: ['Manage access', 'Delete…'],
  },
  {
    role: 'R4 steward, delegator',
    flags: { canShare: true, shareAuthority: 'delegator', canGrantReview: true, canRetire: true },
    on: ['Manage access', 'Delete…'],
  },
  {
    role: 'R5 project admin, not owner',
    flags: { canGrantReview: true, canRetire: true },
    on: ['Manage access', 'Delete…'],
  },
  {
    role: 'R5u custodian only',
    flags: { canRetire: true },
    art: { ownerKind: 'user', ownerRef: 'u-1' },
    on: ['Delete…'],
  },
  { role: 'R6 delegator, review only', flags: { canGrantReview: true }, on: ['Manage access'] },
  { role: 'R7 reviewer', flags: { canReview: true }, on: [] },
  { role: 'R8 viewer', flags: {}, on: [] },
];

describe('artifact page: Manage access (web.artifact_manage_access)', () => {
  beforeAll(async () => {
    const hd = (
      window as unknown as { happyDOM?: { settings: { disableIframePageLoading: boolean } } }
    ).happyDOM;
    if (hd) hd.settings.disableIframePageLoading = true;
    await import('./artifact-detail.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    showToast.mockReset();
    resetPrincipalNames();
    delete (window as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
  });

  for (const r of ROLES) {
    it(`flag on, ${r.role}: ${r.on.length ? r.on.join(', ') : 'no access buttons'}`, async () => {
      mockFetch(meta(r.flags, r.art));
      const el = await mount(true);
      expect(accessButtons(el)).toEqual(r.on);
      const shown = r.on.includes('Manage access');
      const ds = dialogs(el);
      expect(ds).toHaveLength(shown ? 1 : 0);
      if (shown) {
        expect(ds[0]!.classList.contains('access-dialog')).toBe(true);
        expect(ds[0]!.mode).toBe('manage');
      }
    });
  }

  it('flag on: the one dialog receives the capabilities and opens from Manage access', async () => {
    mockFetch(
      meta({
        canShare: true,
        shareAuthority: 'delegator',
        canGrantReview: true,
        canRetire: true,
      })
    );
    const el = await mount(true);
    const [d] = dialogs(el);
    expect(d!.capabilities).toEqual({
      canManage: false,
      canShare: true,
      canGrantReview: true,
      shareAuthority: 'delegator',
    });
    expect(d!.open).toBe(false);
    const b = el.shadowRoot!.querySelector(
      '.header-actions sl-button.manage-access'
    ) as HTMLElement;
    expect(b.querySelector('sl-icon')!.getAttribute('name')).toBe('shield-lock');
    b.click();
    await settle(el);
    expect(d!.open).toBe(true);
    d!.dispatchEvent(new CustomEvent('artifact-share-closed', { bubbles: true, composed: true }));
    await settle(el);
    expect(d!.open).toBe(false);
  });

  it('flag off: Manage access is absent and the earlier buttons stay', async () => {
    mockFetch(
      meta({
        canShare: true,
        shareAuthority: 'project_owner',
        canGrantReview: true,
        canRetire: true,
      })
    );
    const el = await mount(false);
    expect(accessButtons(el)).toEqual(['Grant review access', 'Retention…', 'Delete…']);
    expect(el.shadowRoot!.querySelector('.header-actions sl-button.manage-access')).toBeNull();
    expect(el.shadowRoot!.querySelector('scion-artifact-share-dialog.access-dialog')).toBeNull();
    expect(dialogs(el).map((d) => d.mode)).toEqual(['review', 'retention']);
  });

  it('flag off: an owner keeps Share', async () => {
    mockFetch(meta({ canManage: true, canShare: true, canRetire: true }, { ownerKind: 'user' }));
    const el = await mount(false);
    expect(accessButtons(el)).toEqual(['Share', 'Move…', 'Delete…']);
    expect(dialogs(el).map((d) => d.mode)).toEqual(['share']);
  });
});
