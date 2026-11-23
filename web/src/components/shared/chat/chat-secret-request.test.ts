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
import {
  AGENT_SECRETS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../../utils/feature-flags.js';
import { stateManager } from '../../../client/state.js';
import type { SecretRequestView } from '../../../client/secret-requests.js';
import './chat-secret-request.js';
import type { ScionChatSecretRequest } from './chat-secret-request.js';

function makeRow(overrides: Partial<SecretRequestView> = {}): SecretRequestView {
  return {
    rowId: 'row-1',
    messageId: 'msg-1',
    projectId: 'proj-1',
    agentId: 'agent-1',
    agentName: 'builder-1',
    key: 'GITHUB_TOKEN',
    version: 1,
    description: 'A GitHub PAT with repo scope',
    secretType: 'environment',
    target: '',
    position: 0,
    status: 'pending',
    submitting: false,
    createdAt: '2026-11-22T23:50:00Z',
    expiresAt: '2026-11-29T23:50:00Z',
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function mountCard(
  props: Partial<ScionChatSecretRequest> = {}
): Promise<ScionChatSecretRequest> {
  const el = document.createElement('scion-chat-secret-request') as ScionChatSecretRequest;
  el.messageId = 'msg-1';
  el.agentName = 'builder-1';
  el.projectName = 'my-project';
  el.currentUserId = 'user-1';
  el.canRespond = true;
  el.rows = [makeRow()];
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-chat-secret-request', () => {
  beforeEach(() => {
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    localStorage.clear();
    sessionStorage.clear();
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
  });

  it('hides completely when hub.agent_secrets is off', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, false);
    const el = await mountCard();
    expect(el.shadowRoot?.querySelector('.card')).toBeNull();
  });

  it('renders every per-key state and the storage notice', async () => {
    const rows: SecretRequestView[] = [
      makeRow({
        rowId: 'r-pending',
        key: 'KEY_PENDING',
        version: 1,
        position: 0,
        status: 'pending',
      }),
      makeRow({
        rowId: 'r-submitting',
        key: 'KEY_SUBMITTING',
        version: 1,
        position: 1,
        status: 'pending',
        submitting: true,
      }),
      makeRow({
        rowId: 'r-fulfilled',
        key: 'KEY_FULFILLED',
        version: 2,
        position: 2,
        status: 'fulfilled',
        fulfilled: {
          at: '2026-11-22T23:55:00Z',
          byUserId: 'user-2',
          byName: 'Alice',
          surface: 'standalone_page',
        },
      }),
      makeRow({
        rowId: 'r-replaced',
        key: 'KEY_REPLACED',
        version: 1,
        position: 3,
        status: 'replaced',
        replacedBy: {
          rowId: 'r-new',
          messageId: 'msg-2',
          version: 2,
          at: '2026-11-22T23:56:00Z',
        },
      }),
      makeRow({
        rowId: 'r-withdrawn',
        key: 'KEY_WITHDRAWN',
        version: 1,
        position: 4,
        status: 'withdrawn',
        withdrawn: {
          at: '2026-11-22T23:57:00Z',
          reason: 'no longer needed',
          by: 'agent',
          byName: 'builder-1',
        },
      }),
      makeRow({
        rowId: 'r-dismissed',
        key: 'KEY_DISMISSED',
        version: 1,
        position: 5,
        status: 'dismissed',
        dismissed: {
          at: '2026-11-22T23:58:00Z',
          byUserId: 'user-3',
          byName: 'Bob',
          reason: 'use the shared bot token',
          surface: 'native',
        },
      }),
      makeRow({
        rowId: 'r-expired',
        key: 'KEY_EXPIRED',
        version: 1,
        position: 6,
        status: 'expired',
        expired: { at: '2026-11-29T23:50:00Z' },
      }),
    ];

    const el = await mountCard({ rows, agentName: 'builder-1', projectName: 'my-project' });
    const shadow = el.shadowRoot!;

    // Storage notice
    const notice = shadow
      .querySelector('.storage-notice')
      ?.textContent?.replace(/\s+/g, ' ')
      .trim();
    expect(notice).toContain(
      'Stored as a secret readable only by agent builder-1 in project my-project.'
    );

    // Pending row: key label, version, description, masked textarea + show/hide toggle
    const pendingRow = shadow.querySelector('[data-row-id="r-pending"]')!;
    expect(pendingRow.querySelector('.key-label')?.textContent).toBe('KEY_PENDING');
    expect(pendingRow.querySelector('.version')?.textContent).toBe('v1');
    expect(pendingRow.querySelector('.description')?.textContent).toBe(
      'A GitHub PAT with repo scope'
    );
    const textarea = pendingRow.querySelector('textarea') as HTMLTextAreaElement;
    expect(textarea.classList.contains('masked')).toBe(true);
    const toggleBtn = pendingRow.querySelector('.toggle-visibility') as HTMLButtonElement;
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
    toggleBtn.click();
    await el.updateComplete;
    expect(textarea.classList.contains('masked')).toBe(false);
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('true');

    // Submitting row
    const submittingRow = shadow.querySelector('[data-row-id="r-submitting"]')!;
    expect(submittingRow.textContent).toContain('Submitting…');

    // Fulfilled row
    const fulfilledRow = shadow.querySelector('[data-row-id="r-fulfilled"]')!;
    expect(fulfilledRow.textContent).toContain('Fulfilled by Alice');

    // Replaced row with jump link
    const replacedRow = shadow.querySelector('[data-row-id="r-replaced"]')!;
    expect(replacedRow.textContent).toContain('Replaced by v2');
    const jumpLink = replacedRow.querySelector('a.jump') as HTMLAnchorElement;
    expect(jumpLink).not.toBeNull();
    let jumpedTo = '';
    el.addEventListener('secret-request-jump', (e) => {
      jumpedTo = (e as CustomEvent<{ messageId: string }>).detail.messageId;
    });
    jumpLink.click();
    expect(jumpedTo).toBe('msg-2');

    // Withdrawn row with reason
    const withdrawnRow = shadow.querySelector('[data-row-id="r-withdrawn"]')!;
    expect(withdrawnRow.textContent).toContain('No longer needed');
    expect(withdrawnRow.textContent).toContain('builder-1');
    expect(withdrawnRow.textContent).toContain('no longer needed');

    // Dismissed row with who & reason
    const dismissedRow = shadow.querySelector('[data-row-id="r-dismissed"]')!;
    expect(dismissedRow.textContent).toContain('Dismissed');
    expect(dismissedRow.textContent).toContain('Bob');
    expect(dismissedRow.textContent).toContain('use the shared bot token');

    // Expired row
    const expiredRow = shadow.querySelector('[data-row-id="r-expired"]')!;
    expect(expiredRow.textContent).toContain('Expired');
  });

  it('submits exactly the pending keys, sends surface & idempotencyKey, and clears values after submit', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        rows: [
          makeRow({
            rowId: 'r-1',
            key: 'KEY_A',
            position: 0,
            status: 'fulfilled',
            fulfilled: {
              at: '2026-11-22T23:55:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              surface: 'standalone_page',
            },
          }),
          makeRow({
            rowId: 'r-2',
            key: 'KEY_B',
            position: 1,
            status: 'dismissed',
            dismissed: { at: '2026-11-22T23:51:00Z', byName: 'Bob', surface: 'native' },
          }),
          makeRow({
            rowId: 'r-3',
            key: 'KEY_C',
            position: 2,
            status: 'fulfilled',
            fulfilled: {
              at: '2026-11-22T23:55:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              surface: 'standalone_page',
            },
          }),
        ],
        responseMessageId: 'resp-1',
        replayed: false,
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({
      surface: 'standalone_page',
      rows: [
        makeRow({ rowId: 'r-1', key: 'KEY_A', position: 0, status: 'pending' }),
        makeRow({
          rowId: 'r-2',
          key: 'KEY_B',
          position: 1,
          status: 'dismissed',
          dismissed: { at: '2026-11-22T23:51:00Z', byName: 'Bob', surface: 'native' },
        }),
        makeRow({ rowId: 'r-3', key: 'KEY_C', position: 2, status: 'pending' }),
      ],
    });

    const inputA = el.shadowRoot!.querySelector(
      'textarea[data-key="KEY_A"]'
    ) as HTMLTextAreaElement;
    const inputC = el.shadowRoot!.querySelector(
      'textarea[data-key="KEY_C"]'
    ) as HTMLTextAreaElement;
    inputA.value = 'secret-val-a';
    inputA.dispatchEvent(new Event('input'));
    inputC.value = 'secret-val-c';
    inputC.dispatchEvent(new Event('input'));
    await el.updateComplete;

    // Values must never touch localStorage or sessionStorage
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);

    const submitBtn = el.shadowRoot!.querySelector('.submit-btn') as HTMLElement;
    expect(submitBtn.hasAttribute('disabled')).toBe(false);
    submitBtn.click();

    await vi.waitFor(() => {
      expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/secret-requests/msg-1/fulfill');
    expect(init.method).toBe('POST');
    const sent = JSON.parse(init.body as string) as {
      values: Record<string, string>;
      surface: string;
      idempotencyKey: string;
    };
    expect(sent.values).toEqual({ KEY_A: 'secret-val-a', KEY_C: 'secret-val-c' });
    expect(sent.surface).toBe('standalone_page');
    expect(sent.idempotencyKey.length).toBeGreaterThan(0);

    // Rows updated and form values cleared
    expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
    expect(el.shadowRoot!.querySelector('textarea')).toBeNull();
    // Internal map cleared (re-mounting/resetting rows to pending would show empty inputs)
    el.rows = [makeRow({ rowId: 'r-1', key: 'KEY_A', position: 0, status: 'pending' })];
    el.messageId = 'msg-fresh';
    await el.updateComplete;
    const freshInput = el.shadowRoot!.querySelector(
      'textarea[data-key="KEY_A"]'
    ) as HTMLTextAreaElement;
    expect(freshInput.value).toBe('');
  });

  it('clears form values on disconnect/navigation', async () => {
    const el = await mountCard();
    const input = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    input.value = 'ephemeral-secret';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    el.remove();
    document.body.appendChild(el);
    await el.updateComplete;

    const afterReconnect = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    expect(afterReconnect.value).toBe('');
  });

  it('dismisses whole card (omits rowIds) with optional reason', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        dismissed: [
          makeRow({
            rowId: 'r-1',
            key: 'KEY_A',
            position: 0,
            status: 'dismissed',
            dismissed: {
              at: '2026-11-22T23:59:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              reason: 'not needed',
              surface: 'standalone_page',
            },
          }),
          makeRow({
            rowId: 'r-2',
            key: 'KEY_B',
            position: 1,
            status: 'dismissed',
            dismissed: {
              at: '2026-11-22T23:59:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              reason: 'not needed',
              surface: 'standalone_page',
            },
          }),
        ],
        alreadyDismissed: [],
        responseMessageId: 'resp-d1',
        replayed: false,
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({
      surface: 'standalone_page',
      rows: [
        makeRow({ rowId: 'r-1', key: 'KEY_A', position: 0 }),
        makeRow({ rowId: 'r-2', key: 'KEY_B', position: 1 }),
      ],
    });

    (el.shadowRoot!.querySelector('.dismiss-btn') as HTMLElement).click();
    await el.updateComplete;

    const reasonInput = el.shadowRoot!.querySelector('.dismiss-reason-input') as HTMLElement & {
      value: string;
    };
    reasonInput.value = 'not needed';
    reasonInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
    await el.updateComplete;

    (el.shadowRoot!.querySelector('.dismiss-confirm-btn') as HTMLElement).click();
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    await el.updateComplete;

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/secret-requests/msg-1/dismiss');
    const sent = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(sent.rowIds).toBeUndefined();
    expect(sent.reason).toBe('not needed');
    expect(sent.surface).toBe('standalone_page');
    expect(typeof sent.idempotencyKey).toBe('string');
  });

  it('dismisses a subset of keys (sends rowIds) with reason', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        dismissed: [
          makeRow({
            rowId: 'r-1',
            key: 'KEY_A',
            position: 0,
            status: 'dismissed',
            dismissed: {
              at: '2026-11-22T23:59:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              reason: 'skip A',
              surface: 'native',
            },
          }),
        ],
        alreadyDismissed: [],
        responseMessageId: 'resp-d2',
        replayed: false,
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({
      rows: [
        makeRow({ rowId: 'r-1', key: 'KEY_A', position: 0 }),
        makeRow({ rowId: 'r-2', key: 'KEY_B', position: 1 }),
      ],
    });

    (el.shadowRoot!.querySelector('.dismiss-btn') as HTMLElement).click();
    await el.updateComplete;

    // Uncheck r-2 so only r-1 is dismissed
    const cb2 = el.shadowRoot!.querySelector(
      'sl-checkbox[data-dismiss-row="r-2"]'
    ) as HTMLElement & {
      checked: boolean;
    };
    cb2.checked = false;
    cb2.dispatchEvent(new CustomEvent('sl-change', { bubbles: true }));
    await el.updateComplete;

    const reasonInput = el.shadowRoot!.querySelector('.dismiss-reason-input') as HTMLElement & {
      value: string;
    };
    reasonInput.value = 'skip A';
    reasonInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
    await el.updateComplete;

    (el.shadowRoot!.querySelector('.dismiss-confirm-btn') as HTMLElement).click();
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    await el.updateComplete;

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const sent = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(sent.rowIds).toEqual(['r-1']);
    expect(sent.reason).toBe('skip A');
  });

  it.each([
    {
      code: 'replaced',
      expectedText: 'was replaced by v2',
      expectedStatus: 'replaced',
      details: {
        rowId: 'row-1',
        key: 'GITHUB_TOKEN',
        version: 1,
        status: 'replaced',
        replacedBy: { rowId: 'row-2', messageId: 'msg-2', version: 2, at: '2026-11-22T23:55:00Z' },
      },
    },
    {
      code: 'withdrawn',
      expectedText: 'was withdrawn by the agent',
      expectedStatus: 'withdrawn',
      details: { rowId: 'row-1', key: 'GITHUB_TOKEN', version: 1, status: 'withdrawn' },
    },
    {
      code: 'dismissed',
      expectedText: 'was already dismissed',
      expectedStatus: 'dismissed',
      details: { rowId: 'row-1', key: 'GITHUB_TOKEN', version: 1, status: 'dismissed' },
    },
    {
      code: 'expired',
      expectedText: 'has expired',
      expectedStatus: 'expired',
      details: { rowId: 'row-1', key: 'GITHUB_TOKEN', version: 1, status: 'expired' },
    },
    {
      code: 'already_fulfilled',
      expectedText: 'has already been fulfilled',
      expectedStatus: 'fulfilled',
      details: { rowId: 'row-1', key: 'GITHUB_TOKEN', version: 1, status: 'fulfilled' },
    },
    {
      code: 'submitting',
      expectedText: 'Another submission is currently in progress',
      expectedStatus: 'pending',
      details: { rowId: 'row-1', key: 'GITHUB_TOKEN', version: 1, status: 'pending' },
    },
  ])(
    'maps 409 $code to the right state and error message',
    async ({ code, expectedText, expectedStatus, details }) => {
      const fetchMock = vi
        .fn()
        .mockResolvedValue(jsonResponse({ error: { code, message: 'conflict', details } }, 409));
      vi.stubGlobal('fetch', fetchMock);

      const el = await mountCard();
      const input = el.shadowRoot!.querySelector(
        'textarea[data-key="GITHUB_TOKEN"]'
      ) as HTMLTextAreaElement;
      input.value = 'tok-123';
      input.dispatchEvent(new Event('input'));
      await el.updateComplete;

      (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
      await vi.waitFor(() => {
        expect(el.shadowRoot!.querySelector('.status-alert.error')).not.toBeNull();
      });
      await el.updateComplete;

      const alert = el.shadowRoot!.querySelector('.status-alert.error');
      expect(alert?.textContent).toContain(expectedText);
      const rowEl = el.shadowRoot!.querySelector('[data-row-id="row-1"]');
      expect(rowEl?.getAttribute('data-status')).toBe(expectedStatus);
    }
  );

  it('disables form and displays reason when canRespond is false', async () => {
    const el = await mountCard({
      canRespond: false,
      reason: 'Requires agent.attach permission on builder-1.',
    });

    const textarea = el.shadowRoot!.querySelector('textarea') as HTMLTextAreaElement;
    const submitBtn = el.shadowRoot!.querySelector('.submit-btn') as HTMLElement;
    const dismissBtn = el.shadowRoot!.querySelector('.dismiss-btn') as HTMLElement;
    const blocked = el.shadowRoot!.querySelector('.status-alert.blocked');

    expect(textarea.disabled).toBe(true);
    expect(submitBtn.hasAttribute('disabled')).toBe(true);
    expect(dismissBtn.hasAttribute('disabled')).toBe(true);
    expect(blocked?.textContent).toContain('Requires agent.attach permission on builder-1.');
  });

  it('applies live updates from chat.secret_request.updated via stateManager', async () => {
    const el = await mountCard();
    expect(el.shadowRoot!.querySelector('textarea')).not.toBeNull();

    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          data: {
            kind: 'secret_request',
            messageId: 'msg-1',
            secret_request: makeRow({
              rowId: 'row-1',
              status: 'withdrawn',
              withdrawn: {
                at: '2026-11-22T23:59:00Z',
                reason: 'found in cache',
                by: 'agent',
                byName: 'builder-1',
              },
            }),
          },
        },
      })
    );
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('textarea')).toBeNull();
    expect(el.shadowRoot!.textContent).toContain('found in cache');
  });
});
