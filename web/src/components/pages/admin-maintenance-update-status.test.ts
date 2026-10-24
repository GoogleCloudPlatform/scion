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

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

// The maintenance page reads the server version, mode and installer from
// GET /api/v1/admin/maintenance/update-status, applies updates with an
// explicit target_version, and waits for the hub to come back after a
// restarting update run.

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));
vi.mock('../../client/hub-restart.js', () => ({ waitForHubRestart: vi.fn() }));

import { showToast } from '../../utils/toast.js';
import { waitForHubRestart } from '../../client/hub-restart.js';

type Json = Record<string, unknown>;

function updateStatus(overrides: Json = {}): Json {
  return {
    current: {
      version: 'nightly-20261022',
      channel: 'nightly',
      commit: 'abc123def4567890',
      buildTime: '2026-10-22T00:00:00Z',
      isGitBuild: false,
    },
    mode: { effective: 'release', repoConfigured: false, repoBranch: '' },
    channel: { effective: 'nightly', configured: 'nightly', source: 'config' },
    pin: 'latest',
    installer: { strategy: 'symlink', supported: true, reason: '', migrateHint: '' },
    updatePolicy: 'notify',
    pendingUpdate: null,
    lastUpdate: null,
    ...overrides,
  };
}

function op(key: string, title: string, lastRun?: Json): Json {
  return {
    key,
    title,
    description: `${title} description`,
    category: 'operation',
    status: 'pending',
    createdAt: '2026-01-01T00:00:00Z',
    ...(lastRun ? { lastRun } : {}),
  };
}

const OPS = [
  op('rebuild-server', 'Rebuild from Git'),
  op('update-binary', 'Update Binary'),
  op('op-a', 'Operation A'),
];

interface Options {
  /** Body of GET update-status, or an HTTP status to fail with. */
  status?: Json | number | (() => Json);
  /** Body of GET update-available. */
  updateAvailable?: Json;
  /** Response to POST update-binary/run. */
  run?: { status: number; body: Json };
  /** Operations list; may change between calls. */
  operations?: () => Json[];
}

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function pathOf(input: string | URL | Request): string {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  return new URL(url, 'http://localhost').pathname;
}

const runBodies: unknown[] = [];

function handler(opts: Options) {
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = pathOf(input);
    const method = (init?.method ?? 'GET').toUpperCase();
    if (path === '/api/v1/admin/maintenance/update-status') {
      const s = opts.status ?? updateStatus();
      if (typeof s === 'number') {
        return json({ error: { code: 'internal_error', message: 'status unavailable' } }, s);
      }
      return json(typeof s === 'function' ? s() : s);
    }
    if (path === '/api/v1/admin/maintenance/update-available') {
      return json(opts.updateAvailable ?? { update_available: false });
    }
    if (method === 'POST' && path.endsWith('/run')) {
      runBodies.push(JSON.parse(init?.body as string));
      const r = opts.run ?? { status: 200, body: { runId: 'run-1', status: 'running' } };
      return json(r.body, r.status);
    }
    if (path === '/api/v1/admin/maintenance/operations') {
      return json({ migrations: [], operations: opts.operations ? opts.operations() : OPS });
    }
    if (path === '/api/v1/admin/maintenance') {
      return json({ enabled: false });
    }
    return json({});
  };
}

type Page = HTMLElement & {
  updateComplete: Promise<unknown>;
  updateConfirmTarget: string | null;
  stopPolling(): void;
  loadData(isPolling?: boolean): Promise<void>;
};

beforeAll(async () => {
  await import('./admin-maintenance.js');
}, 60_000);

let el: Page | null = null;
let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.mocked(showToast).mockClear();
  vi.mocked(waitForHubRestart).mockReset();
  runBodies.length = 0;
});

afterEach(() => {
  el?.stopPolling();
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
});

async function settle(e: Page): Promise<void> {
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await e.updateComplete;
}

async function mount(opts: Options = {}): Promise<Page> {
  fetchMock = vi.fn(handler(opts));
  vi.stubGlobal('fetch', fetchMock);
  const e = document.createElement('scion-page-admin-maintenance') as unknown as Page;
  document.body.appendChild(e);
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 200));
  await e.updateComplete;
  return e;
}

function root(e: Page): ShadowRoot {
  return e.shadowRoot!;
}

function versionSection(e: Page): HTMLElement {
  const s = root(e).querySelector<HTMLElement>('.section.server-version');
  expect(s).not.toBeNull();
  return s!;
}

