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
 * Tests for secret-list.ts — specifically for the base64-encode-before-send
 * behaviour introduced to fix the silent 400 regression (issue #251).
 *
 * Root cause: the backend began requiring base64-encoded secret values in
 * commit 579c0dcc, but the web UI was sending raw text.  The fix encodes
 * dialogValue with TextEncoder → btoa before sending.
 *
 * These tests assert:
 *  1. Any value typed in the dialog is transmitted base64-encoded.
 *  2. Non-ASCII / Unicode input is encoded correctly (UTF-8 safe).
 *  3. The PUT body structure matches what the API expects.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionSecretList: any;

/** Encode a string exactly as secret-list.ts encodes it. */
function encodeValue(raw: string): string {
  return btoa(Array.from(new TextEncoder().encode(raw), (b) => String.fromCharCode(b)).join(''));
}

/** Create the component, append to DOM, and wait for render. */
async function createComponent(
  fetchMock: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
) {
  vi.stubGlobal('fetch', vi.fn(fetchMock));
  const el = document.createElement('scion-secret-list') as InstanceType<typeof ScionSecretList>;
  el.setAttribute('scope', 'user');
  el.setAttribute('apiBasePath', '/api/v1');
  document.body.appendChild(el);
  await el.updateComplete;
  // Let async loadSecrets() settle.
  await new Promise((r) => setTimeout(r, 50));
  await el.updateComplete;
  return el;
}

/** Minimal fetch mock: return empty list for GETs, 200 for PUTs. */
function makeBasicFetch(putSpy?: (body: Record<string, unknown>) => void) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const method = init?.method ?? 'GET';
    if (method === 'PUT') {
      if (init?.body) {
        try {
          const parsed = JSON.parse(init.body as string) as Record<string, unknown>;
          putSpy?.(parsed);
        } catch {
          /* ignore parse errors in the mock */
        }
      }
      return Promise.resolve(
        new Response(JSON.stringify({ secret: { key: 'TEST', version: 1 }, created: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }
    // GET /api/v1/secrets — return empty list.
    return Promise.resolve(
      new Response(JSON.stringify({ secrets: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

describe('scion-secret-list — base64 encoding before send (issue #251)', () => {
  beforeAll(async () => {
    const mod = await import('./secret-list.js');
    ScionSecretList = mod.ScionSecretList;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('sends a base64-encoded value, not raw text', async () => {
    const rawValue = 'my-plain-secret';
    let capturedBody: Record<string, unknown> = {};

    const el = await createComponent(
      makeBasicFetch((body) => {
        capturedBody = body;
      })
    );

    // Simulate the user opening the dialog and typing a value.
    (el as any).dialogKey = 'MY_SECRET';
    (el as any).dialogValue = rawValue;
    (el as any).dialogMode = 'create';

    await (el as any).handleSave(new Event('submit'));

    expect(capturedBody.value).toBeDefined();
    // The transmitted value must be base64-encoded, not the raw string.
    expect(capturedBody.value).not.toBe(rawValue);
    expect(capturedBody.value).toBe(encodeValue(rawValue));
    // And decoding it must yield the original value.
    expect(atob(capturedBody.value as string)).toBe(rawValue);
  });

  it('encodes Unicode / non-ASCII secrets correctly (UTF-8 safe)', async () => {
    // Characters outside Latin-1: bare btoa() would throw on these.
    const unicodeValue = 'こんにちは🔑secret';
    let capturedBody: Record<string, unknown> = {};

    const el = await createComponent(
      makeBasicFetch((body) => {
        capturedBody = body;
      })
    );

    (el as any).dialogKey = 'UNICODE_KEY';
    (el as any).dialogValue = unicodeValue;
    (el as any).dialogMode = 'create';

    await (el as any).handleSave(new Event('submit'));

    expect(capturedBody.value).toBeDefined();
    expect(capturedBody.value).not.toBe(unicodeValue);
    // Verify the encoding matches the expected UTF-8 safe base64.
    expect(capturedBody.value).toBe(encodeValue(unicodeValue));
  });

  it('sends empty value validation error (no network call) for empty dialogValue', async () => {
    const putSpy = vi.fn();

    const el = await createComponent(makeBasicFetch(putSpy));

    (el as any).dialogKey = 'KEY';
    (el as any).dialogValue = '';
    (el as any).dialogMode = 'create';

    await (el as any).handleSave(new Event('submit'));

    // The handler should have set dialogError and NOT made a network call.
    expect((el as any).dialogError).toBeTruthy();
    expect(putSpy).not.toHaveBeenCalled();
  });

  it('PUT body includes scope, type, injectionMode alongside the encoded value', async () => {
    const rawValue = 'api-key-value';
    let capturedBody: Record<string, unknown> = {};

    const el = await createComponent(
      makeBasicFetch((body) => {
        capturedBody = body;
      })
    );

    (el as any).dialogKey = 'API_KEY';
    (el as any).dialogValue = rawValue;
    (el as any).dialogType = 'environment';
    (el as any).dialogInjectionMode = 'as_needed';
    (el as any).dialogMode = 'create';

    await (el as any).handleSave(new Event('submit'));

    expect(capturedBody.value).toBe(encodeValue(rawValue));
    expect(capturedBody.scope).toBe('user');
    expect(capturedBody.type).toBe('environment');
    expect(capturedBody.injectionMode).toBe('as_needed');
  });
});
