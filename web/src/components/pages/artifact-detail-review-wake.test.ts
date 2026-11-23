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
 * Tests for suspended-agent wake handling on artifact review submission
 * (ptone/scion#8946): auto-wake notice for system-suspended agents, shared
 * wake-confirm dialog for user-suspended agents (confirm & decline), refused
 * and failed wake notices, and unchanged behavior for running agents.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));
vi.mock('../shared/chat/chat-wake.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../shared/chat/chat-wake.js')>();
  return {
    ...actual,
    confirmWake: vi.fn(),
  };
});

import './artifact-detail.js';
import { reviewDeliveryNotice } from './artifact-detail.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import { confirmWake } from '../shared/chat/chat-wake.js';
import { ARTIFACTS_FLAG } from '../../client/artifacts.js';
import type { ArtifactResponse, ReviewDeliveryInfo } from '../../client/artifacts.js';
import { requestBodyText, requestUrl } from '../../client/__fixtures__/request-url.js';
import { resetServerFlagStateForTests } from '../../utils/feature-flags.js';

const PROJECT_ID = '11111111-1111-1111-1111-111111111111';
const ARTIFACT_ID = '33333333-3333-3333-3333-333333333333';
const BASE_TEXT = 'We ship in Q3.\n';
const MARKED_TEXT = 'We {~~ship~>launch~~} in Q3.\n';

function agentArtifact(seq = 1, reviewDelivery?: ReviewDeliveryInfo): ArtifactResponse {
  return {
    artifact: {
      id: ARTIFACT_ID,
      ref: `scion://artifact/${ARTIFACT_ID}`,
      scopeKind: 'project',
      scopeRef: PROJECT_ID,
      ownerKind: 'agent',
      ownerRef: 'agent-1',
      title: 'Plan',
      currentSeq: seq,
      createdAt: '2026-11-20T00:00:00Z',
      updatedAt: '2026-11-20T00:00:00Z',
    },
    version: {
      seq,
      ref: `scion://artifact/${ARTIFACT_ID}@${seq}`,
      kind: seq > 1 ? 'review' : 'publish',
      entryPath: 'plan.md',
      totalBytes: BASE_TEXT.length,
      fileCount: 1,
      createdByKind: seq > 1 ? 'user' : 'agent',
      createdByRef: seq > 1 ? 'u-1' : 'agent-1',
      createdAt: '2026-11-20T00:00:00Z',
      state: 'ready',
      files: [
        {
          path: 'plan.md',
          size: BASE_TEXT.length,
          sha256: 'abc',
          mediaType: 'text/markdown',
        },
      ],
    },
    canPublish: true,
    canReview: true,
    ...(reviewDelivery ? { reviewDelivery } : {}),
  };
}

async function settle(el: ScionPageArtifactDetail): Promise<void> {
  for (let i = 0; i < 25; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

interface RecordedFinalize {
  url: string;
  body: Record<string, unknown>;
}

function setupMockFetch(
  onFinalize: (callIndex: number, body: Record<string, unknown>) => Response
): { finalizeCalls: RecordedFinalize[]; createCount: () => number; uploadCount: () => number } {
  const finalizeCalls: RecordedFinalize[] = [];
  let creates = 0;
  let uploads = 0;
  let currentSeq = 1;

  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';

      if (method === 'POST' && url.endsWith(`/artifacts/${ARTIFACT_ID}/versions`)) {
        creates++;
        return Promise.resolve(
          new Response(
            JSON.stringify({
              artifact: agentArtifact(currentSeq).artifact,
              version: { ...agentArtifact(2).version, seq: 2, state: 'pending' },
              upload: { required: ['plan.md'] },
            }),
            { status: 201 }
          )
        );
      }
      if (method === 'PUT' && url.includes('/files/')) {
        uploads++;
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (method === 'POST' && url.endsWith('/finalize')) {
        const raw = requestBodyText(init?.body);
        const body = raw ? (JSON.parse(raw) as Record<string, unknown>) : {};
        const idx = finalizeCalls.length;
        finalizeCalls.push({ url, body });
        const res = onFinalize(idx, body);
        if (res.ok) currentSeq = 2;
        return Promise.resolve(res);
      }
      if (url.includes('/review-sessions/')) {
        return Promise.resolve(new Response(JSON.stringify({ currentSeq }), { status: 200 }));
      }
      if (url.endsWith('/grants')) {
        return Promise.resolve(
          new Response(JSON.stringify({ grants: [], crossProjectSharing: false }), { status: 200 })
        );
      }
      if (url.startsWith('/api/v1/agents/')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({ id: 'agent-1', slug: 'review-owner', name: 'review-owner' }),
            {
              status: 200,
            }
          )
        );
      }
      if (/\/versions(\?|$)/.test(url)) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              versions: [{ ...agentArtifact(currentSeq).version, files: [] }],
              discarded: [],
            }),
            { status: 200 }
          )
        );
      }
      if (url.includes('/files/')) {
        return Promise.resolve(
          new Response(currentSeq === 2 ? MARKED_TEXT : BASE_TEXT, {
            status: 200,
            headers: { 'Content-Type': 'text/markdown' },
          })
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify(agentArtifact(currentSeq)), { status: 200 })
      );
    })
  );

  return {
    finalizeCalls,
    createCount: () => creates,
    uploadCount: () => uploads,
  };
}

