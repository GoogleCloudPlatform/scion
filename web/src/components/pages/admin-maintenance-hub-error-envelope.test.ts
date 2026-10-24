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

import { describe, it, expect } from 'vitest';

// hubErrorEnvelope tells the hub's own refusal ({error: {code, message}})
// apart from every other failed body. A false positive would cancel the
// Restart Hub wait while the hub is really restarting (for example on a
// JSON error page from a front proxy).

import { hubErrorEnvelope } from './admin-maintenance.js';

function jsonResponse(body: unknown, status = 409): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function textResponse(text: string, status = 502): Response {
  return new Response(text, { status, headers: { 'Content-Type': 'text/html' } });
}

describe('hubErrorEnvelope', () => {
  it('returns code and message for the hub envelope', async () => {
    const res = jsonResponse({
      error: { code: 'conflict', message: 'update-binary is in progress', details: { runId: 'r' } },
    });
    expect(await hubErrorEnvelope(res)).toEqual({
      code: 'conflict',
      message: 'update-binary is in progress',
    });
  });

  it.each([
    ['no message', { error: { code: 'internal_error' } }],
    ['an empty message', { error: { code: 'internal_error', message: '' } }],
    ['a non-string message', { error: { code: 'internal_error', message: 7 } }],
  ])('falls back to the code for %s', async (_name, body) => {
    expect(await hubErrorEnvelope(jsonResponse(body, 500))).toEqual({
      code: 'internal_error',
      message: 'internal_error',
    });
  });

  it.each<[string, Response]>([
    ['a string error', jsonResponse({ error: 'Bad Gateway' }, 502)],
    ['an empty error object', jsonResponse({ error: {} }, 502)],
    ['an empty code', jsonResponse({ error: { code: '', message: 'x' } }, 502)],
    ['a numeric code', jsonResponse({ error: { code: 5, message: 'x' } }, 502)],
    ['a null error', jsonResponse({ error: null }, 502)],
    ['a JSON proxy error body', jsonResponse({ message: 'Bad Gateway' }, 502)],
    ['JSON null', jsonResponse(null, 502)],
    ['a JSON string', jsonResponse('Bad Gateway', 502)],
    ['an HTML page', textResponse('<html>502 Bad Gateway</html>')],
    ['an empty body', textResponse('')],
  ])('returns null for %s', async (_name, res) => {
    expect(await hubErrorEnvelope(res)).toBeNull();
  });

  it('returns null when the body stalls past the timeout', async () => {
    const stalled = new ReadableStream<Uint8Array>({
      start(controller): void {
        controller.enqueue(new TextEncoder().encode('{"error":'));
        // Never closes.
      },
    });
    const res = new Response(stalled, {
      status: 409,
      headers: { 'Content-Type': 'application/json' },
    });
    expect(await hubErrorEnvelope(res, 20)).toBeNull();
  });
});
