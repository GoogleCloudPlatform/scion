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

/** Login page sign-in denial messages (ptone/scion#3330). */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import './login.js';
import type { ScionLoginPage } from './login.js';

beforeEach(() => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({ providers: [{ id: 'google', name: 'Google', enabled: true }] }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
    )
  );
});

afterEach(() => {
  document.body.innerHTML = '';
  window.history.replaceState(null, '', '/');
  vi.unstubAllGlobals();
});

async function mountWithError(code: string): Promise<ScionLoginPage> {
  window.history.replaceState(null, '', `/login?error=${encodeURIComponent(code)}`);
  const el = document.createElement('scion-login-page');
  document.body.appendChild(el);
  await vi.waitFor(() => {
    expect(el.shadowRoot!.querySelector('.error-message')).not.toBeNull();
  });
  return el;
}

function errorText(el: ScionLoginPage): string {
  return el.shadowRoot!.querySelector('.error-message')!.textContent!.trim();
}

describe('scion-login-page sign-in denial messages', () => {
  it('shows the invite-only message for error=invite_only', async () => {
    const el = await mountWithError('invite_only');
    expect(errorText(el)).toBe('This hub is invite-only. Ask an administrator for an invite.');
  });

  it('shows the domain message for error=unauthorized_domain', async () => {
    const el = await mountWithError('unauthorized_domain');
    expect(errorText(el)).toBe('Your email domain is not authorized to access this application.');
  });
});
