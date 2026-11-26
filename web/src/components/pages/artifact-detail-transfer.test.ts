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
 * Artifact detail page ownership transfer UI tests (experiment
 * hub.artifacts_transfer): visibility gating on canTransfer, Transfer… button
 * and dialog wiring, state refresh after transfer, and "Transferred from"
 * header metadata.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { Artifact, ArtifactResponse, TransferCandidate } from '../../client/artifacts.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import { resetPrincipalNames } from '../../client/principal-names.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

const { showToast } = vi.hoisted(() => ({ showToast: vi.fn() }));
vi.mock('../../utils/toast.js', () => ({ showToast }));

const ID = '5f1c2d3e-0000-4000-8000-0000000000cc';
const OLD_AGENT = '9d0e1f2a-3b4c-4d5e-8f6a-7b8c9d0e1f2a';
const NEW_AGENT = '1a2b3c4d-5e6f-4789-8abc-def012345678';

function meta(
  extra: Partial<ArtifactResponse> = {},
  art: Partial<Artifact> = {}
): ArtifactResponse {
  return {
    artifact: {
      id: ID,
      ref: `scion://artifact/${ID}`,
      scopeKind: 'project',
      scopeRef: 'p-1',
      ownerKind: 'agent',
      ownerRef: OLD_AGENT,
      title: 'Release plan',
      currentSeq: 1,
      createdAt: '2026-11-20T12:00:00Z',
      updatedAt: '2026-11-25T12:00:00Z',
      ownerDeleted: true,
      ...art,
    },
    version: {
      seq: 1,
      ref: `scion://artifact/${ID}@1`,
      kind: 'publish',
      entryPath: 'plan.md',
      totalBytes: 6,
      fileCount: 1,
      createdAt: '2026-11-20T12:00:00Z',
      state: 'ready',
      files: [{ path: 'plan.md', size: 6, sha256: 'ab', mediaType: 'text/markdown' }],
    },
    ...extra,
  };
}

interface MockOpts {
  initial: ArtifactResponse;
  targets?: TransferCandidate[];
  afterTransfer?: ArtifactResponse;
  agentNames?: Record<string, string>;
}

function mockFetch(opts: MockOpts): {
  calls: Array<{ method: string; url: string; body: unknown }>;
} {
  const calls: Array<{ method: string; url: string; body: unknown }> = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      calls.push({ method, url, body });
      const json = (payload: unknown, status = 200): Promise<Response> =>
        Promise.resolve(
          new Response(JSON.stringify(payload), {
            status,
            headers: { 'Content-Type': 'application/json' },
          })
        );

      if (url === `/api/v1/artifacts/${ID}/transfer-targets` && method === 'GET') {
        return json({
          agents: opts.targets ?? [
            { id: NEW_AGENT, slug: 'release-bot', name: 'Release Bot', phase: 'running' },
          ],
          truncated: false,
        });
      }
      if (url === `/api/v1/artifacts/${ID}/transfer` && method === 'POST') {
        const updated =
          opts.afterTransfer ??
          meta(
            { canTransfer: false, canShare: true, canRetire: true },
            {
              ownerRef: NEW_AGENT,
              ownerDeleted: false,
              formerOwnerKind: 'agent',
              formerOwnerRef: OLD_AGENT,
              transferredAt: '2026-11-26T04:15:00Z',
            }
          );
        return json({ artifact: updated, notified: { newOwner: true, formerOwner: false } });
      }
      if (url.endsWith('/links')) return json({ links: [] });
      if (url.endsWith('/grants')) return json({ grants: [], crossProjectSharing: false });
      if (/\/versions(\?|$)/.test(url)) {
        return json({ versions: [{ ...opts.initial.version, files: [] }], discarded: [] });
      }
      if (url.includes('/files/')) return Promise.resolve(new Response('# Plan', { status: 200 }));
      if (url.startsWith('/api/v1/agents/')) {
        const agentId = url.slice('/api/v1/agents/'.length);
        const name = opts.agentNames?.[agentId];
        if (name) return json({ id: agentId, name, slug: name.toLowerCase() });
        return json({ error: { code: 'not_found' } }, 404);
      }
      if (url.startsWith(`/api/v1/artifacts/${ID}`)) return json(opts.initial);
      return json({ error: { code: 'not_found' } }, 404);
    })
  );
  return { calls };
}

