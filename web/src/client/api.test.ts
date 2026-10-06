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

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import type { AccessDeniedDetail } from './api.js';
import {
  apiErrorMessageFromBody,
  apiFetch,
  extractApiError,
  _resetSuspendedState,
  ACCESS_DENIED_UNREADABLE_REASON,
} from './api.js';

/**
 * Build a fake Response with the given status and JSON body.
 */
function fakeResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('apiFetch — 403 access-denied event', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let captured: AccessDeniedDetail[];

  function listener(e: Event) {
    captured.push((e as CustomEvent<AccessDeniedDetail>).detail);
  }

  beforeEach(() => {
    captured = [];
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    window.addEventListener('scion:access-denied', listener);
  });

  afterEach(() => {
    window.removeEventListener('scion:access-denied', listener);
    vi.restoreAllMocks();
  });

  // -----------------------------------------------------------------------
  // Canonical structured 403 with details
  // -----------------------------------------------------------------------

  it('parses canonical structured 403 with resource_type and denied_action', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
          details: {
            resource_type: 'agent',
            denied_action: 'delete',
          },
        },
      })
    );

    const res = await apiFetch('/api/v1/agents/test');
    expect(res.status).toBe(403);

    // Event fires exactly once.
    expect(captured).toHaveLength(1);
    const detail = captured[0];
    expect(detail.action).toBe('delete');
    expect(detail.resource).toBe('agent');
    expect(detail.reason).toBe('Insufficient permissions');
  });

  it('parses structured 403 with custom message and details', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Agents can only create sub-agents within their own project',
          details: {
            resource_type: 'agent',
            denied_action: 'create',
          },
        },
      })
    );

    const res = await apiFetch('/api/v1/agents');
    expect(res.status).toBe(403);
    expect(captured).toHaveLength(1);
    expect(captured[0].action).toBe('create');
    expect(captured[0].resource).toBe('agent');
    expect(captured[0].reason).toBe('Agents can only create sub-agents within their own project');
  });

  it('parses structured 403 with partial details (only resource_type)', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
          details: { resource_type: 'project' },
        },
      })
    );

    await apiFetch('/api/v1/projects/x');
    expect(captured).toHaveLength(1);
    // action falls back to error.code when denied_action is absent.
    expect(captured[0].action).toBe('forbidden');
    expect(captured[0].resource).toBe('project');
  });

  it('parses structured 403 with partial details (only denied_action)', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
          details: { denied_action: 'manage' },
        },
      })
    );

    await apiFetch('/api/v1/settings');
    expect(captured).toHaveLength(1);
    expect(captured[0].action).toBe('manage');
    expect(captured[0].resource).toBeUndefined();
  });

  // -----------------------------------------------------------------------
  // Legacy / generic 403 — graceful degradation
  // -----------------------------------------------------------------------

  it('degrades gracefully for legacy 403 with no details field', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
        },
      })
    );

    await apiFetch('/api/v1/admin/config');
    expect(captured).toHaveLength(1);
    // action falls back to code; resource undefined (no details).
    expect(captured[0].action).toBe('forbidden');
    expect(captured[0].resource).toBeUndefined();
    expect(captured[0].reason).toBe('Insufficient permissions');
  });

  it('degrades gracefully for flat {message} body', async () => {
    fetchMock.mockResolvedValue(fakeResponse(403, { message: 'Go away' }));

    await apiFetch('/api/v1/old');
    expect(captured).toHaveLength(1);
    expect(captured[0].reason).toBe('Go away');
    expect(captured[0].action).toBeUndefined();
    expect(captured[0].resource).toBeUndefined();
  });

  it('degrades gracefully for flat {error: "string"} body', async () => {
    fetchMock.mockResolvedValue(fakeResponse(403, { error: 'Nope' }));

    await apiFetch('/api/v1/legacy');
    expect(captured).toHaveLength(1);
    expect(captured[0].reason).toBe('Nope');
  });

  it('degrades gracefully for non-JSON 403 body', async () => {
    fetchMock.mockResolvedValue(
      new Response('Forbidden', {
        status: 403,
        headers: { 'Content-Type': 'text/plain' },
      })
    );

    await apiFetch('/api/v1/plain');
    expect(captured).toHaveLength(1);
    // Empty detail — parsing failed gracefully.
    expect(captured[0]).toEqual({});
  });

  it('degrades gracefully for empty JSON object', async () => {
    fetchMock.mockResolvedValue(fakeResponse(403, {}));

    await apiFetch('/api/v1/empty');
    expect(captured).toHaveLength(1);
    expect(captured[0].reason).toBe('Access denied');
  });

  // -----------------------------------------------------------------------
  // Malformed / hostile details
  // -----------------------------------------------------------------------

  it('handles details with hostile strings without error', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
          details: {
            resource_type: '<script>alert(1)</script>',
            denied_action: '<img onerror=alert(1)>',
          },
        },
      })
    );

    await apiFetch('/api/v1/xss');
    expect(captured).toHaveLength(1);
    // Hostile strings pass through as literal text.
    expect(captured[0].action).toBe('<img onerror=alert(1)>');
    expect(captured[0].resource).toBe('<script>alert(1)</script>');
  });

  it('handles details with non-string values gracefully', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
          details: {
            resource_type: 42,
            denied_action: true,
          },
        },
      })
    );

    await apiFetch('/api/v1/bad-types');
    expect(captured).toHaveLength(1);
    // Values pass through — the formatter handles non-string gracefully.
    expect(captured[0].action).toBe(true);
    expect(captured[0].resource).toBe(42);
  });

  // -----------------------------------------------------------------------
  // Event suppression and non-403 statuses
  // -----------------------------------------------------------------------

  it('does not fire event when suppressAccessDeniedToast is set', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: { code: 'forbidden', message: 'Insufficient permissions' },
      })
    );

    await apiFetch('/api/v1/agents', { suppressAccessDeniedToast: true });
    expect(captured).toHaveLength(0);
  });

  it('does not fire event for non-403 responses', async () => {
    fetchMock.mockResolvedValue(fakeResponse(200, { ok: true }));
    await apiFetch('/api/v1/agents');

    fetchMock.mockResolvedValue(fakeResponse(404, { error: { code: 'not_found' } }));
    await apiFetch('/api/v1/agents/missing');

    fetchMock.mockResolvedValue(fakeResponse(500, { error: { code: 'internal_error' } }));
    await apiFetch('/api/v1/agents/crash');

    expect(captured).toHaveLength(0);
  });

  it('fires event exactly once per 403 response', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
          details: { resource_type: 'agent', denied_action: 'delete' },
        },
      })
    );

    await apiFetch('/api/v1/agents/a');
    await apiFetch('/api/v1/agents/b');

    expect(captured).toHaveLength(2);
  });
});

