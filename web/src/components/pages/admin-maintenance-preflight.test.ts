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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './admin-maintenance.js';
import type { ScionPageAdminMaintenance } from './admin-maintenance.js';
import { canForceRerun } from './admin-maintenance-banner.js';

const testSHA = '0123456789abcdef0123456789abcdef01234567';

function makeFailedPreflightRun(id = 'run-fail-1', overrides: Record<string, unknown> = {}) {
  return {
    id,
    operationKey: 'rebuild-server',
    status: 'failed',
    startedAt: '2026-11-21T12:00:00Z',
    completedAt: '2026-11-21T12:01:00Z',
    startedBy: 'admin@example.com',
    log: '==> Preflight checks\n[BLOCK] settings.profiles.missing: missing dev',
    result: JSON.stringify({
      branch: 'main',
      branchPinnable: true,
      commit: testSHA,
      error: 'preflight found 1 blocking finding(s)',
      preflight: {
        version: 1,
        checks: ['settings.profiles'],
        blocking: 1,
        warnings: 1,
        findings: [
          {
            check: 'settings.profiles',
            code: 'settings.profiles.missing',
            severity: 'block',
            message: 'profile <script>alert(1)</script> missing',
            remediation: 'restore profile dev',
            subjects: ['agent-1', 'agent-2'],
            subjectCount: 5,
            data: { profile: 'dev', project: 'p1', agents: 5 },
          },
          {
            check: 'settings.profiles',
            code: 'settings.profiles.warn',
            severity: 'warn',
            message: 'minor warning',
          },
          {
            check: 'settings.profiles',
            code: 'settings.profiles.info',
            severity: 'info',
            message: 'info note',
          },
        ],
      },
      ...overrides,
    }),
  };
}

