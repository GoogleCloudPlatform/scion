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
 * Tests for scion-page-home — focused on invite-stats 403 suppression (#1733).
 *
 * The dashboard calls `apiFetch('/api/v1/admin/invites/stats')` for admin
 * users. When the user lacks the specific permission, the endpoint returns 403.
 * The call must use `suppressAccessDeniedToast: true` so no toast fires (and
 * thus no double-removal NotFoundError occurs).
 */

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import type { AccessDeniedDetail } from '../../client/api.js';

// We test the suppression behavior by intercepting fetch and the
// scion:access-denied event — no need to render the full Lit component.

describe('dashboard invite-stats 403 suppression (#1733)', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let accessDeniedEvents: AccessDeniedDetail[];
  const accessDeniedListener = (e: Event) => {
    accessDeniedEvents.push((e as CustomEvent<AccessDeniedDetail>).detail);
  };

  beforeEach(() => {
    accessDeniedEvents = [];
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    window.addEventListener('scion:access-denied', accessDeniedListener);
  });

  afterEach(() => {
    window.removeEventListener('scion:access-denied', accessDeniedListener);
    vi.restoreAllMocks();
  });

  it('invite-stats 403 does not produce scion:access-denied event', async () => {
    // Import apiFetch fresh so it uses our mocked fetch
    const { apiFetch } = await import('../../client/api.js');

    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          error: {
            code: 'forbidden',
            message: 'Insufficient permissions',
            details: {
              resource_type: 'invite_stats',
              denied_action: 'read',
            },
          },
        }),
        { status: 403, headers: { 'Content-Type': 'application/json' } }
      )
    );

    // This mirrors the exact call in home.ts loadData()
    await apiFetch('/api/v1/admin/invites/stats', {
      suppressAccessDeniedToast: true,
    }).catch(() => null);

    // No access-denied event should fire
    expect(accessDeniedEvents).toHaveLength(0);
  });

  it('unsuppressed 403 from a different endpoint still fires access-denied event', async () => {
    const { apiFetch } = await import('../../client/api.js');

    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          error: {
            code: 'forbidden',
            message: 'Insufficient permissions',
            details: {
              resource_type: 'agent',
              denied_action: 'delete',
            },
          },
        }),
        { status: 403, headers: { 'Content-Type': 'application/json' } }
      )
    );

    // A normal apiFetch call WITHOUT suppressAccessDeniedToast
    await apiFetch('/api/v1/agents/test-agent');

    // Access-denied event should fire normally
    expect(accessDeniedEvents).toHaveLength(1);
    expect(accessDeniedEvents[0].action).toBe('delete');
    expect(accessDeniedEvents[0].resource).toBe('agent');
  });
});