async function mountAndSubmitReview(): Promise<ScionPageArtifactDetail> {
  window.__SCION_FEATURES__ = { 'hub.artifacts': true };
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.pageData = {
    path: `/projects/${PROJECT_ID}/artifacts/${ARTIFACT_ID}`,
  } as ScionPageArtifactDetail['pageData'];
  document.body.appendChild(el);
  await settle(el);

  // Enter review mode via the Review button.
  const reviewBtn = Array.from(
    el.shadowRoot!.querySelectorAll<HTMLElement>('.header-actions sl-button')
  ).find((b) => b.textContent?.trim().startsWith('Review'));
  expect(reviewBtn).toBeDefined();
  reviewBtn!.click();
  await settle(el);

  // Update review text via the code editor event and click Save review.
  const editor = el.shadowRoot!.querySelector('scion-code-editor.review-editor')!;
  editor.dispatchEvent(new CustomEvent('content-changed', { detail: { content: MARKED_TEXT } }));
  await el.updateComplete;
  const saveBtn = Array.from(
    el.shadowRoot!.querySelectorAll<HTMLElement>('.edit-footer sl-button')
  ).find((b) => b.textContent?.includes('Save review'));
  expect(saveBtn).toBeDefined();
  saveBtn!.click();
  await settle(el);
  return el;
}

