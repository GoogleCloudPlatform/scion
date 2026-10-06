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
  formatHeartbeatAge,
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
      section_metadata: {
        lifecycle: { source: 'db', revision: 4 },
        access: { source: 'db', revision: 9 },
      },
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

  it('sends create-only revision 0 when the lifecycle row is not in the DB yet', () => {
    for (const lifecycle of [
      { source: 'default', revision: 0 },
      { source: 'file' },
      { source: 'db', revision: 0 },
      // Only a DB row's revision is a CAS base.
      { source: 'file', revision: 3 },
    ]) {
      expect(
        buildStallConfigUpdate({ section_metadata: { lifecycle } }, true).expected_revisions
      ).toEqual({ lifecycle: 0 });
    }
  });

  it('sends no revision on a file-backed hub (no section metadata)', () => {
    const body = buildStallConfigUpdate({ server: { hub: { stalled_threshold: '5m' } } }, true);
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

describe('formatHeartbeatAge', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('shows never for a null, undefined or empty heartbeat', () => {
    expect(formatHeartbeatAge(null)).toBe('never');
    expect(formatHeartbeatAge(undefined)).toBe('never');
    expect(formatHeartbeatAge('')).toBe('never');
  });

  it('shows never for the Go zero time', () => {
    expect(formatHeartbeatAge('0001-01-01T00:00:00Z')).toBe('never');
    expect(formatHeartbeatAge('1970-01-01T00:00:00Z')).toBe('never');
    expect(formatHeartbeatAge('1969-12-31T23:59:59Z')).toBe('never');
  });

  it('still formats a recent heartbeat as a relative age', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-06T12:00:00Z'));
    expect(formatHeartbeatAge('2026-10-06T11:59:30Z')).toBe('30s ago');
    expect(formatHeartbeatAge('2026-10-06T11:55:00Z')).toBe('5m ago');
    expect(formatHeartbeatAge('2026-10-06T09:00:00Z')).toBe('3h ago');
    expect(formatHeartbeatAge('2026-10-05T12:00:00Z')).toBe('yesterday');
    expect(formatHeartbeatAge('2026-10-04T12:00:00Z')).toBe('2d ago');
  });

  it('shows just now for a future instant and unknown for garbage', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-06T12:00:00Z'));
    expect(formatHeartbeatAge('2026-10-06T12:01:00Z')).toBe('just now');
    expect(formatHeartbeatAge('not-a-date')).toBe('unknown');
  });
});

describe('scion-page-health-dashboard runtime brokers (ptone/scion#3582)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.mocked(apiFetch).mockReset();
  });

  it('renders runtime_brokers as one table row per broker', async () => {
    vi.mocked(apiFetch).mockImplementation(async () =>
      json({
        status: 'healthy',
        hub: { status: 'healthy', version: 'v1', uptime: '1h', connected_brokers: 2 },
        database: { status: 'healthy', pool_active: 1, pool_max: 10, pool_idle: 1 },
        runtime_brokers: {
          items: [
            {
              id: 'b1',
              name: 'zulu',
              version: '1.0.0',
              status: 'online',
              last_heartbeat: null,
              runtime: null,
              workspace_storage: { backend: 'nfs', nfs_healthy: true },
              agents: { running: 0, attention: 0, total: 0 },
            },
            {
              id: 'b2',
              name: 'alpha',
              version: '1.0.0',
              status: 'offline',
              last_heartbeat: null,
              runtime: { type: 'docker', profile: 'docker' },
              workspace_storage: { backend: 'local' },
              agents: { running: 0, attention: 0, total: 0 },
            },
          ],
          total: 5,
          truncated: true,
        },
        agents: { total: 0, by_phase: {}, stalled: [], crashed: [], errored: [] },
        dispatch: null,
        stall_config: { threshold_seconds: 300, auto_suspend: false },
      })
    );
    const page = document.createElement('scion-page-health-dashboard') as ScionPageHealthDashboard;
    document.body.appendChild(page);
    await (page as unknown as { fetchData(): Promise<void> }).fetchData();
    await page.updateComplete;

    const table = page.shadowRoot?.querySelector('scion-health-broker-table');
    expect(table).not.toBeNull();
    await (table as LitLike).updateComplete;
    const rows = [...(table!.shadowRoot?.querySelectorAll('tbody tr') ?? [])];
    expect(rows.map((r) => (r as HTMLElement).dataset.brokerId)).toEqual(['b2', 'b1']);
    expect(table!.shadowRoot?.querySelector('.note')?.textContent?.trim()).toBe('Showing 2 of 5');
    expect(page.shadowRoot?.querySelector('.broker-card')).toBeNull();
  });
});

type LitLike = Element & { updateComplete: Promise<unknown> };
