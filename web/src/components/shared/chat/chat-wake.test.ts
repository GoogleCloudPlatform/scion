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

const { confirmWake, errorMessageFromBody, wakeConfirmMessage, wakeOfferFromErrorBody } =
  await import('./chat-wake.js');

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
