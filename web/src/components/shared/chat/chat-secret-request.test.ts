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

    // Pending row: key label, version, description, textarea + show/hide toggle
    const pendingRow = shadow.querySelector('[data-row-id="r-pending"]')!;
    expect(pendingRow.querySelector('.key-label')?.textContent).toBe('KEY_PENDING');
    expect(pendingRow.querySelector('.version')?.textContent).toBe('v1');
    expect(pendingRow.querySelector('.description')?.textContent).toBe(
      'A GitHub PAT with repo scope'
    );
    const textarea = pendingRow.querySelector('textarea') as HTMLTextAreaElement;
    expect(textarea).not.toBeNull();
    const toggleBtn = pendingRow.querySelector('.toggle-visibility') as HTMLButtonElement;
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
    toggleBtn.click();
    await el.updateComplete;
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

    // Rows updated and form values cleared with messageId unchanged
    expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
    expect(el.shadowRoot!.querySelector('textarea')).toBeNull();
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
  });

  it('clears rendered textarea values after submit with messageId unchanged', async () => {
    // Return empty rows array on 200 so the same messageId keeps its pending
    // textarea mounted; this directly asserts handleSubmit clears values
    // without relying on a messageId change or row transition.
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        rows: [],
        responseMessageId: 'resp-empty',
        replayed: false,
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({
      messageId: 'msg-stay',
      rows: [makeRow({ rowId: 'r-1', messageId: 'msg-stay', key: 'KEY_A', status: 'pending' })],
    });

    const input = el.shadowRoot!.querySelector('textarea[data-key="KEY_A"]') as HTMLTextAreaElement;
    input.value = 'top-secret-value';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    let fulfilledFired = false;
    el.addEventListener('secret-request-fulfilled', () => {
      fulfilledFired = true;
    });

    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => expect(fulfilledFired).toBe(true));
    await el.updateComplete;

    expect(el.messageId).toBe('msg-stay');
    const afterInput = el.shadowRoot!.querySelector(
      'textarea[data-key="KEY_A"]'
    ) as HTMLTextAreaElement;
    expect(afterInput.value).toBe('');
    const submitBtn = el.shadowRoot!.querySelector('.submit-btn') as HTMLElement;
    expect(submitBtn.hasAttribute('disabled')).toBe(true);
  });

  it('reuses dismiss idempotency key on retry of the same attempt and mints a new one for a different attempt', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse({ error: { code: 'service_unavailable', message: 'try again' } }, 503)
      )
      .mockResolvedValueOnce(
        jsonResponse({ error: { code: 'service_unavailable', message: 'try again' } }, 503)
      )
      .mockResolvedValueOnce(
        jsonResponse({ error: { code: 'service_unavailable', message: 'try again' } }, 503)
      )
      .mockResolvedValueOnce(
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
                reason: 'different reason',
                surface: 'native',
              },
            }),
          ],
          alreadyDismissed: [],
          responseMessageId: 'resp-d3',
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

    const runDismiss = async (opts: { uncheckR2?: boolean; reason: string }): Promise<string> => {
      const priorCalls = fetchMock.mock.calls.length;
      (el.shadowRoot!.querySelector('.dismiss-btn') as HTMLElement).click();
      await el.updateComplete;

      if (opts.uncheckR2) {
        const cb2 = el.shadowRoot!.querySelector(
          'sl-checkbox[data-dismiss-row="r-2"]'
        ) as HTMLElement & { checked: boolean };
        cb2.checked = false;
        cb2.dispatchEvent(new CustomEvent('sl-change', { bubbles: true }));
        await el.updateComplete;
      }

      const reasonInput = el.shadowRoot!.querySelector('.dismiss-reason-input') as HTMLElement & {
        value: string;
      };
      reasonInput.value = opts.reason;
      reasonInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
      await el.updateComplete;

      (el.shadowRoot!.querySelector('.dismiss-confirm-btn') as HTMLElement).click();
      await vi.waitFor(() => {
        expect(fetchMock).toHaveBeenCalledTimes(priorCalls + 1);
        const dialog = el.shadowRoot!.querySelector('sl-dialog.dismiss-dialog');
        expect(dialog?.hasAttribute('open')).toBe(false);
      });
      await el.updateComplete;

      const [, init] = fetchMock.mock.calls[priorCalls] as [string, RequestInit];
      const sent = JSON.parse(init.body as string) as { idempotencyKey: string };
      return sent.idempotencyKey;
    };

    // 1. Initial attempt (whole card, reason "first reason") fails with 503
    const key1 = await runDismiss({ reason: 'first reason' });
    // 2. Retry exact same attempt (whole card, reason "first reason") -> reuses key1
    const key2 = await runDismiss({ reason: 'first reason' });
    expect(key2).toBe(key1);
    // 3. New attempt with different reason -> mints a new key
    const key3 = await runDismiss({ reason: 'different reason' });
    expect(key3).not.toBe(key1);
    // 4. New attempt with different row selection (subset) -> mints a new key
    const key4 = await runDismiss({ uncheckR2: true, reason: 'different reason' });
    expect(key4).not.toBe(key3);
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

  it('dismisses whole card (sends explicit rowIds) with optional reason', async () => {
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
    expect(sent.rowIds).toEqual(['r-1', 'r-2']);
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

  it('defaults canRespond to false until explicitly enabled', async () => {
    const el = document.createElement('scion-chat-secret-request') as ScionChatSecretRequest;
    el.messageId = 'msg-1';
    el.rows = [makeRow()];
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.canRespond).toBe(false);
    const textarea = el.shadowRoot!.querySelector('textarea') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(true);
    expect(el.shadowRoot!.querySelector('.submit-btn')?.hasAttribute('disabled')).toBe(true);
    expect(el.shadowRoot!.querySelector('.dismiss-btn')?.hasAttribute('disabled')).toBe(true);
    expect(el.shadowRoot!.querySelector('.status-alert.blocked')).not.toBeNull();
  });

  it('keeps textarea mounted while typing on focus and removes plaintext from DOM when masked on blur', async () => {
    const el = await mountCard();
    const initialTextarea = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    expect(initialTextarea).not.toBeNull();

    // Focus and type several keystrokes — textarea stays mounted throughout
    initialTextarea.dispatchEvent(new Event('focus'));
    await el.updateComplete;
    for (const chunk of ['ghp_', 'ghp_super_', 'ghp_super_secret\nline2']) {
      initialTextarea.value = chunk;
      initialTextarea.dispatchEvent(new Event('input'));
      await el.updateComplete;
      expect(el.shadowRoot!.querySelector('textarea[data-key="GITHUB_TOKEN"]')).toBe(
        initialTextarea
      );
    }

    // Blur switches to read-only masked placeholder with NO plaintext in DOM
    initialTextarea.dispatchEvent(new Event('blur'));
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('textarea[data-key="GITHUB_TOKEN"]')).toBeNull();
    const masked = el.shadowRoot!.querySelector('[data-testid="secret-masked"]');
    expect(masked).not.toBeNull();
    expect(masked?.textContent?.replace(/\s+/g, ' ').trim()).toContain('2 lines');
    expect(el.shadowRoot!.textContent).not.toContain('ghp_super_secret');
    expect(el.shadowRoot!.innerHTML).not.toContain('ghp_super_secret');

    // Toggle Show brings textarea back with value; toggle Hide removes plaintext again
    const toggleBtn = el.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement;
    toggleBtn.click();
    await el.updateComplete;
    const revealedTextarea = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    expect(revealedTextarea).not.toBeNull();
    expect(revealedTextarea.value).toBe('ghp_super_secret\nline2');

    toggleBtn.click();
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('textarea[data-key="GITHUB_TOKEN"]')).toBeNull();
    expect(el.shadowRoot!.textContent).not.toContain('ghp_super_secret');
    expect(el.shadowRoot!.innerHTML).not.toContain('ghp_super_secret');
  });

  it('clears form values on pagehide (bfcache protection)', async () => {
    const el = await mountCard();
    const input = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    input.dispatchEvent(new Event('focus'));
    input.value = 'bfcache-secret';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    window.dispatchEvent(new Event('pagehide'));
    await el.updateComplete;

    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    const after = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    expect(after.value).toBe('');
    expect(el.shadowRoot!.textContent).not.toContain('bfcache-secret');
    expect(el.shadowRoot!.innerHTML).not.toContain('bfcache-secret');
  });

  it('wires submit once (type=button) and sends a single POST per click', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        rows: [
          makeRow({
            rowId: 'row-1',
            status: 'fulfilled',
            fulfilled: {
              at: '2026-11-22T23:55:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              surface: 'native',
            },
          }),
        ],
        responseMessageId: 'resp-single',
        replayed: false,
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({ surface: 'native' });
    const submitBtn = el.shadowRoot!.querySelector('.submit-btn') as HTMLElement;
    expect(submitBtn.getAttribute('type')).toBe('button');

    const input = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    input.value = 'tok-once';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    submitBtn.click();
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const sent = JSON.parse(init.body as string) as { surface: string };
    expect(sent.surface).toBe('native');
  });

  it('rejects values exceeding 64 KiB UTF-8 bytes client-side without calling fetch', async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard();
    const input = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    // 22,000 3-byte UTF-8 characters = 66,000 bytes > 65,536 (even though .length < 65,536)
    input.value = '€'.repeat(22_000);
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await el.updateComplete;

    expect(fetchMock).not.toHaveBeenCalled();
    const alert = el.shadowRoot!.querySelector('.status-alert.error');
    expect(alert?.textContent).toContain('64 KiB');
  });

  it('refetches the card on 400 with details.field === "values"', async () => {
    const updatedRow = makeRow({
      rowId: 'row-1',
      status: 'withdrawn',
      withdrawn: {
        at: '2026-11-22T23:58:00Z',
        reason: 'stale card',
        by: 'agent',
        byName: 'builder-1',
      },
    });
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: {
              code: 'validation_error',
              message: 'values must contain exactly the 0 pending key(s) on this card',
              details: { field: 'values' },
            },
          },
          400
        )
      )
      .mockResolvedValueOnce(
        jsonResponse({
          messageId: 'msg-1',
          projectId: 'proj-1',
          agentId: 'agent-1',
          agentName: 'builder-1',
          canRespond: false,
          reason: 'all secret requests on this card are closed',
          rows: [updatedRow],
        })
      );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard();
    let refreshedEventFired = false;
    el.addEventListener('secret-request-refreshed', () => {
      refreshedEventFired = true;
    });

    const input = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    input.value = 'tok-stale';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();

    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(refreshedEventFired).toBe(true);
    });
    await el.updateComplete;

    expect(fetchMock.mock.calls[1][0]).toBe('/api/v1/secret-requests/msg-1');
    expect(el.shadowRoot!.querySelector('[data-row-id="row-1"]')?.getAttribute('data-status')).toBe(
      'withdrawn'
    );
  });

  it('reuses fulfill idempotencyKey on 503 and network retry, and mints a new key after edit or 409', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse({ error: { code: 'unavailable', message: 'backend down' } }, 503)
      )
      .mockRejectedValueOnce(new TypeError('network down'))
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: {
              code: 'submitting',
              message: 'busy',
            },
          },
          409
        )
      )
      .mockResolvedValueOnce(
        jsonResponse({ error: { code: 'unavailable', message: 'retry after edit' } }, 503)
      )
      .mockResolvedValueOnce(
        jsonResponse({
          rows: [
            makeRow({
              rowId: 'row-1',
              status: 'fulfilled',
              fulfilled: {
                at: '2026-11-22T23:59:00Z',
                byUserId: 'user-1',
                byName: 'Me',
                surface: 'native',
              },
            }),
          ],
          responseMessageId: 'resp-ok',
          replayed: false,
        })
      );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard();
    const input = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    input.dispatchEvent(new Event('focus'));
    input.value = 'tok-1';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const getSentKey = (callIdx: number): string => {
      const [, init] = fetchMock.mock.calls[callIdx] as [string, RequestInit];
      return (JSON.parse(init.body as string) as { idempotencyKey: string }).idempotencyKey;
    };

    // 1. Initial submit -> 503
    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(el.shadowRoot!.querySelector('.status-alert.error')?.textContent).toContain(
        'backend down'
      );
    });
    await el.updateComplete;
    const key1 = getSentKey(0);

    // 2. Retry same value -> network error -> reuses key1
    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(el.shadowRoot!.querySelector('.status-alert.error')?.textContent).toContain(
        'Could not reach the server'
      );
    });
    await el.updateComplete;
    expect(getSentKey(1)).toBe(key1);

    // 3. Retry after network error -> 409 submitting -> still reuses key1, then clears it
    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(3);
      expect(el.shadowRoot!.querySelector('.status-alert.error')?.textContent).toContain(
        'Another submission is currently in progress'
      );
    });
    await el.updateComplete;
    expect(getSentKey(2)).toBe(key1);

    // 4. Retry after 409 (without editing) -> mints a new key2 (and fails with 503)
    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(4);
      expect(el.shadowRoot!.querySelector('.status-alert.error')?.textContent).toContain(
        'retry after edit'
      );
    });
    await el.updateComplete;
    const key2 = getSentKey(3);
    expect(key2).not.toBe(key1);

    // 5. Edit value -> mints a new key3
    const textareaAfter = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    textareaAfter.value = 'tok-2';
    textareaAfter.dispatchEvent(new Event('input'));
    await el.updateComplete;

    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(5);
      expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
    });
    await el.updateComplete;
    const key3 = getSentKey(4);
    expect(key3).not.toBe(key2);
  });

  it('clears form values and displays reason on 403 refusal', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        jsonResponse(
          { error: { code: 'forbidden', message: 'you need agent.attach permission' } },
          403
        )
      );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard();
    const input = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    input.value = 'secret-denied';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(el.shadowRoot!.querySelector('.status-alert.blocked')).not.toBeNull();
    });
    await el.updateComplete;

    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    expect(el.shadowRoot!.querySelector('.status-alert.blocked')?.textContent).toContain(
      'you need agent.attach permission'
    );
    const textareaAfter = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    expect(textareaAfter.value).toBe('');
    expect(textareaAfter.disabled).toBe(true);
  });

  it('applies full secretRequest view from 409 conflict details (applyConflictDetails)', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          error: {
            code: 'withdrawn',
            message: 'conflict',
            details: {
              rowId: 'row-1',
              key: 'GITHUB_TOKEN',
              secretRequest: makeRow({
                rowId: 'row-1',
                status: 'withdrawn',
                withdrawn: {
                  at: '2026-11-22T23:57:00Z',
                  reason: 'agent deleted',
                  by: 'hub',
                },
              }),
            },
          },
        },
        409
      )
    );
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
      expect(el.shadowRoot!.textContent).toContain('agent deleted');
    });
    await el.updateComplete;

    expect(el.shadowRoot!.textContent).toContain('the hub');
    expect(el.shadowRoot!.querySelector('[data-row-id="row-1"]')?.getAttribute('data-status')).toBe(
      'withdrawn'
    );
  });
});