function cardTitles(e: Page): string[] {
  const sections = Array.from(root(e).querySelectorAll<HTMLElement>('.section'));
  const ops = sections.find(
    (s) => s.querySelector('.section-title')?.textContent?.trim() === 'Routine Operations'
  );
  return Array.from(ops?.querySelectorAll('.card-title') ?? []).map(
    (t) => t.textContent?.trim() ?? ''
  );
}

function opCard(e: Page, title: string): HTMLElement | undefined {
  return Array.from(root(e).querySelectorAll<HTMLElement>('.card')).find(
    (c) => c.querySelector('.card-title')?.textContent?.trim() === title
  );
}

function findButton(scope: ParentNode, text: string): HTMLElement | undefined {
  return Array.from(scope.querySelectorAll<HTMLElement>('sl-button')).find(
    (b) => b.textContent?.trim() === text
  );
}

function statusFetchCount(): number {
  return fetchMock.mock.calls.filter(
    (c) => pathOf(c[0] as string) === '/api/v1/admin/maintenance/update-status'
  ).length;
}

describe('Server Version & Updates', () => {
  it('renders version, channel, commit and installer for a supported installer', async () => {
    el = await mount();
    const s = versionSection(el);
    expect(s.querySelector('.current-version')?.textContent).toBe('nightly-20261022');
    expect(s.querySelector('sl-badge.channel-badge')?.textContent?.trim()).toBe('nightly');
    expect(s.querySelector('.current-commit')?.textContent).toBe('abc123def456');
    expect(s.querySelector('.installer-strategy')?.textContent).toBe('symlink');
    expect(s.querySelector('.installer-unsupported')).toBeNull();
    expect(s.querySelector('.last-update')).toBeNull();
  });

  it('shows the reason and migrate hint when the installer is unsupported', async () => {
    el = await mount({
      status: updateStatus({
        installer: {
          strategy: 'unsupported',
          supported: false,
          reason: '/usr/local/bin/scion is not a symlink into the install root',
          migrateHint: 'Re-run deploy.sh migrate-layout',
        },
      }),
    });
    const alert = versionSection(el).querySelector('sl-alert.installer-unsupported');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('is not a symlink into the install root');
    expect(alert!.textContent).toContain('Re-run deploy.sh migrate-layout');
  });

  it.each([
    ['verified', 'verified', 'completed'],
    ['failed', 'failed', 'failed'],
    ['rolled_back', 'rolled back', 'pending'],
  ])('renders lastUpdate status %s', async (status, label, badgeClass) => {
    el = await mount({
      status: updateStatus({
        lastUpdate: {
          runId: 'run-9',
          from: 'nightly-20261021',
          to: 'nightly-20261022',
          status,
          at: '2026-10-22T01:00:00Z',
        },
      }),
    });
    const last = versionSection(el).querySelector('.last-update');
    expect(last).not.toBeNull();
    expect(last!.textContent).toContain('nightly-20261021');
    expect(last!.textContent).toContain('nightly-20261022');
    const badge = last!.querySelector('.status-badge');
    expect(badge?.textContent?.trim()).toBe(label);
    expect(badge?.classList.contains(badgeClass)).toBe(true);
    expect(last!.querySelector('a.last-update-run')?.textContent).toBe('View run');
  });

  it('shows release-mode operations only when mode is release', async () => {
    el = await mount();
    expect(cardTitles(el)).toEqual(['Update Binary', 'Operation A']);
  });

  it('shows git-mode operations only when mode is git', async () => {
    el = await mount({
      status: updateStatus({
        mode: { effective: 'git', repoConfigured: true, repoBranch: 'main' },
      }),
    });
    expect(cardTitles(el)).toEqual(['Rebuild from Git', 'Operation A']);
  });

  it('shows an error and hides mode-specific operations when update-status fails', async () => {
    el = await mount({ status: 500 });
    const alert = versionSection(el).querySelector('sl-alert.update-status-error');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('status unavailable');
    // No guessing: neither Rebuild nor Update is offered.
    expect(cardTitles(el)).toEqual(['Operation A']);
    // The rest of the page still renders.
    expect(root(el).querySelector('.error-state')).toBeNull();
  });

  it('reloads update-status from the error alert', async () => {
    let fail = true;
    el = await mount({
      status: () => {
        if (fail) return { unexpected: true };
        return updateStatus();
      },
    });
    expect(versionSection(el).querySelector('sl-alert.update-status-error')).not.toBeNull();
    fail = false;
    findButton(versionSection(el), 'Reload status')!.click();
    await settle(el);
    expect(versionSection(el).querySelector('sl-alert.update-status-error')).toBeNull();
    expect(cardTitles(el)).toEqual(['Update Binary', 'Operation A']);
  });
});

