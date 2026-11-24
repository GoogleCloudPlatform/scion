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

    // Pending row: key label, version, description, password input + show/hide toggle
    const pendingRow = shadow.querySelector('[data-row-id="r-pending"]')!;
    expect(pendingRow.querySelector('.key-label')?.textContent).toBe('KEY_PENDING');
    expect(pendingRow.querySelector('.version')?.textContent).toBe('v1');
    expect(pendingRow.querySelector('.description')?.textContent).toBe(
      'A GitHub PAT with repo scope'
    );
    const input = pendingRow.querySelector('input') as HTMLInputElement;
    expect(input).not.toBeNull();
    expect(input.type).toBe('password');
    expect(input.autocomplete).toBe('off');
    const toggleBtn = pendingRow.querySelector('.toggle-visibility') as HTMLButtonElement;
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
    toggleBtn.click();
    await el.updateComplete;
    expect(pendingRow.querySelector('textarea')).not.toBeNull();
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

    const inputA = el.shadowRoot!.querySelector('input[data-key="KEY_A"]') as HTMLInputElement;
    const inputC = el.shadowRoot!.querySelector('input[data-key="KEY_C"]') as HTMLInputElement;
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
    expect(el.shadowRoot!.querySelector('input')).toBeNull();
    expect(el.shadowRoot!.querySelector('textarea')).toBeNull();
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
  });

  it('clears rendered input values after submit with messageId unchanged', async () => {
    // Return empty rows array on 200 so the same messageId keeps its pending
    // input mounted; this directly asserts handleSubmit clears values
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

    const input = el.shadowRoot!.querySelector('input[data-key="KEY_A"]') as HTMLInputElement;
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
    const afterInput = el.shadowRoot!.querySelector('input[data-key="KEY_A"]') as HTMLInputElement;
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
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    input.value = 'ephemeral-secret';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    el.remove();
    document.body.appendChild(el);
    await el.updateComplete;

    const afterReconnect = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
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
        'input[data-key="GITHUB_TOKEN"]'
      ) as HTMLInputElement;
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

    const input = el.shadowRoot!.querySelector('input') as HTMLInputElement;
    const submitBtn = el.shadowRoot!.querySelector('.submit-btn') as HTMLElement;
    const dismissBtn = el.shadowRoot!.querySelector('.dismiss-btn') as HTMLElement;
    const blocked = el.shadowRoot!.querySelector('.status-alert.blocked');

    expect(input.disabled).toBe(true);
    expect(submitBtn.hasAttribute('disabled')).toBe(true);
    expect(dismissBtn.hasAttribute('disabled')).toBe(true);
    expect(blocked?.textContent).toContain('Requires agent.attach permission on builder-1.');
  });

  it('applies live updates from chat.secret_request.updated via stateManager', async () => {
    const el = await mountCard();
    expect(el.shadowRoot!.querySelector('input')).not.toBeNull();

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

    expect(el.shadowRoot!.querySelector('input')).toBeNull();
    expect(el.shadowRoot!.textContent).toContain('found in cache');
  });

  it('defaults canRespond to false until explicitly enabled', async () => {
    const el = document.createElement('scion-chat-secret-request') as ScionChatSecretRequest;
    el.messageId = 'msg-1';
    el.rows = [makeRow()];
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.canRespond).toBe(false);
    const input = el.shadowRoot!.querySelector('input') as HTMLInputElement;
    expect(input.disabled).toBe(true);
    expect(el.shadowRoot!.querySelector('.submit-btn')?.hasAttribute('disabled')).toBe(true);
    expect(el.shadowRoot!.querySelector('.dismiss-btn')?.hasAttribute('disabled')).toBe(true);
    expect(el.shadowRoot!.querySelector('.status-alert.blocked')).not.toBeNull();
  });

  it('captures multi-line paste verbatim on file rows while masked, preserves value across Show/Hide, and submits newlines intact', async () => {
    const multilineSecret =
      '-----BEGIN PRIVATE KEY-----\nline-two-secret\n-----END PRIVATE KEY-----';
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        rows: [
          makeRow({
            rowId: 'r-file',
            key: 'DEPLOY_KEY',
            secretType: 'file',
            status: 'fulfilled',
            fulfilled: {
              at: '2026-11-24T00:05:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              surface: 'native',
            },
          }),
        ],
        responseMessageId: 'resp-file-paste',
        replayed: false,
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({
      rows: [makeRow({ rowId: 'r-file', key: 'DEPLOY_KEY', secretType: 'file' })],
    });
    const input = el.shadowRoot!.querySelector('input[data-key="DEPLOY_KEY"]') as HTMLInputElement;
    expect(input).not.toBeNull();
    expect(input.type).toBe('password');
    expect(input.autocomplete).toBe('off');
    expect(el.shadowRoot!.querySelector('textarea[data-key="DEPLOY_KEY"]')).toBeNull();

    // Dispatch paste event with multi-line clipboard data
    const pasteEvent = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(pasteEvent, 'clipboardData', {
      value: {
        getData: (type: string) => (type === 'text/plain' ? multilineSecret : ''),
      },
    });
    input.dispatchEvent(pasteEvent);
    await el.updateComplete;

    expect(pasteEvent.defaultPrevented).toBe(true);
    expect(el.shadowRoot!.querySelector('textarea[data-key="DEPLOY_KEY"]')).toBeNull();
    const maskedStatus = el.shadowRoot!.querySelector('[data-testid="file-value-masked"]');
    expect(maskedStatus).not.toBeNull();
    expect(maskedStatus?.getAttribute('aria-label')).toBe(
      'Secret value for DEPLOY_KEY entered, hidden'
    );
    expect(maskedStatus?.querySelector('span[aria-hidden="true"]')).not.toBeNull();
    expect(maskedStatus?.textContent).not.toContain('line');
    expect(el.shadowRoot!.textContent).not.toContain('line-two-secret');
    expect(el.shadowRoot!.innerHTML).not.toContain('line-two-secret');

    // Show reveals textarea with verbatim multi-line value; Hide returns to password input without losing it
    const toggleBtn = el.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement;
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
    toggleBtn.click();
    await el.updateComplete;

    const textarea = el.shadowRoot!.querySelector(
      'textarea[data-key="DEPLOY_KEY"]'
    ) as HTMLTextAreaElement;
    expect(textarea).not.toBeNull();
    expect(textarea.value).toBe(multilineSecret);
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('true');

    toggleBtn.click();
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('textarea[data-key="DEPLOY_KEY"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('input[data-key="DEPLOY_KEY"]')).not.toBeNull();
    expect(el.shadowRoot!.textContent).not.toContain('line-two-secret');
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');

    // Submit sends exact multi-line value with newlines intact
    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
    });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const sent = JSON.parse(init.body as string) as { values: Record<string, string> };
    expect(sent.values).toEqual({ DEPLOY_KEY: multilineSecret });
  });

  it('loads UTF-8 files via file picker, rejects binary and >64 KiB files, and clears file input value', async () => {
    const el = await mountCard({
      rows: [makeRow({ rowId: 'r-file', key: 'DEPLOY_KEY', secretType: 'file' })],
    });
    const fileInput = el.shadowRoot!.querySelector(
      'input[type="file"][data-file-input="DEPLOY_KEY"]'
    ) as HTMLInputElement;
    expect(fileInput).not.toBeNull();

    expect(fileInput.getAttribute('tabindex')).toBe('-1');

    const triggerFile = async (file: File): Promise<void> => {
      Object.defineProperty(fileInput, 'files', {
        value: [file],
        configurable: true,
      });
      fileInput.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 10));
      await el.updateComplete;
    };

    // 1. Reject > 64 KiB before reading
    const oversized = new File([new Uint8Array(64 * 1024 + 1)], 'big.pem');
    await triggerFile(oversized);
    expect(el.shadowRoot!.querySelector('.status-alert.error')?.textContent).toContain('64 KiB');
    expect(fileInput.value).toBe('');
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);

    // 2. Reject invalid UTF-8 (binary) file
    const binary = new File([new Uint8Array([0xff, 0xfe, 0x00, 0x01])], 'cert.der');
    await triggerFile(binary);
    expect(el.shadowRoot!.querySelector('.status-alert.error')?.textContent).toContain('UTF-8');
    expect(fileInput.value).toBe('');
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);

    // 3. Reject valid UTF-8 file containing NUL bytes as binary
    const nulFile = new File(['hello\u0000world'], 'nul.bin', { type: 'text/plain' });
    await triggerFile(nulFile);
    expect(el.shadowRoot!.querySelector('.status-alert.error')?.textContent).toContain('NUL');
    expect(fileInput.value).toBe('');
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);

    // 4. Accept valid UTF-8 multi-line file, masked by default, never rendered into password input value
    const pemContent = '-----BEGIN CERT-----\nabc123\n-----END CERT-----\n';
    const validFile = new File([pemContent], 'cert.pem', { type: 'text/plain' });
    await triggerFile(validFile);
    expect(el.shadowRoot!.querySelector('.status-alert.error')).toBeNull();
    expect(fileInput.value).toBe('');
    expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).not.toBeNull();
    const pwdInput = el.shadowRoot!.querySelector(
      'input[data-key="DEPLOY_KEY"]'
    ) as HTMLInputElement;
    expect(pwdInput.value).toBe('');
    expect(el.shadowRoot!.textContent).not.toContain('abc123');
    expect(el.shadowRoot!.innerHTML).not.toContain('abc123');
    expect((el as unknown as { formValues: Map<string, string> }).formValues.get('r-file')).toBe(
      pemContent
    );

    // pagehide clears loaded file value too
    window.dispatchEvent(new Event('pagehide'));
    await el.updateComplete;
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).toBeNull();

    // 5. Loading a file while Show is active automatically hides the value
    const toggleBtn = el.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement;
    toggleBtn.click();
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('textarea[data-key="DEPLOY_KEY"]')).not.toBeNull();
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('true');

    await triggerFile(new File(['reloaded-secret\nline2'], 'cert2.pem', { type: 'text/plain' }));
    expect(el.shadowRoot!.querySelector('textarea[data-key="DEPLOY_KEY"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('input[data-key="DEPLOY_KEY"]')).not.toBeNull();
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
    expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).not.toBeNull();
    expect(el.shadowRoot!.textContent).not.toContain('reloaded-secret');
  });

  it('clears form values on pagehide (bfcache protection)', async () => {
    const el = await mountCard();
    const input = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    input.value = 'bfcache-secret';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    window.dispatchEvent(new Event('pagehide'));
    await el.updateComplete;

    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    const after = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
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
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
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
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
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

  it('refetches the card on 400 with details.field === "values" and applies deniedReason when canRespond becomes false', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        jsonResponse(
          {
            error: {
              code: 'validation_error',
              message: 'values must contain exactly the 1 pending key(s) on this card',
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
          reason: 'attach permission was revoked',
          rows: [makeRow({ rowId: 'row-1', status: 'pending' })],
        })
      );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard();
    let refreshedEventFired = false;
    el.addEventListener('secret-request-refreshed', () => {
      refreshedEventFired = true;
    });

    const input = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
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
    expect(el.shadowRoot!.querySelector('.status-alert.blocked')?.textContent).toContain(
      'attach permission was revoked'
    );
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    const inputAfter = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    expect(inputAfter.disabled).toBe(true);
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
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
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
    const inputAfter = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    inputAfter.value = 'tok-2';
    inputAfter.dispatchEvent(new Event('input'));
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
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
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
    const inputAfter = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    expect(inputAfter.value).toBe('');
    expect(inputAfter.disabled).toBe(true);
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
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
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

  it('renders variable rows as native password input when masked and textarea only when Show is on', async () => {
    const el = await mountCard({
      rows: [makeRow({ rowId: 'r-var', key: 'API_CONFIG', secretType: 'variable' })],
    });
    const input = el.shadowRoot!.querySelector('input[data-key="API_CONFIG"]') as HTMLInputElement;
    expect(input).not.toBeNull();
    expect(input.type).toBe('password');
    expect(input.autocomplete).toBe('off');
    expect(el.shadowRoot!.querySelector('textarea[data-key="API_CONFIG"]')).toBeNull();

    input.value = '{"token":"secret"}';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    // Show switches to textarea with value preserved in memory
    const toggleBtn = el.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement;
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
    toggleBtn.click();
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('input[data-key="API_CONFIG"]')).toBeNull();
    const ta = el.shadowRoot!.querySelector(
      'textarea[data-key="API_CONFIG"]'
    ) as HTMLTextAreaElement;
    expect(ta).not.toBeNull();
    expect(ta.value).toBe('{"token":"secret"}');
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('true');

    // Hide switches back to native password input
    toggleBtn.click();
    await el.updateComplete;
    const maskedAgain = el.shadowRoot!.querySelector(
      'input[data-key="API_CONFIG"]'
    ) as HTMLInputElement;
    expect(maskedAgain).not.toBeNull();
    expect(maskedAgain.type).toBe('password');
    expect(toggleBtn.getAttribute('aria-pressed')).toBe('false');
  });

  it('ignores a second synchronous click on Submit while a fulfill POST is in flight', async () => {
    let resolveFetch: ((res: Response) => void) | null = null;
    const fetchMock = vi.fn().mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          resolveFetch = resolve;
        })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({ surface: 'native' });
    const input = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    input.value = 'tok-inflight';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;

    const submitBtn = el.shadowRoot!.querySelector('.submit-btn') as HTMLElement;
    // Two synchronous clicks before updateComplete must send only one POST
    submitBtn.click();
    submitBtn.click();

    expect(fetchMock).toHaveBeenCalledTimes(1);

    resolveFetch!(
      jsonResponse({
        rows: [
          makeRow({
            rowId: 'row-1',
            status: 'fulfilled',
            fulfilled: {
              at: '2026-11-24T00:05:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              surface: 'native',
            },
          }),
        ],
        responseMessageId: 'resp-1',
        replayed: false,
      })
    );
    await vi.waitFor(() => {
      expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it.each(['environment', 'variable'] as const)(
    'preserves CR/LF on masked paste and never merges newlines on Hide+type for %s rows',
    async (secretType) => {
      const fetchMock = vi
        .fn()
        .mockResolvedValueOnce(
          jsonResponse({
            rows: [],
            responseMessageId: 'resp-lf',
            replayed: false,
          })
        )
        .mockResolvedValueOnce(
          jsonResponse({
            rows: [],
            responseMessageId: 'resp-crlf',
            replayed: false,
          })
        )
        .mockResolvedValueOnce(
          jsonResponse({
            rows: [],
            responseMessageId: 'resp-replace',
            replayed: false,
          })
        );
      vi.stubGlobal('fetch', fetchMock);

      const el = await mountCard({
        rows: [makeRow({ rowId: 'r-1', key: 'MULTI_KEY', secretType })],
      });

      const pasteInto = (text: string): void => {
        const input = el.shadowRoot!.querySelector(
          'input[data-key="MULTI_KEY"]'
        ) as HTMLInputElement;
        const pasteEvent = new Event('paste', {
          bubbles: true,
          cancelable: true,
        }) as ClipboardEvent;
        Object.defineProperty(pasteEvent, 'clipboardData', {
          value: { getData: (t: string) => (t === 'text/plain' ? text : '') },
        });
        input.dispatchEvent(pasteEvent);
        expect(pasteEvent.defaultPrevented).toBe(true);
      };

      // 1. Masked paste "A\nB" -> submits "A\nB"
      pasteInto('A\nB');
      await el.updateComplete;
      expect(
        (el.shadowRoot!.querySelector('input[data-key="MULTI_KEY"]') as HTMLInputElement).value
      ).toBe('');
      expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).not.toBeNull();
      let fulfilledCount = 0;
      el.addEventListener('secret-request-fulfilled', () => {
        fulfilledCount++;
      });
      (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
      await vi.waitFor(() => expect(fulfilledCount).toBe(1));
      await el.updateComplete;
      expect(
        JSON.parse((fetchMock.mock.calls[0] as [string, RequestInit])[1].body as string).values
      ).toEqual({
        MULTI_KEY: 'A\nB',
      });

      // 2. Masked paste "A\r\nB" -> submits "A\r\nB" unchanged
      pasteInto('A\r\nB');
      await el.updateComplete;
      (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
      await vi.waitFor(() => expect(fulfilledCount).toBe(2));
      await el.updateComplete;
      expect(
        JSON.parse((fetchMock.mock.calls[1] as [string, RequestInit])[1].body as string).values
      ).toEqual({
        MULTI_KEY: 'A\r\nB',
      });

      // 3. Reveal -> enter "A\nB" in textarea -> Hide -> password input is empty; typing "X" replaces (never "ABX")
      const toggleBtn = el.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement;
      toggleBtn.click();
      await el.updateComplete;
      const ta = el.shadowRoot!.querySelector(
        'textarea[data-key="MULTI_KEY"]'
      ) as HTMLTextAreaElement;
      expect(ta).not.toBeNull();
      ta.value = 'A\nB';
      ta.dispatchEvent(new Event('input'));
      await el.updateComplete;

      toggleBtn.click();
      await el.updateComplete;
      const hiddenInput = el.shadowRoot!.querySelector(
        'input[data-key="MULTI_KEY"]'
      ) as HTMLInputElement;
      expect(hiddenInput.value).toBe('');
      hiddenInput.value = 'X';
      hiddenInput.dispatchEvent(new Event('input'));
      await el.updateComplete;

      (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
      await vi.waitFor(() => expect(fulfilledCount).toBe(3));
      expect(
        JSON.parse((fetchMock.mock.calls[2] as [string, RequestInit])[1].body as string).values
      ).toEqual({
        MULTI_KEY: 'X',
      });
    }
  );

  it('clears form values when messageId changes', async () => {
    const el = await mountCard({ messageId: 'msg-a' });
    const input = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    input.value = 'secret-for-a';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(1);

    el.messageId = 'msg-b';
    el.rows = [makeRow({ rowId: 'row-1', messageId: 'msg-b' })];
    await el.updateComplete;

    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    expect(
      (el.shadowRoot!.querySelector('input[data-key="GITHUB_TOKEN"]') as HTMLInputElement).value
    ).toBe('');
    expect(el.shadowRoot!.querySelector('.submit-btn')?.hasAttribute('disabled')).toBe(true);
  });

  it('prunes in-memory form values when a live update resolves a pending row', async () => {
    const el = await mountCard();
    const input = el.shadowRoot!.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    input.value = 'typed-before-fulfilled';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(1);

    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          data: {
            kind: 'secret_request',
            messageId: 'msg-1',
            secret_request: makeRow({
              rowId: 'row-1',
              status: 'fulfilled',
              fulfilled: {
                at: '2026-11-24T00:05:00Z',
                byUserId: 'user-2',
                byName: 'Bob',
                surface: 'native',
              },
            }),
          },
        },
      })
    );
    await el.updateComplete;

    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
  });

  it('discards an async file read that completes after clearFormValues()', async () => {
    const el = await mountCard({
      rows: [makeRow({ rowId: 'r-file', key: 'DEPLOY_KEY', secretType: 'file' })],
    });
    const fileInput = el.shadowRoot!.querySelector(
      'input[type="file"][data-file-input="DEPLOY_KEY"]'
    ) as HTMLInputElement;

    let resolveBuf: ((buf: ArrayBuffer) => void) | null = null;
    const slowFile = new File(['delayed-secret'], 'slow.pem', { type: 'text/plain' });
    vi.spyOn(slowFile, 'arrayBuffer').mockImplementation(
      () =>
        new Promise<ArrayBuffer>((resolve) => {
          resolveBuf = resolve;
        })
    );
    Object.defineProperty(fileInput, 'files', { value: [slowFile], configurable: true });
    fileInput.dispatchEvent(new Event('change'));
    await new Promise((r) => setTimeout(r, 0));

    // Clear values (e.g. pagehide / submit / 403) before arrayBuffer resolves
    el.clearFormValues();
    resolveBuf!(new TextEncoder().encode('delayed-secret').buffer);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).toBeNull();
  });

  it('hides the blocked alert on closed cards and refetches canRespond when a submitting claim rolls back to pending', async () => {
    const fetchMock = vi.fn().mockImplementation(() =>
      Promise.resolve(
        jsonResponse({
          messageId: 'msg-1',
          projectId: 'proj-1',
          agentId: 'agent-1',
          agentName: 'builder-1',
          canRespond: true,
          reason: '',
          rows: [makeRow({ rowId: 'row-1', status: 'pending', submitting: false })],
        })
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    // Closed card with canRespond=false shows no blocked warning alert
    const closed = await mountCard({
      canRespond: false,
      reason: 'all secret requests on this card are closed',
      rows: [
        makeRow({
          rowId: 'row-1',
          status: 'fulfilled',
          fulfilled: {
            at: '2026-11-24T00:05:00Z',
            byUserId: 'user-2',
            byName: 'Alice',
            surface: 'native',
          },
        }),
      ],
    });
    expect(closed.shadowRoot!.querySelector('.status-alert.blocked')).toBeNull();

    // Card loaded while submitting (canRespond=false) refetches when claim rolls back to pending
    const rolling = await mountCard({
      canRespond: false,
      reason: 'all secret requests on this card are closed',
      rows: [makeRow({ rowId: 'row-1', status: 'pending', submitting: true })],
    });
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          data: {
            kind: 'secret_request',
            messageId: 'msg-1',
            secret_request: makeRow({ rowId: 'row-1', status: 'pending', submitting: false }),
          },
        },
      })
    );

    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/v1/secret-requests/msg-1',
        expect.objectContaining({ method: 'GET' })
      );
      expect(rolling.canRespond).toBe(true);
    });
    await rolling.updateComplete;
    expect(
      (rolling.shadowRoot!.querySelector('input[data-key="GITHUB_TOKEN"]') as HTMLInputElement)
        .disabled
    ).toBe(false);
  });

  it('composes type-then-paste at selection and captures beforeinput/drop multi-line text', async () => {
    const el = await mountCard({
      rows: [makeRow({ rowId: 'r-1', key: 'TOKEN', secretType: 'environment' })],
    });
    const input = el.shadowRoot!.querySelector('input[data-key="TOKEN"]') as HTMLInputElement;

    // 1. Single-line paste on non-captured row does NOT preventDefault (native browser compose)
    input.value = 'abc';
    input.dispatchEvent(new Event('input'));
    await el.updateComplete;
    const singlePaste = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(singlePaste, 'clipboardData', {
      value: { getData: (t: string) => (t === 'text/plain' ? 'def' : '') },
    });
    input.dispatchEvent(singlePaste);
    expect(singlePaste.defaultPrevented).toBe(false);

    // 2. Multi-line paste with caret in the middle composes at selection
    input.setSelectionRange(1, 1);
    const multiPaste = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(multiPaste, 'clipboardData', {
      value: { getData: (t: string) => (t === 'text/plain' ? 'X\nY' : '') },
    });
    input.dispatchEvent(multiPaste);
    await el.updateComplete;
    expect(multiPaste.defaultPrevented).toBe(true);
    expect((el as unknown as { formValues: Map<string, string> }).formValues.get('r-1')).toBe(
      'aX\nYbc'
    );

    // 3. beforeinput insertText with \r\n replaces captured value verbatim
    const bi = new InputEvent('beforeinput', {
      bubbles: true,
      cancelable: true,
      inputType: 'insertText',
      data: 'Y1\r\nY2',
    });
    input.dispatchEvent(bi);
    await el.updateComplete;
    expect(bi.defaultPrevented).toBe(true);
    expect((el as unknown as { formValues: Map<string, string> }).formValues.get('r-1')).toBe(
      'Y1\r\nY2'
    );

    // 4. drop event with multi-line text replaces captured value verbatim
    const dropEv = new Event('drop', { bubbles: true, cancelable: true }) as DragEvent;
    Object.defineProperty(dropEv, 'dataTransfer', {
      value: { getData: (t: string) => (t === 'text/plain' ? 'D1\nD2\nD3' : '') },
    });
    input.dispatchEvent(dropEv);
    await el.updateComplete;
    expect(dropEv.defaultPrevented).toBe(true);
    expect((el as unknown as { formValues: Map<string, string> }).formValues.get('r-1')).toBe(
      'D1\nD2\nD3'
    );
  });

  it('clears a captured value on Backspace / delete beforeinput and disables Submit', async () => {
    const el = await mountCard({
      rows: [makeRow({ rowId: 'r-1', key: 'TOKEN', secretType: 'file' })],
    });
    const input = el.shadowRoot!.querySelector('input[data-key="TOKEN"]') as HTMLInputElement;
    const paste = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(paste, 'clipboardData', {
      value: { getData: (t: string) => (t === 'text/plain' ? 'line1\nline2' : '') },
    });
    input.dispatchEvent(paste);
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.submit-btn')?.hasAttribute('disabled')).toBe(false);

    // Clear via deleteContentBackward beforeinput
    const delEv = new InputEvent('beforeinput', {
      bubbles: true,
      cancelable: true,
      inputType: 'deleteContentBackward',
    });
    input.dispatchEvent(delEv);
    await el.updateComplete;

    expect(delEv.defaultPrevented).toBe(true);
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('.submit-btn')?.hasAttribute('disabled')).toBe(true);

    // Re-paste and clear via Backspace keydown (for browsers that don't fire beforeinput on empty input)
    input.dispatchEvent(paste);
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).not.toBeNull();
    const kd = new KeyboardEvent('keydown', { key: 'Backspace', bubbles: true, cancelable: true });
    input.dispatchEvent(kd);
    await el.updateComplete;
    expect(kd.defaultPrevented).toBe(true);
    expect((el as unknown as { formValues: Map<string, string> }).formValues.size).toBe(0);
    expect(el.shadowRoot!.querySelector('[data-testid="file-value-masked"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('.submit-btn')?.hasAttribute('disabled')).toBe(true);
  });

  it('preserves a leading UTF-8 BOM verbatim when loading and submitting a file', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        rows: [
          makeRow({
            rowId: 'r-file',
            key: 'BOM_KEY',
            secretType: 'file',
            status: 'fulfilled',
            fulfilled: {
              at: '2026-11-24T00:05:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              surface: 'native',
            },
          }),
        ],
        responseMessageId: 'resp-bom',
        replayed: false,
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({
      rows: [makeRow({ rowId: 'r-file', key: 'BOM_KEY', secretType: 'file' })],
    });
    const fileInput = el.shadowRoot!.querySelector(
      'input[type="file"][data-file-input="BOM_KEY"]'
    ) as HTMLInputElement;
    // EF BB BF + "abc\n"
    const bomBytes = new Uint8Array([0xef, 0xbb, 0xbf, 0x61, 0x62, 0x63, 0x0a]);
    const bomFile = new File([bomBytes], 'bom.txt', { type: 'text/plain' });
    Object.defineProperty(fileInput, 'files', { value: [bomFile], configurable: true });
    fileInput.dispatchEvent(new Event('change'));
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
    });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const sent = JSON.parse(init.body as string) as { values: Record<string, string> };
    expect(sent.values).toEqual({ BOM_KEY: '\uFEFFabc\n' });
  });

  it('moves focus to the revealed textarea when Show is clicked', async () => {
    const el = await mountCard();
    const toggleBtn = el.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement;
    toggleBtn.click();
    await el.updateComplete;

    const ta = el.shadowRoot!.querySelector(
      'textarea[data-key="GITHUB_TOKEN"]'
    ) as HTMLTextAreaElement;
    expect(ta).not.toBeNull();
    expect(el.shadowRoot!.activeElement).toBe(ta);
  });

  it('preserves CRLF line endings when editing a revealed all-CRLF value', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        rows: [
          makeRow({
            rowId: 'row-1',
            key: 'CRLF_KEY',
            secretType: 'file',
            status: 'fulfilled',
            fulfilled: {
              at: '2026-11-24T00:05:00Z',
              byUserId: 'user-1',
              byName: 'Me',
              surface: 'native',
            },
          }),
        ],
        responseMessageId: 'resp-crlf',
        replayed: false,
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = await mountCard({
      rows: [makeRow({ rowId: 'row-1', key: 'CRLF_KEY', secretType: 'file' })],
    });
    const input = el.shadowRoot!.querySelector('input[data-key="CRLF_KEY"]') as HTMLInputElement;
    const pasteEv = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(pasteEv, 'clipboardData', {
      value: { getData: () => 'AAA-l1\r\nBBB-l2\r\nCCC-l3' },
    });
    input.dispatchEvent(pasteEv);
    await el.updateComplete;

    const toggleBtn = el.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement;
    toggleBtn.click();
    await el.updateComplete;

    const ta = el.shadowRoot!.querySelector('textarea[data-key="CRLF_KEY"]') as HTMLTextAreaElement;
    expect(ta.readOnly).toBe(false);
    expect(el.shadowRoot!.querySelector('[data-testid="mixed-line-endings-note"]')).toBeNull();

    // Simulate browser HTML textarea LF-normalized edit
    ta.value = 'AAA-l1\nBBB-l2\nCCC-l3X';
    ta.dispatchEvent(new Event('input', { bubbles: true }));
    await el.updateComplete;

    toggleBtn.click();
    await el.updateComplete;

    (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
    });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const sent = JSON.parse(init.body as string) as { values: Record<string, string> };
    expect(sent.values).toEqual({ CRLF_KEY: 'AAA-l1\r\nBBB-l2\r\nCCC-l3X' });
  });

  it('renders revealed textarea readonly with note for mixed line endings or bare CR and submits unchanged', async () => {
    for (const [label, rawValue] of [
      ['mixed', 'a\r\nb\nc'],
      ['bare-cr', 'a\rb'],
    ] as const) {
      const fetchMock = vi.fn().mockResolvedValue(
        jsonResponse({
          rows: [
            makeRow({
              rowId: 'row-1',
              key: 'MIXED_KEY',
              secretType: 'environment',
              status: 'fulfilled',
              fulfilled: {
                at: '2026-11-24T00:05:00Z',
                byUserId: 'user-1',
                byName: 'Me',
                surface: 'native',
              },
            }),
          ],
          responseMessageId: `resp-${label}`,
          replayed: false,
        })
      );
      vi.stubGlobal('fetch', fetchMock);

      const el = await mountCard({
        rows: [makeRow({ rowId: 'row-1', key: 'MIXED_KEY', secretType: 'environment' })],
      });
      const input = el.shadowRoot!.querySelector('input[data-key="MIXED_KEY"]') as HTMLInputElement;
      const pasteEv = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
      Object.defineProperty(pasteEv, 'clipboardData', {
        value: { getData: () => rawValue },
      });
      input.dispatchEvent(pasteEv);
      await el.updateComplete;

      const toggleBtn = el.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement;
      toggleBtn.click();
      await el.updateComplete;

      const ta = el.shadowRoot!.querySelector(
        'textarea[data-key="MIXED_KEY"]'
      ) as HTMLTextAreaElement;
      expect(ta.readOnly).toBe(true);
      const note = el.shadowRoot!.querySelector('[data-testid="mixed-line-endings-note"]');
      expect(note?.textContent?.trim()).toBe(
        'This value has mixed line endings; paste or load to replace it'
      );

      // Even if a synthetic input event fires, the stored value is not altered
      ta.value = 'corrupted';
      ta.dispatchEvent(new Event('input', { bubbles: true }));
      await el.updateComplete;

      toggleBtn.click();
      await el.updateComplete;

      (el.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
      await vi.waitFor(() => {
        expect(el.shadowRoot!.textContent).toContain('Fulfilled by you');
      });
      const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
      const sent = JSON.parse(init.body as string) as { values: Record<string, string> };
      expect(sent.values).toEqual({ MIXED_KEY: rawValue });
      el.remove();
    }
  });

  it('preserves CRLF pasted into an empty revealed textarea and transitions LF + CRLF paste to mixed readonly', async () => {
    const fetchMock = vi.fn().mockImplementation(() =>
      Promise.resolve(
        jsonResponse({
          rows: [
            makeRow({
              rowId: 'row-1',
              key: 'REV_PASTE',
              secretType: 'variable',
              status: 'fulfilled',
              fulfilled: {
                at: '2026-11-24T00:05:00Z',
                byUserId: 'user-1',
                byName: 'Me',
                surface: 'native',
              },
            }),
          ],
          responseMessageId: 'resp-rev-paste',
          replayed: false,
        })
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    // 1. Empty row -> Show -> paste CRLF -> preserves CRLF
    const el1 = await mountCard({
      rows: [makeRow({ rowId: 'row-1', key: 'REV_PASTE', secretType: 'variable' })],
    });
    (el1.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement).click();
    await el1.updateComplete;
    const ta1 = el1.shadowRoot!.querySelector(
      'textarea[data-key="REV_PASTE"]'
    ) as HTMLTextAreaElement;
    const paste1 = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(paste1, 'clipboardData', {
      value: { getData: () => 'A\r\nB\r\nC' },
    });
    ta1.dispatchEvent(paste1);
    await el1.updateComplete;
    expect(paste1.defaultPrevented).toBe(true);
    expect(ta1.readOnly).toBe(false);
    (el1.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(el1.shadowRoot!.textContent).toContain('Fulfilled by you');
    });
    const sent1 = JSON.parse(
      (fetchMock.mock.calls[0] as [string, RequestInit])[1].body as string
    ) as {
      values: Record<string, string>;
    };
    expect(sent1.values).toEqual({ REV_PASTE: 'A\r\nB\r\nC' });
    el1.remove();

    // 2. LF value -> Show -> paste "\r\nX" at end -> becomes mixed readonly and submits verbatim
    const el2 = await mountCard({
      rows: [makeRow({ rowId: 'row-1', key: 'REV_PASTE', secretType: 'variable' })],
    });
    const input2 = el2.shadowRoot!.querySelector('input[data-key="REV_PASTE"]') as HTMLInputElement;
    const lfPaste = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(lfPaste, 'clipboardData', { value: { getData: () => 'L1\nL2' } });
    input2.dispatchEvent(lfPaste);
    await el2.updateComplete;

    (el2.shadowRoot!.querySelector('.toggle-visibility') as HTMLButtonElement).click();
    await el2.updateComplete;
    const ta2 = el2.shadowRoot!.querySelector(
      'textarea[data-key="REV_PASTE"]'
    ) as HTMLTextAreaElement;
    ta2.setSelectionRange(ta2.value.length, ta2.value.length);
    const crlfAppend = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(crlfAppend, 'clipboardData', { value: { getData: () => '\r\nX' } });
    ta2.dispatchEvent(crlfAppend);
    await el2.updateComplete;
    expect(crlfAppend.defaultPrevented).toBe(true);
    expect(ta2.readOnly).toBe(true);
    expect(el2.shadowRoot!.querySelector('[data-testid="mixed-line-endings-note"]')).not.toBeNull();

    (el2.shadowRoot!.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(el2.shadowRoot!.textContent).toContain('Fulfilled by you');
    });
    const sent2 = JSON.parse(
      (fetchMock.mock.calls[1] as [string, RequestInit])[1].body as string
    ) as {
      values: Record<string, string>;
    };
    expect(sent2.values).toEqual({ REV_PASTE: 'L1\nL2\r\nX' });
    el2.remove();
  });
});
