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
 * Health dashboard stall settings save (ptone/scion#3059).
 *
 * The hub decodes server.hub.auto_suspend_stalled from a nested object; a
 * flat dotted key is dropped while the PUT still returns 200. A DB-backed hub
 * also replaces the whole lifecycle row, so the other lifecycle keys must be
 * carried over.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import { apiFetch } from '../../client/api.js';
import { showToast } from '../../utils/toast.js';
import {
  buildStallConfigUpdate,
  ScionPageHealthDashboard,
  type ServerConfigSnapshot,
} from './health-dashboard.js';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('buildStallConfigUpdate', () => {
  it('sends the nested shape, never a flat dotted key', () => {
    const body = buildStallConfigUpdate({}, true);
    expect(body).toEqual({ server: { hub: { auto_suspend_stalled: true } } });
    expect(body).not.toHaveProperty(['server.hub.auto_suspend_stalled']);
  });

  it('sends false explicitly', () => {
    expect(buildStallConfigUpdate({}, false)).toEqual({
      server: { hub: { auto_suspend_stalled: false } },
    });
  });

  it('carries over the other lifecycle keys and the lifecycle revision', () => {
    const current: ServerConfigSnapshot = {
      server: {
        hub: {
          stalled_threshold: '10m',
          soft_delete_retention: '72h',
          soft_delete_retain_files: false,
        },
      },
      section_metadata: { lifecycle: { revision: 4 }, access: { revision: 9 } },
    };
    expect(buildStallConfigUpdate(current, true)).toEqual({
      server: {
        hub: {
          auto_suspend_stalled: true,
          stalled_threshold: '10m',
          soft_delete_retention: '72h',
          soft_delete_retain_files: false,
        },
      },
      expected_revisions: { lifecycle: 4 },
    });
  });

  it('omits the revision when the lifecycle section is not in the DB', () => {
    const body = buildStallConfigUpdate({ section_metadata: { lifecycle: { revision: 0 } } }, true);
    expect(body).not.toHaveProperty('expected_revisions');
  });
});

describe('scion-page-health-dashboard stall settings save', () => {
  let el: ScionPageHealthDashboard;
  /** The hub's stored value, as the fake hub reads it back. */
  let stored: boolean;
  let puts: unknown[];

  beforeEach(() => {
    stored = false;
    puts = [];
    vi.mocked(apiFetch).mockImplementation(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      if (url === '/api/v1/admin/health/summary') {
        return json({ stall_config: { threshold_seconds: 300, auto_suspend: stored } });
      }
      if (url === '/api/v1/admin/server-config' && method === 'GET') {
        return json({
          server: { hub: { auto_suspend_stalled: stored, stalled_threshold: '10m' } },
          section_metadata: { lifecycle: { source: 'db', revision: 2 } },
        });
      }
      if (url === '/api/v1/admin/server-config' && method === 'PUT') {
        const body = JSON.parse(String(init?.body)) as {
          server?: { hub?: { auto_suspend_stalled?: boolean } };
        };
        puts.push(body);
        // Like the hub: only the nested key is decoded.
        const v = body.server?.hub?.auto_suspend_stalled;
        if (typeof v === 'boolean') stored = v;
        return json({ applied: ['auto_suspend_stalled'] });
      }
      throw new Error(`unexpected request ${method} ${url}`);
    });
    el = new ScionPageHealthDashboard();
  });

  afterEach(() => {
    el.remove();
    vi.mocked(apiFetch).mockReset();
    vi.mocked(showToast).mockReset();
  });

  interface Internals {
    stallAutoSuspend: boolean;
    editingStall: boolean;
    saveStallConfig(): Promise<void>;
    fetchData(): Promise<void>;
  }

  it('persists the toggle and reads it back', async () => {
    const i = el as unknown as Internals;
    i.editingStall = true;
    i.stallAutoSuspend = true;
    await i.saveStallConfig();

    expect(puts).toEqual([
      {
        server: { hub: { auto_suspend_stalled: true, stalled_threshold: '10m' } },
        expected_revisions: { lifecycle: 2 },
      },
    ]);
    expect(showToast).toHaveBeenCalledWith('Stall detection settings saved', 'success');
    expect(stored).toBe(true);

    // A fresh read reflects the saved value.
    i.stallAutoSuspend = false;
    await i.fetchData();
    expect(i.stallAutoSuspend).toBe(true);
  });

  it('does not PUT when the current settings cannot be read', async () => {
    vi.mocked(apiFetch).mockImplementation(async () =>
      json({ error: { code: 'internal', message: 'boom' } }, 500)
    );
    const i = el as unknown as Internals;
    i.stallAutoSuspend = true;
    await i.saveStallConfig();
    expect(
      vi.mocked(apiFetch).mock.calls.some(([, init]) => (init as RequestInit)?.method === 'PUT')
    ).toBe(false);
    expect(vi.mocked(showToast).mock.calls[0]?.[1]).toBe('danger');
  });
});