describe('Apply update', () => {
  const AVAILABLE = {
    update_available: true,
    update: {
      version: 'nightly-20261023',
      channel: 'nightly',
      detected_at: '2026-10-23T00:00:00Z',
    },
  };

  it('confirms in a dialog, then posts exactly target_version and pin', async () => {
    el = await mount({ updateAvailable: AVAILABLE });
    findButton(root(el), 'Apply Update')!.click();
    await settle(el);

    const dialog = root(el).querySelector('sl-dialog[label="Update Hub"]');
    expect(dialog).not.toBeNull();
    expect(dialog!.textContent).toContain('nightly-20261023');
    expect(dialog!.textContent).toContain('restarts');
    expect(runBodies).toHaveLength(0);

    dialog!.querySelector<HTMLElement>('sl-button.confirm-update')!.click();
    await settle(el);

    expect(runBodies).toEqual([{ params: { target_version: 'nightly-20261023', pin: 'latest' } }]);
    const runCall = fetchMock.mock.calls.find((c) => pathOf(c[0] as string).endsWith('/run'));
    expect(pathOf(runCall![0] as string)).toBe(
      '/api/v1/admin/maintenance/operations/update-binary/run'
    );
    expect(el.updateConfirmTarget).toBeNull();
  });

  it('runs the update-binary card with latest:<channel> when no tag is known', async () => {
    el = await mount();
    const card = opCard(el, 'Update Binary');
    findButton(card!, 'Run')!.click();
    await settle(el);
    expect(el.updateConfirmTarget).toBe('latest:nightly');
    root(el)
      .querySelector<HTMLElement>('sl-dialog[label="Update Hub"] sl-button.confirm-update')!
      .click();
    await settle(el);
    expect(runBodies).toEqual([{ params: { target_version: 'latest:nightly', pin: 'latest' } }]);
  });

  it('disables the confirm button when the installer is unsupported', async () => {
    el = await mount({
      updateAvailable: AVAILABLE,
      status: updateStatus({
        installer: { strategy: 'unsupported', supported: false, reason: 'no layout' },
      }),
    });
    findButton(root(el), 'Apply Update')!.click();
    await settle(el);
    const confirm = root(el).querySelector<HTMLElement>(
      'sl-dialog[label="Update Hub"] sl-button.confirm-update'
    );
    expect(confirm!.hasAttribute('disabled')).toBe(true);
  });

  it.each([
    ['already_running', 409, 'already running nightly-20261023'],
    ['downgrade_not_yet_supported', 409, 'downgrades are not supported yet'],
    ['unknown_param', 400, 'unknown parameter'],
    ['preflight_failed', 409, 'could not start'],
  ])('shows %s readably and keeps the dialog open', async (code, status, text) => {
    el = await mount({
      updateAvailable: AVAILABLE,
      run: { status, body: { error: { code, message: 'hub says no' } } },
    });
    el.updateConfirmTarget = 'nightly-20261023';
    await settle(el);
    root(el)
      .querySelector<HTMLElement>('sl-dialog[label="Update Hub"] sl-button.confirm-update')!
      .click();
    await settle(el);

    expect(showToast).toHaveBeenCalledTimes(1);
    const msg = vi.mocked(showToast).mock.calls[0][0];
    expect(msg).toContain(text);
    expect(msg).toContain('hub says no');
    expect(el.updateConfirmTarget).toBe('nightly-20261023');
  });
});

