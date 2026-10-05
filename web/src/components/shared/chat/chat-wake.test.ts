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

import { describe, expect, it, vi } from 'vitest';

const showConfirm = vi.fn<(message: string, options?: unknown) => Promise<boolean>>();
vi.mock('../confirm-dialog.js', () => ({
  showConfirm: (message: string, options?: unknown) => showConfirm(message, options),
}));

const {
  confirmWake,
  WAKE_CONFIRM_MAX_MS,
  errorMessageFromBody,
  isGatewayDrop,
  isSendInProgressBody,
  jsonResponse,
  saveDraftForConversation,
  wakeConfirmMessage,
  wakeConfirmBudgetMs,
  wakeOfferFromErrorBody,
} = await import('./chat-wake.js');

describe('wakeOfferFromErrorBody', () => {
  it('reads an explicit wake offer', () => {
    expect(
      wakeOfferFromErrorBody({
        error: {
          code: 'agent_not_running',
          details: { agentId: 'a-1', agentSlug: 'sleepy', canWake: true },
        },
      })
    ).toEqual({ agentId: 'a-1', agentSlug: 'sleepy' });
  });

  it('fails closed without canWake === true', () => {
    for (const canWake of [false, 'true', undefined]) {
      expect(
        wakeOfferFromErrorBody({ error: { code: 'agent_not_running', details: { canWake } } })
      ).toBeNull();
    }
  });

  it('ignores other error codes and malformed bodies', () => {
    expect(
      wakeOfferFromErrorBody({ error: { code: 'conflict', details: { canWake: true } } })
    ).toBeNull();
    expect(wakeOfferFromErrorBody(null)).toBeNull();
    expect(wakeOfferFromErrorBody({ error: 'nope' })).toBeNull();
  });
});

describe('errorMessageFromBody', () => {
  it('prefers the structured message, then a string error, then the fallback', () => {
    expect(errorMessageFromBody({ error: { message: 'boom' } }, 'fb')).toBe('boom');
    expect(errorMessageFromBody({ error: 'plain' }, 'fb')).toBe('plain');
    expect(errorMessageFromBody(null, 'fb')).toBe('fb');
  });
});

describe('confirmWake', () => {
  it('names the agent and offers "Wake and send" or Cancel', async () => {
    showConfirm.mockResolvedValueOnce(true);
    await expect(confirmWake({ agentId: 'a-1', agentSlug: 'sleepy' })).resolves.toBe(true);
    const [message, options] = showConfirm.mock.calls[0]!;
    expect(message).toBe(wakeConfirmMessage({ agentId: 'a-1', agentSlug: 'sleepy' }));
    expect(message).toContain('@sleepy is suspended');
    expect(options).toMatchObject({ confirmText: 'Wake and send', cancelText: 'Cancel' });
  });

  it('falls back to a generic name without a slug', () => {
    expect(wakeConfirmMessage({ agentId: 'a-1', agentSlug: '' })).toMatch(/^This agent is/);
  });
});

describe('saveDraftForConversation', () => {
  it('saves into an empty draft slot and never overwrites another draft', () => {
    localStorage.removeItem('scion-chat-draft-c1');
    expect(saveDraftForConversation('c1', 'first')).toBe(true);
    expect(localStorage.getItem('scion-chat-draft-c1')).toBe('first');
    expect(saveDraftForConversation('c1', 'second')).toBe(false);
    expect(localStorage.getItem('scion-chat-draft-c1')).toBe('first');
    localStorage.removeItem('scion-chat-draft-c1');
  });

  it('ignores an empty key or text', () => {
    expect(saveDraftForConversation('', 'x')).toBe(false);
    expect(saveDraftForConversation('c1', '')).toBe(false);
  });
});

describe('isSendInProgressBody', () => {
  it('recognises only the send_in_progress code', () => {
    expect(isSendInProgressBody({ error: { code: 'send_in_progress' } })).toBe(true);
    expect(isSendInProgressBody({ error: { code: 'agent_not_running' } })).toBe(false);
    expect(isSendInProgressBody({ error: 'send_in_progress' })).toBe(false);
    expect(isSendInProgressBody(null)).toBe(false);
  });
});

describe('jsonResponse', () => {
  it('rebuilds a readable JSON response with the given status', async () => {
    const res = jsonResponse({ error: { code: 'conflict' } }, 409);
    expect(res.status).toBe(409);
    expect(res.ok).toBe(false);
    await expect(res.json()).resolves.toEqual({ error: { code: 'conflict' } });
  });
});

describe('wakeConfirmBudgetMs', () => {
  it("is the hub's budget for the recipients plus a margin", () => {
    // hub: 90s + n * 30s + 30s; margin 30s
    expect(wakeConfirmBudgetMs(1)).toBe(180_000);
    expect(wakeConfirmBudgetMs(2)).toBe(210_000);
    expect(wakeConfirmBudgetMs(0)).toBe(wakeConfirmBudgetMs(1));
  });

  it('stays below the 5 minute idempotency TTL', () => {
    expect(WAKE_CONFIRM_MAX_MS).toBeLessThan(5 * 60_000);
    expect(wakeConfirmBudgetMs(20)).toBe(WAKE_CONFIRM_MAX_MS);
  });
});

describe('isGatewayDrop', () => {
  it('treats a gateway status without a hub error as a drop', () => {
    expect(isGatewayDrop(502, null)).toBe(true);
    expect(isGatewayDrop(504, { message: 'gateway timeout' })).toBe(true);
    expect(isGatewayDrop(503, { error: 'plain' })).toBe(true);
  });

  it("returns the hub's own structured answers", () => {
    expect(isGatewayDrop(502, { error: { code: 'runtime_error', message: 'x' } })).toBe(false);
    expect(isGatewayDrop(503, { error: { code: 'unavailable' } })).toBe(false);
  });

  it('ignores other statuses', () => {
    expect(isGatewayDrop(500, null)).toBe(false);
    expect(isGatewayDrop(409, null)).toBe(false);
  });
});
