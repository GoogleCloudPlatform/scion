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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageProfileSlack } from './profile-slack.js';

describe('scion-page-profile-slack', () => {
  beforeAll(async () => {
    await import('./profile-slack.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    window.history.replaceState({}, '', '/profile/slack');
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('renders manual linking instructions and submits POST /api/v1/slack/link/verify', async () => {
    window.history.replaceState({}, '', '/profile/slack');
    const fetchMock = vi.fn(
      (_url: string | URL | Request, _init?: RequestInit): Promise<Response> =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              status: 'confirmed',
              slackUserId: 'U12345',
              user: { id: 'u1', email: 'alice@example.com' },
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        )
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-page-profile-slack') as ScionPageProfileSlack;
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot!.textContent).toContain('/scion register');
    expect(fetchMock).not.toHaveBeenCalled();

    const input = el.shadowRoot!.querySelector('sl-input') as HTMLElement & { value: string };
    expect(input).not.toBeNull();
    input.value = 'ab-12c9';
    input.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(input.value).toBe('AB12C9');

    const form = el.shadowRoot!.querySelector('form.code-form') as HTMLFormElement;
    form.dispatchEvent(new Event('submit', { cancelable: true }));
    await vi.waitFor(() => {
      expect(el.shadowRoot!.querySelector('.result-success')).not.toBeNull();
    });

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/slack/link/verify',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ code: 'AB12C9' }),
      })
    );
    expect(el.shadowRoot!.querySelector('.result-success')!.textContent).toContain(
      'Slack account linked successfully!'
    );
  });

  it('pre-fills ?code= without auto-submitting on mount and ignores spoofable ?user_name=', async () => {
    window.history.replaceState({}, '', '/profile/slack?code=xy78z2&user_name=spoofed_handle');
    const fetchMock = vi.fn(
      (_url: string | URL | Request, _init?: RequestInit): Promise<Response> =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              status: 'confirmed',
              slackUserId: 'U12345',
              user: { id: 'u1', email: 'alice@example.com' },
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        )
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-page-profile-slack') as ScionPageProfileSlack;
    document.body.appendChild(el);
    await el.updateComplete;

    // Must NOT auto-submit on GET navigation (prevents cross-site link-jacking).
    expect(fetchMock).not.toHaveBeenCalled();
    // Must NOT display untrusted ?user_name= query param.
    expect(el.shadowRoot!.textContent).not.toContain('spoofed_handle');
    // Must pre-fill sanitized code and show explicit confirmation prompt.
    const input = el.shadowRoot!.querySelector('sl-input') as HTMLElement & { value: string };
    expect(input).not.toBeNull();
    expect(input.value).toBe('XY78Z2');
    expect(el.shadowRoot!.querySelector('.confirm-notice')).not.toBeNull();

    // User clicks submit to confirm.
    const form = el.shadowRoot!.querySelector('form.code-form') as HTMLFormElement;
    form.dispatchEvent(new Event('submit', { cancelable: true }));
    await vi.waitFor(() => {
      expect(el.shadowRoot!.querySelector('.result-success')).not.toBeNull();
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/slack/link/verify',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ code: 'XY78Z2' }),
      })
    );
  });

  it('parses Hub error envelope on failure and keeps the form usable for retry', async () => {
    window.history.replaceState({}, '', '/profile/slack?code=BAD999');
    const fetchMock = vi
      .fn<(_url: string | URL | Request, _init?: RequestInit) => Promise<Response>>()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            error: { code: 'not_found', message: 'code has expired' },
          }),
          { status: 410, headers: { 'Content-Type': 'application/json' } }
        )
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            error: { code: 'rate_limited', message: 'too many verify attempts, try again later' },
          }),
          { status: 429, headers: { 'Content-Type': 'application/json' } }
        )
      );
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-page-profile-slack') as ScionPageProfileSlack;
    document.body.appendChild(el);
    await el.updateComplete;

    expect(fetchMock).not.toHaveBeenCalled();

    const form = el.shadowRoot!.querySelector('form.code-form') as HTMLFormElement;
    form.dispatchEvent(new Event('submit', { cancelable: true }));
    await vi.waitFor(() => {
      expect(el.shadowRoot!.querySelector('.result-error')!.textContent).toContain(
        'code has expired'
      );
    });

    // Form stays visible so the user can enter a new code and retry.
    const input = el.shadowRoot!.querySelector('sl-input') as HTMLElement & { value: string };
    expect(input).not.toBeNull();
    input.value = 'NEW456';
    input.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;

    form.dispatchEvent(new Event('submit', { cancelable: true }));
    await vi.waitFor(() => {
      expect(el.shadowRoot!.querySelector('.result-error')!.textContent).toContain(
        'too many verify attempts, try again later'
      );
    });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock).toHaveBeenLastCalledWith(
      '/api/v1/slack/link/verify',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ code: 'NEW456' }),
      })
    );
  });
});