// ---------------------------------------------------------------------------
// user_suspended — one-time redirect and toast suppression
// ---------------------------------------------------------------------------

describe('apiFetch — user_suspended handling', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let captured: AccessDeniedDetail[];
  let reloadSpy: ReturnType<typeof vi.fn>;

  function listener(e: Event) {
    captured.push((e as CustomEvent<AccessDeniedDetail>).detail);
  }

  beforeEach(() => {
    captured = [];
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    window.addEventListener('scion:access-denied', listener);

    // Reset the internal suspension flag between tests.
    _resetSuspendedState();

    // Mock window.location.reload to prevent actual reloads in JSDOM.
    reloadSpy = vi.fn();
    Object.defineProperty(window, 'location', {
      value: { ...window.location, reload: reloadSpy },
      writable: true,
      configurable: true,
    });
  });

  afterEach(() => {
    window.removeEventListener('scion:access-denied', listener);
    vi.restoreAllMocks();
  });

  it('triggers page reload on first user_suspended 403', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'user_suspended',
          message: 'Your account has been suspended.',
        },
      })
    );

    await apiFetch('/api/v1/agents');

    // Should trigger a full page reload.
    expect(reloadSpy).toHaveBeenCalledTimes(1);

    // Should NOT fire access-denied event (no toast).
    expect(captured).toHaveLength(0);
  });

  it('suppresses subsequent 403 toasts after user_suspended', async () => {
    // First call: user_suspended → triggers reload.
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'user_suspended',
          message: 'Your account has been suspended.',
        },
      })
    );
    await apiFetch('/api/v1/agents');
    expect(reloadSpy).toHaveBeenCalledTimes(1);

    // Second call: another 403 (generic) — should be suppressed entirely.
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
        },
      })
    );
    await apiFetch('/api/v1/projects');

    // No additional reload calls.
    expect(reloadSpy).toHaveBeenCalledTimes(1);

    // No access-denied event (toast suppressed).
    expect(captured).toHaveLength(0);
  });

  it('does not interfere with non-suspended 403 responses', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'forbidden',
          message: 'Insufficient permissions',
          details: { resource_type: 'project', denied_action: 'delete' },
        },
      })
    );

    await apiFetch('/api/v1/projects/x');

    // Should NOT trigger reload.
    expect(reloadSpy).not.toHaveBeenCalled();

    // Should fire the normal access-denied event.
    expect(captured).toHaveLength(1);
    expect(captured[0].action).toBe('delete');
    expect(captured[0].resource).toBe('project');
  });

  it('suppresses all toasts for concurrent user_suspended responses', async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(403, {
        error: {
          code: 'user_suspended',
          message: 'Your account has been suspended.',
        },
      })
    );

    // Simulate concurrent API calls all returning user_suspended.
    // In a single-threaded JS event loop, all three may detect suspension
    // before the flag suppresses them, but the important invariant is:
    // zero access-denied toasts, and at least one reload.
    await Promise.all([
      apiFetch('/api/v1/agents'),
      apiFetch('/api/v1/projects'),
      apiFetch('/api/v1/skills'),
    ]);

    // At least one reload was triggered.
    expect(reloadSpy).toHaveBeenCalled();

    // No access-denied events (no toast avalanche).
    expect(captured).toHaveLength(0);
  });

  it('suppresses sequential 403 toasts after initial suspension detection', async () => {
    // First call: user_suspended detected → flag set.
    fetchMock.mockResolvedValueOnce(
      fakeResponse(403, {
        error: {
          code: 'user_suspended',
          message: 'Your account has been suspended.',
        },
      })
    );
    await apiFetch('/api/v1/agents');
    expect(reloadSpy).toHaveBeenCalledTimes(1);

    // Sequential calls after the flag is set are fully suppressed.
    fetchMock.mockResolvedValueOnce(
      fakeResponse(403, {
        error: {
          code: 'user_suspended',
          message: 'Your account has been suspended.',
        },
      })
    );
    await apiFetch('/api/v1/projects');

    // No additional reload.
    expect(reloadSpy).toHaveBeenCalledTimes(1);
    // No toasts.
    expect(captured).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// 403 body read aborted mid-read (ptone/scion#2583)
// ---------------------------------------------------------------------------

describe('apiFetch — 403 body read aborted by the request signal', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let captured: AccessDeniedDetail[];
  let reloadSpy: ReturnType<typeof vi.fn>;

  function listener(e: Event) {
    captured.push((e as CustomEvent<AccessDeniedDetail>).detail);
  }

  /**
   * A 403 whose body sends `prefix` and then stalls until `signal` aborts,
   * at which point the stream errors (as a fetch body does on abort).
   */
  function slowResponse(prefix: string, signal: AbortSignal): Response {
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(prefix));
        signal.addEventListener('abort', () => {
          controller.error(new DOMException('The operation was aborted.', 'AbortError'));
        });
      },
    });
    return new Response(body, {
      status: 403,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  beforeEach(() => {
    captured = [];
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    window.addEventListener('scion:access-denied', listener);
    _resetSuspendedState();
    reloadSpy = vi.fn();
    Object.defineProperty(window, 'location', {
      value: { ...window.location, reload: reloadSpy },
      writable: true,
      configurable: true,
    });
  });

  afterEach(() => {
    window.removeEventListener('scion:access-denied', listener);
    vi.restoreAllMocks();
  });

  it('dispatches a generic reason, not empty detail, when aborted mid-read', async () => {
    const controller = new AbortController();
    fetchMock.mockResolvedValue(slowResponse('{"error":{"code":"forb', controller.signal));

    const pending = apiFetch('/api/v1/slow', { signal: controller.signal });
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalled());
    controller.abort();
    const res = await pending;

    expect(res.status).toBe(403);
    expect(captured).toEqual([{ reason: ACCESS_DENIED_UNREADABLE_REASON }]);
    expect(reloadSpy).not.toHaveBeenCalled();
  });

  it('does not treat an aborted user_suspended body as suspended', async () => {
    const controller = new AbortController();
    fetchMock.mockResolvedValue(
      slowResponse('{"error":{"code":"user_suspended","mess', controller.signal)
    );

    const pending = apiFetch('/api/v1/slow', { signal: controller.signal });
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalled());
    controller.abort();
    await pending;

    expect(reloadSpy).not.toHaveBeenCalled();
    expect(captured).toEqual([{ reason: ACCESS_DENIED_UNREADABLE_REASON }]);

    // Later handling is not suppressed: a readable user_suspended 403 still reloads.
    fetchMock.mockResolvedValue(
      fakeResponse(403, { error: { code: 'user_suspended', message: 'suspended' } })
    );
    await apiFetch('/api/v1/next');
    expect(reloadSpy).toHaveBeenCalledTimes(1);
  });

  it('honours suppressAccessDeniedToast when aborted mid-read', async () => {
    const controller = new AbortController();
    fetchMock.mockResolvedValue(slowResponse('{"err', controller.signal));

    const pending = apiFetch('/api/v1/slow', {
      signal: controller.signal,
      suppressAccessDeniedToast: true,
    });
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalled());
    controller.abort();
    await pending;

    expect(captured).toHaveLength(0);
    expect(reloadSpy).not.toHaveBeenCalled();
  });

  it('keeps empty detail for an unparseable body when not aborted', async () => {
    const controller = new AbortController();
    fetchMock.mockResolvedValue(
      new Response('Forbidden', { status: 403, headers: { 'Content-Type': 'text/plain' } })
    );

    await apiFetch('/api/v1/plain', { signal: controller.signal });
    expect(captured).toEqual([{}]);
  });
});