async function settle(el: ScionPageArtifactDetail): Promise<void> {
  for (let i = 0; i < 25; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(): Promise<ScionPageArtifactDetail> {
  window.__SCION_FEATURES__ = { 'hub.artifacts': true };
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

function headerButton(el: ScionPageArtifactDetail, text: string): HTMLElement {
  const b = Array.from(el.shadowRoot!.querySelectorAll('.header-actions sl-button')).find(
    (x) => x.textContent!.trim() === text
  );
  if (!b) throw new Error(`no button ${text}`);
  return b as HTMLElement;
}

describe('artifact detail page: ownership transfer', () => {
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
  });

  it('hides Transfer… and transfer dialog when canTransfer is false or absent', async () => {
    mockFetch({ initial: meta({ canTransfer: false, canShare: true, canRetire: true }) });
    const el = await mount();
    expect(buttons(el)).not.toContain('Transfer…');
    expect(el.shadowRoot!.querySelector('scion-artifact-transfer-dialog')).toBeNull();
  });

  it('shows Transfer… when canTransfer is true, transfers ownership, and refreshes header state', async () => {
    const { calls } = mockFetch({
      initial: meta({
        canTransfer: true,
        canShare: true,
        shareAuthority: 'project_owner',
        canRetire: true,
      }),
      agentNames: {
        [NEW_AGENT]: 'Release Bot',
      },
    });
    const el = await mount();
    expect(buttons(el)).toContain('Transfer…');
    expect(el.shadowRoot!.querySelector('sl-badge.owner-deleted')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.meta .transferred-from')).toBeNull();

    headerButton(el, 'Transfer…').click();
    await settle(el);

    const dialog = el.shadowRoot!.querySelector('scion-artifact-transfer-dialog') as HTMLElement & {
      open: boolean;
    };
    expect(dialog).not.toBeNull();
    expect(dialog.open).toBe(true);

    const select = dialog.shadowRoot!.querySelector('sl-select') as HTMLElement & { value: string };
    select.value = NEW_AGENT;
    select.dispatchEvent(new Event('sl-change'));
    await settle(el);

    const confirmBtn = Array.from(dialog.shadowRoot!.querySelectorAll('sl-button')).find(
      (b) => b.textContent!.trim() === 'Transfer ownership'
    ) as HTMLElement;
    confirmBtn.click();
    await settle(el);

    expect(
      calls.some(
        (c) =>
          c.method === 'POST' &&
          c.url === `/api/v1/artifacts/${ID}/transfer` &&
          (c.body as { to: string }).to === `agent:${NEW_AGENT}`
      )
    ).toBe(true);
    expect(showToast).toHaveBeenCalledWith('Transferred ownership of “Release plan”.', 'success');

    // Header state updated from recomputed ArtifactResponse.
    expect(el.shadowRoot!.querySelector('sl-badge.owner-deleted')).toBeNull();
    expect(el.shadowRoot!.querySelector('.meta')!.textContent).toContain(
      'Owner: Release Bot (agent)'
    );
    const transferredFrom = el.shadowRoot!.querySelector('.meta .transferred-from');
    expect(transferredFrom).not.toBeNull();
    expect(transferredFrom!.textContent!.replace(/\s+/g, ' ').trim()).toMatch(
      /^Transferred from agent 9d0e1f2a… · /
    );
    // Recomputed canTransfer: false hides the button and unmounts the closed dialog.
    expect(buttons(el)).not.toContain('Transfer…');
    expect(el.shadowRoot!.querySelector('scion-artifact-transfer-dialog')).toBeNull();
  });

  it('displays Transferred from header meta with resolved former agent name on initial load', async () => {
    mockFetch({
      initial: meta(
        { canTransfer: false },
        {
          ownerRef: NEW_AGENT,
          ownerDeleted: false,
          formerOwnerKind: 'agent',
          formerOwnerRef: OLD_AGENT,
          transferredAt: '2026-11-26T04:15:00Z',
        }
      ),
      agentNames: {
        [OLD_AGENT]: 'old-builder',
        [NEW_AGENT]: 'Release Bot',
      },
    });
    const el = await mount();
    const transferredFrom = el.shadowRoot!.querySelector('.meta .transferred-from');
    expect(transferredFrom).not.toBeNull();
    expect(transferredFrom!.textContent!.replace(/\s+/g, ' ').trim()).toMatch(
      /^Transferred from agent old-builder · /
    );
  });
});