describe('artifact-detail review wake handling (#8946)', () => {
  beforeEach(() => {
    localStorage.setItem(ARTIFACTS_FLAG, '1');
    vi.mocked(confirmWake).mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    localStorage.clear();
    resetServerFlagStateForTests();
    vi.unstubAllGlobals();
  });

  it('auto-wakes a system-suspended owner without prompting and shows a clear notice', async () => {
    const tracker = setupMockFetch(() => {
      return new Response(
        JSON.stringify(
          agentArtifact(2, {
            status: 'auto_waking',
            agentId: 'agent-1',
            agentSlug: 'review-owner',
          })
        ),
        { status: 200 }
      );
    });

    const el = await mountAndSubmitReview();

    expect(confirmWake).not.toHaveBeenCalled();
    expect(tracker.finalizeCalls).toHaveLength(1);
    expect(tracker.finalizeCalls[0].body).toEqual({ base: 1, offer_wake: true });

    const alert = el.shadowRoot!.querySelector('.review-notice');
    expect(alert).not.toBeNull();
    expect(alert!.getAttribute('variant')).toBe('primary');
    expect(alert!.textContent).toContain(
      'Review saved. Waking @review-owner to deliver the review.'
    );
  });

  it('shows the shared wake-confirm dialog for a user-suspended owner and wakes on confirm', async () => {
    vi.mocked(confirmWake).mockResolvedValue(true);
    const tracker = setupMockFetch((idx) => {
      if (idx === 0) {
        return new Response(
          JSON.stringify({
            error: {
              code: 'agent_not_running',
              message: 'Agent "review-owner" is suspended',
              details: {
                agentId: 'agent-1',
                agentSlug: 'review-owner',
                phase: 'suspended',
                canWake: true,
              },
            },
          }),
          { status: 409 }
        );
      }
      return new Response(
        JSON.stringify(
          agentArtifact(2, {
            status: 'woken',
            agentId: 'agent-1',
            agentSlug: 'review-owner',
          })
        ),
        { status: 200 }
      );
    });

    const el = await mountAndSubmitReview();

    expect(confirmWake).toHaveBeenCalledTimes(1);
    expect(confirmWake).toHaveBeenCalledWith(
      { agentId: 'agent-1', agentSlug: 'review-owner' },
      {
        message: '@review-owner is suspended. Wake it to deliver this review?',
        confirmText: 'Wake and submit',
        cancelText: 'Submit without waking',
      }
    );
    // Reuses the same pending version without recreating or re-uploading.
    expect(tracker.createCount()).toBe(1);
    expect(tracker.uploadCount()).toBe(1);
    expect(tracker.finalizeCalls).toHaveLength(2);
    expect(tracker.finalizeCalls[0].body).toEqual({ base: 1, offer_wake: true });
    expect(tracker.finalizeCalls[1].body).toEqual({ base: 1, wake: true });

    const alert = el.shadowRoot!.querySelector('.review-notice');
    expect(alert).not.toBeNull();
    expect(alert!.getAttribute('variant')).toBe('primary');
    expect(alert!.textContent).toContain('Review saved. @review-owner was woken and notified.');
  });

  it('saves without waking when the user declines the wake-confirm dialog', async () => {
    vi.mocked(confirmWake).mockResolvedValue(false);
    const tracker = setupMockFetch((idx) => {
      if (idx === 0) {
        return new Response(
          JSON.stringify({
            error: {
              code: 'agent_not_running',
              message: 'Agent "review-owner" is suspended',
              details: {
                agentId: 'agent-1',
                agentSlug: 'review-owner',
                phase: 'suspended',
                canWake: true,
              },
            },
          }),
          { status: 409 }
        );
      }
      return new Response(
        JSON.stringify(
          agentArtifact(2, {
            status: 'suspended',
            agentId: 'agent-1',
            agentSlug: 'review-owner',
          })
        ),
        { status: 200 }
      );
    });

    const el = await mountAndSubmitReview();

    expect(confirmWake).toHaveBeenCalledTimes(1);
    expect(tracker.createCount()).toBe(1);
    expect(tracker.uploadCount()).toBe(1);
    expect(tracker.finalizeCalls).toHaveLength(2);
    expect(tracker.finalizeCalls[1].body).toEqual({ base: 1 });

    const alert = el.shadowRoot!.querySelector('.review-notice');
    expect(alert).not.toBeNull();
    expect(alert!.getAttribute('variant')).toBe('warning');
    expect(alert!.textContent).toContain(
      'Review saved. @review-owner is suspended and will see it when resumed.'
    );
  });

  it('shows no dialog and no wake notice for a running agent', async () => {
    const tracker = setupMockFetch(() => {
      return new Response(JSON.stringify(agentArtifact(2)), { status: 200 });
    });

    const el = await mountAndSubmitReview();

    expect(confirmWake).not.toHaveBeenCalled();
    expect(tracker.finalizeCalls).toHaveLength(1);
    expect(el.shadowRoot!.querySelector('.review-notice')).toBeNull();
  });

  it('formats refused and failed wake delivery notices clearly', () => {
    expect(
      reviewDeliveryNotice({
        status: 'wake_refused',
        agentSlug: 'review-owner',
        message: 'You do not have permission to wake this agent',
      })
    ).toEqual({
      message:
        'Review saved, but @review-owner could not be woken: You do not have permission to wake this agent. It will see the review when resumed.',
      variant: 'warning',
    });

    expect(
      reviewDeliveryNotice({
        status: 'wake_failed',
        agentSlug: 'review-owner',
        message: 'broker unavailable',
      })
    ).toEqual({
      message:
        'Review saved, but waking @review-owner failed: broker unavailable. It will see the review when resumed.',
      variant: 'warning',
    });
  });
});