describe('apiErrorMessageFromBody / extractApiError', () => {
  it('reads {error: {message}}, appending error.details.guidance', () => {
    expect(apiErrorMessageFromBody({ error: { code: 'x', message: 'nope' } })).toBe('nope');
    expect(
      apiErrorMessageFromBody({
        error: { message: 'clone failed', details: { guidance: 'retry' } },
      })
    ).toBe('clone failed — retry');
  });

  it('reads {message} and {error: "..."}', () => {
    expect(apiErrorMessageFromBody({ message: 'quota' })).toBe('quota');
    expect(apiErrorMessageFromBody({ error: 'bad cursor' })).toBe('bad cursor');
  });

  it('returns undefined for bodies without a message', () => {
    for (const body of [null, undefined, 'text', 42, [], {}, { error: { code: 'x' } }]) {
      expect(apiErrorMessageFromBody(body)).toBeUndefined();
    }
  });

  it('treats empty strings as no message and ignores non-string guidance', () => {
    expect(apiErrorMessageFromBody({ message: '' })).toBeUndefined();
    expect(apiErrorMessageFromBody({ message: '', error: 'x' })).toBe('x');
    expect(apiErrorMessageFromBody({ error: '' })).toBeUndefined();
    expect(
      apiErrorMessageFromBody({ error: { message: 'm', details: { guidance: { a: 1 } } } })
    ).toBe('m');
    expect(apiErrorMessageFromBody({ error: { message: 'm', details: { guidance: '' } } })).toBe(
      'm'
    );
  });

  it('extractApiError uses it and falls back for non-JSON or message-less bodies', async () => {
    const json = (b: unknown) => new Response(JSON.stringify(b), { status: 400 });
    expect(
      await extractApiError(json({ error: { message: 'm', details: { guidance: 'g' } } }), 'fb')
    ).toBe('m — g');
    expect(await extractApiError(json({ error: { code: 'x' } }), 'fb')).toBe('fb');
    expect(await extractApiError(json({ message: '' }), 'fb')).toBe('fb');
    expect(await extractApiError(new Response('<html>', { status: 502 }), 'fb')).toBe('fb');
  });
});