describe('admin-maintenance preflight UI (Slice 2b + H2)', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;
  let el: ScionPageAdminMaintenance;

  beforeEach(() => {
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    el?.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  function mockDefaultEndpoints(
    runs: Array<ReturnType<typeof makeFailedPreflightRun>>,
    postHandler?: (init?: RequestInit) => Response
  ) {
    const latest = runs[0];
    fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
      if (
        url.includes('/api/v1/admin/maintenance/operations/rebuild-server/run') &&
        init?.method === 'POST'
      ) {
        if (postHandler) return Promise.resolve(postHandler(init));
        return Promise.resolve(
          new Response(JSON.stringify({ runId: 'run-forced-new', status: 'running' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (url.includes('/api/v1/admin/maintenance/operations/rebuild-server/runs?')) {
        return Promise.resolve(
          new Response(JSON.stringify({ runs }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (url.includes('/api/v1/admin/maintenance/operations')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              operations: [
                {
                  id: 'op-rebuild',
                  key: 'rebuild-server',
                  title: 'Rebuild Server',
                  description: 'Rebuild hub from git',
                  category: 'operation',
                  status: 'idle',
                  createdAt: '2026-11-21T00:00:00Z',
                  lastRun: latest,
                },
              ],
              migrations: [],
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      if (url.includes('/api/v1/admin/maintenance/update-status')) {
        return Promise.resolve(new Response(JSON.stringify({}), { status: 404 }));
      }
      if (url.includes('/api/v1/admin/maintenance')) {
        return Promise.resolve(
          new Response(JSON.stringify({ enabled: false, message: '' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200 }));
    });
  }

  async function mountPage() {
    el = document.createElement('scion-page-admin-maintenance') as ScionPageAdminMaintenance;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;
    return el;
  }

  it('evaluates the force button eligibility matrix (S38, S102, CN6)', () => {
    const run = makeFailedPreflightRun('r1');
    expect(canForceRerun(run, 'r1')).toEqual({ allowed: true });
    // Older run -> not allowed.
    expect(canForceRerun(run, 'r2')).toEqual({ allowed: false });
    // S38: forcedBy present -> not allowed.
    expect(
      canForceRerun(makeFailedPreflightRun('r1', { forcedBy: { runId: 'r2' } }), 'r1')
    ).toEqual({
      allowed: false,
    });
    // forced: true -> not allowed.
    expect(canForceRerun(makeFailedPreflightRun('r1', { forced: true }), 'r1')).toEqual({
      allowed: false,
    });
    // CN6: forceRejected without forced: true IS allowed.
    expect(
      canForceRerun(
        makeFailedPreflightRun('r1', { rerunOf: 'r0', forceRejected: 'findings changed' }),
        'r1'
      )
    ).toEqual({ allowed: true });
    // S102: branchPinnable: false -> hidden with reason.
    const unpinnable = canForceRerun(makeFailedPreflightRun('r1', { branchPinnable: false }), 'r1');
    expect(unpinnable.allowed).toBe(false);
    expect(unpinnable.unpinnableReason).toContain('cannot be pinned');
  });

  it('renders findings table, escapes XSS text, collapses info rows, and refreshes history on detail open (H2)', async () => {
    const failedRun = makeFailedPreflightRun('run-fail-1');
    mockDefaultEndpoints([failedRun]);
    await mountPage();

    // Open detail directly (without expanding history drawer first) — H2 must refresh history.
    await (el as unknown as { viewRunDetail: (r: unknown) => Promise<void> }).viewRunDetail(
      failedRun
    );
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const historyCalls = fetchMock.mock.calls.filter(([u]) => {
      const url = typeof u === 'string' ? u : u instanceof URL ? u.toString() : u.url;
      return url.includes('/operations/rebuild-server/runs?limit=20');
    });
    expect(historyCalls.length).toBeGreaterThanOrEqual(1);

    const shadow = el.shadowRoot!;
    const table = shadow.querySelector('.preflight-findings-table');
    expect(table).not.toBeNull();
    // XSS: <script> must be rendered as text, never an executable <script> element.
    expect(shadow.querySelector('script')).toBeNull();
    expect(table?.textContent).toContain('profile <script>alert(1)</script> missing');
    expect(table?.textContent).toContain('agent-1, agent-2 and 3 more');
    expect(table?.textContent).toContain('restore profile dev');
    // Info row collapsed under "1 info".
    const infoBtn = shadow.querySelector('.preflight-info-toggle') as HTMLButtonElement;
    expect(infoBtn?.textContent?.trim()).toBe('1 info');
  });

  it('gates confirm button on checkbox and posts force="true" + rerunOf (S39, S40)', async () => {
    const failedRun = makeFailedPreflightRun('run-fail-1');
    let capturedBody: Record<string, unknown> | null = null;
    mockDefaultEndpoints([failedRun], (init) => {
      const rawBody = typeof init?.body === 'string' ? init.body : '{}';
      capturedBody = JSON.parse(rawBody) as Record<string, unknown>;
      return new Response(JSON.stringify({ runId: 'run-forced-2', status: 'running' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    });
    await mountPage();

    await (el as unknown as { viewRunDetail: (r: unknown) => Promise<void> }).viewRunDetail(
      failedRun
    );
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const shadow = el.shadowRoot!;
    const forceBtn = Array.from(shadow.querySelectorAll('sl-button')).find((b) =>
      b.textContent?.includes('Re-run with force')
    ) as HTMLElement | undefined;
    expect(forceBtn).toBeDefined();
    forceBtn!.click();
    await el.updateComplete;

    const confirmDialog = Array.from(shadow.querySelectorAll('sl-dialog')).find(
      (d) => d.getAttribute('label') === 'Confirm Force Re-run'
    );
    expect(confirmDialog).toBeDefined();
    expect(confirmDialog?.textContent).toContain(
      `This builds commit ${testSHA} on main, not the branch tip`
    );

    const confirmBtn = Array.from(confirmDialog!.querySelectorAll('sl-button')).find((b) =>
      b.textContent?.includes('Confirm force re-run')
    ) as HTMLButtonElement;
    // S39: disabled before checkbox is checked.
    expect(confirmBtn.hasAttribute('disabled')).toBe(true);

    // Tick the checkbox.
    const checkbox = confirmDialog!.querySelector('sl-checkbox') as HTMLInputElement;
    checkbox.checked = true;
    checkbox.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(confirmBtn.hasAttribute('disabled')).toBe(false);

    // Click confirm and assert POST body (S40).
    confirmBtn.click();
    await new Promise((r) => setTimeout(r, 20));
    expect(capturedBody).toEqual({
      params: { force: 'true', rerunOf: 'run-fail-1' },
    });
  });

  it('renders forced-run banner with "(was M)" and failed-run link', async () => {
    const failedRun = makeFailedPreflightRun('run-fail-1');
    const forcedRun = {
      id: 'run-forced-2',
      operationKey: 'rebuild-server',
      status: 'completed',
      startedAt: '2026-11-21T12:05:00Z',
      completedAt: '2026-11-21T12:06:00Z',
      startedBy: 'admin@example.com',
      log: 'PREFLIGHT OVERRIDDEN',
      result: JSON.stringify({
        branch: 'main',
        commit: testSHA,
        rerunOf: 'run-fail-1',
        forced: true,
        overrideApplied: true,
        override: {
          actorId: 'u-1',
          actorEmail: 'admin@example.com',
          at: '2026-11-21T12:05:30Z',
          rerunOf: 'run-fail-1',
          commit: testSHA,
          branch: 'main',
          findings: [
            {
              code: 'settings.profiles.missing',
              message: 'profile missing',
              agents: 7,
              failedRunAgents: 5,
            },
          ],
          failedRunFindings: 1,
        },
      }),
    };
    mockDefaultEndpoints([forcedRun, failedRun]);
    await mountPage();

    await (el as unknown as { viewRunDetail: (r: unknown) => Promise<void> }).viewRunDetail(
      forcedRun
    );
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const banner = el.shadowRoot!.querySelector('.preflight-banner');
    expect(banner?.getAttribute('data-banner-row')).toBe('F3');
    expect(banner?.textContent?.replace(/\s+/g, ' ')).toContain('7 agents (was 5)');
    const link = banner?.querySelector('.preflight-run-link');
    expect(link?.textContent).toContain('View run run-fail-1');
  });
});