describe('Reconnect after an update restart', () => {
  const restartingOps = (): Json[] => [
    op('update-binary', 'Update Binary', {
      id: 'run-1',
      status: 'restarting',
      startedAt: '2026-10-24T00:00:00Z',
    }),
  ];

  it('renders a restarting run as in progress', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    el = await mount({ operations: restartingOps });
    const card = opCard(el, 'Update Binary');
    expect(findButton(card!, 'Restarting...')).toBeDefined();
    expect(findButton(card!, 'Run')).toBeUndefined();
    expect(card!.querySelector('.status-badge.restarting')).not.toBeNull();
  });

  it('waits for the target version, then reloads update-status and reports the result', async () => {
    let ops: Json[] = [op('update-binary', 'Update Binary')];
    let status = updateStatus();
    vi.mocked(waitForHubRestart).mockImplementation(() => {
      // The hub restarts on the target and settles the run on boot.
      ops = [
        op('update-binary', 'Update Binary', { id: 'run-1', status: 'completed', startedAt: '' }),
      ];
      status = updateStatus({
        current: {
          version: 'nightly-20261023',
          channel: 'nightly',
          commit: 'f00',
          isGitBuild: false,
        },
        lastUpdate: {
          runId: 'run-1',
          from: 'nightly-20261022',
          to: 'nightly-20261023',
          status: 'verified',
          at: '2026-10-24T00:01:00Z',
        },
      });
      return Promise.resolve({
        ok: true,
        observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
      });
    });
    el = await mount({ operations: () => ops, status: () => status });
    el.updateConfirmTarget = 'nightly-20261023';
    await settle(el);
    root(el)
      .querySelector<HTMLElement>('sl-dialog[label="Update Hub"] sl-button.confirm-update')!
      .click();
    await settle(el);
    const statusLoadsBefore = statusFetchCount();

    // The run reaches restarting; the next poll switches to the health wait.
    ops = restartingOps();
    await el.loadData(true);
    await settle(el);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(waitForHubRestart).toHaveBeenCalledWith({ expectVersion: 'nightly-20261023' });
    expect(statusFetchCount()).toBeGreaterThan(statusLoadsBefore);
    expect(versionSection(el).querySelector('.current-version')?.textContent).toBe(
      'nightly-20261023'
    );
    expect(
      versionSection(el).querySelector('.last-update .status-badge')?.textContent?.trim()
    ).toBe('verified');
    expect(vi.mocked(showToast).mock.calls.some((c) => String(c[0]).includes('verified'))).toBe(
      true
    );
  });

  it('uses pendingUpdate.to as the expected version after a page reload', async () => {
    vi.mocked(waitForHubRestart).mockResolvedValue({ ok: false, observed: null });
    el = await mount({
      operations: restartingOps,
      status: updateStatus({
        pendingUpdate: {
          runId: 'run-1',
          from: 'nightly-20261022',
          to: 'nightly-20261023',
          at: '2026-10-24T00:00:00Z',
        },
      }),
    });
    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(waitForHubRestart).toHaveBeenCalledWith({ expectVersion: 'nightly-20261023' });
  });

  it('resolves a latest:<channel> run to pendingUpdate.to before waiting', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    let ops: Json[] = [op('update-binary', 'Update Binary')];
    let status = updateStatus();
    el = await mount({ operations: () => ops, status: () => status });
    el.updateConfirmTarget = 'latest:nightly';
    await settle(el);
    root(el)
      .querySelector<HTMLElement>('sl-dialog[label="Update Hub"] sl-button.confirm-update')!
      .click();
    await settle(el);

    status = updateStatus({
      pendingUpdate: {
        runId: 'run-1',
        from: 'nightly-20261022',
        to: 'nightly-20261023',
        at: '2026-10-24T00:00:00Z',
      },
    });
    ops = restartingOps();
    await el.loadData(true);
    await settle(el);

    expect(waitForHubRestart).toHaveBeenCalledWith({ expectVersion: 'nightly-20261023' });
    expect(versionSection(el).querySelector('.update-reconnecting')?.textContent).toContain(
      'nightly-20261023'
    );
  });

  it('tells the admin to check journalctl when the hub does not come back', async () => {
    vi.mocked(waitForHubRestart).mockResolvedValue({
      ok: false,
      observed: { version: 'nightly-20261022', uptime: '3s', uptimeSeconds: 3 },
    });
    let ops: Json[] = [op('update-binary', 'Update Binary')];
    el = await mount({ operations: () => ops });
    el.updateConfirmTarget = 'nightly-20261023';
    await settle(el);
    root(el)
      .querySelector<HTMLElement>('sl-dialog[label="Update Hub"] sl-button.confirm-update')!
      .click();
    await settle(el);
    ops = restartingOps();
    await el.loadData(true);
    await settle(el);

    const alert = versionSection(el).querySelector('sl-alert.update-reconnect-error');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('Hub did not come back on nightly-20261023');
    expect(alert!.textContent).toContain('journalctl -u scion-hub');
  });
});

describe('Restart Hub', () => {
  it('waits through the shared helper', async () => {
    vi.mocked(waitForHubRestart).mockResolvedValue({
      ok: true,
      observed: { version: 'v1', uptime: '2s', uptimeSeconds: 2 },
    });
    el = await mount();
    findButton(root(el), 'Restart')!.click();
    await settle(el);
    root(el)
      .querySelector<HTMLElement>('sl-dialog[label="Restart Hub"] sl-button[variant="warning"]')!
      .click();
    await settle(el);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(vi.mocked(waitForHubRestart).mock.calls[0][0]?.expectVersion).toBeUndefined();
    expect(showToast).toHaveBeenCalledWith('Server restarted successfully.', 'success');
  });
});
