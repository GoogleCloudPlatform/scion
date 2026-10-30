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
  status?: Json | number | (() => Json | number | Promise<Json>);
  /**
   * Body of GET update-available; a function may answer differently per
   * call, with an HTTP status to fail with, or 'down' to reject.
   */
  updateAvailable?: Json | (() => Json | number | 'down');
  /** Response to POST update-binary/run. */
  run?: { status: number; body: Json };
  /** Operations list; may change between calls. 'down' rejects the fetch. */
  operations?: () => Json[] | 'down' | Promise<Json[]>;
  /** Body of POST check-updates, or an HTTP status to fail with. */
  checkUpdates?: Json | number;
  /** Response to POST restart: 'down' rejects; text is a non-JSON body. */
  restart?: 'down' | { status: number; body?: Json; text?: string };
  /** Body of GET releases (default: no releases in any channel). */
  releases?: Json;
}

const EMPTY_CATALOG: Json = {
  repo: 'GoogleCloudPlatform/scion',
  fetchedAt: '2026-10-22T00:00:00Z',
  stale: false,
  channels: {
    nightly: { latest: null, releases: [] },
    preview: { latest: null, releases: [] },
    stable: { latest: null, releases: [] },
  },
};

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
      const raw = opts.status ?? updateStatus();
      const s = typeof raw === 'function' ? raw() : raw;
      if (s instanceof Promise) return s.then((b) => json(b));
      if (typeof s === 'number') {
        return json({ error: { code: 'internal_error', message: 'status unavailable' } }, s);
      }
      return json(s);
    }
    if (path === '/api/v1/admin/maintenance/releases') {
      return json(opts.releases ?? EMPTY_CATALOG);
    }
    if (path === '/api/v1/admin/maintenance/update-available') {
      const ua = opts.updateAvailable;
      const body = typeof ua === 'function' ? ua() : (ua ?? { update_available: false });
      if (body === 'down') return Promise.reject(new TypeError('Failed to fetch'));
      if (typeof body === 'number') {
        return json({ error: { code: 'internal_error', message: 'unavailable' } }, body);
      }
      return json(body);
    }
    if (method === 'POST' && path.endsWith('/run')) {
      runBodies.push(JSON.parse(init?.body as string));
      const r = opts.run ?? { status: 200, body: { runId: 'run-1', status: 'running' } };
      return json(r.body, r.status);
    }
    if (path === '/api/v1/admin/maintenance/restart' && opts.restart) {
      const r = opts.restart;
      if (r === 'down') return Promise.reject(new TypeError('Failed to fetch'));
      if (r.text !== undefined) {
        return Promise.resolve(
          new Response(r.text, { status: r.status, headers: { 'Content-Type': 'text/html' } })
        );
      }
      return json(r.body ?? {}, r.status);
    }
    if (path === '/api/v1/admin/maintenance/check-updates') {
      const c = opts.checkUpdates ?? {};
      if (typeof c === 'number') {
        return json({ error: { code: 'internal_error', message: 'check failed' } }, c);
      }
      return json(c);
    }
    if (path === '/api/v1/admin/maintenance/operations') {
      const ops = opts.operations ? opts.operations() : OPS;
      if (ops === 'down') return Promise.reject(new TypeError('Failed to fetch'));
      if (ops instanceof Promise) {
        return ops.then((o) => json({ migrations: [], operations: o }));
      }
      return json({ migrations: [], operations: ops });
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

/** The shadow root of the page's "Server Version & Updates" card. */
function cardRoot(e: Page): ShadowRoot {
  const card = root(e).querySelector('scion-admin-maintenance-update-card');
  expect(card?.shadowRoot).toBeTruthy();
  return card!.shadowRoot!;
}

function versionSection(e: Page): HTMLElement {
  const s = cardRoot(e).querySelector<HTMLElement>('.section.server-version');
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

function confirmUpdate(e: Page): void {
  root(e)
    .querySelector<HTMLElement>('sl-dialog[label="Update Hub"] sl-button.confirm-update')!
    .click();
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

  async function runCard(e: Page): Promise<void> {
    findButton(opCard(e, 'Update Binary')!, 'Run')!.click();
    await settle(e);
  }

  it('resolves the card target to the checked tag before posting', async () => {
    el = await mount({
      checkUpdates: {
        tier: 'binary',
        update_available: true,
        current_version: 'nightly-20261022',
        latest_version: 'nightly-20261023',
        channel: 'nightly',
      },
    });
    await runCard(el);
    expect(el.updateConfirmTarget).toBe('nightly-20261023');
    confirmUpdate(el);
    await settle(el);
    expect(runBodies).toEqual([{ params: { target_version: 'nightly-20261023', pin: 'latest' } }]);
  });

  it('does not open the dialog when the check says the hub is up to date', async () => {
    el = await mount({
      checkUpdates: {
        tier: 'binary',
        update_available: false,
        current_version: 'nightly-20261022',
        latest_version: 'nightly-20261022',
        channel: 'nightly',
      },
    });
    await runCard(el);
    expect(el.updateConfirmTarget).toBeNull();
    expect(showToast).toHaveBeenCalledWith('Already up to date (nightly-20261022).', 'success');
  });

  it('falls back to latest:<channel> when the check gives no tag', async () => {
    el = await mount({ checkUpdates: 500 });
    await runCard(el);
    expect(el.updateConfirmTarget).toBe('latest:nightly');
    confirmUpdate(el);
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
  const idle = (): Json[] => [op('update-binary', 'Update Binary')];
  const withRun = (status: string, id = 'run-1'): Json[] => [
    op('update-binary', 'Update Binary', { id, status, startedAt: '2026-10-24T00:00:00Z' }),
  ];
  const PENDING = {
    runId: 'run-1',
    from: 'nightly-20261022',
    to: 'nightly-20261023',
    at: '2026-10-24T00:00:00Z',
  };
  const verified = (runId: string): Json =>
    updateStatus({
      current: {
        version: 'nightly-20261023',
        channel: 'nightly',
        commit: 'f00',
        isGitBuild: false,
      },
      lastUpdate: {
        runId,
        from: 'nightly-20261022',
        to: 'nightly-20261023',
        status: 'verified',
        at: '2026-10-24T00:01:00Z',
      },
    });

  /** Mounts with mutable operations/status and starts an update to target. */
  async function startUpdate(
    target: string,
    extra: Options = {}
  ): Promise<{ setOps(o: Json[] | 'down'): void; setStatus(s: Json | number): void }> {
    let ops: Json[] | 'down' = idle();
    let status: Json | number = updateStatus();
    el = await mount({ ...extra, operations: () => ops, status: () => status });
    el.updateConfirmTarget = target;
    await settle(el);
    ops = withRun('running');
    confirmUpdate(el);
    await settle(el);
    return {
      setOps: (o) => (ops = o),
      setStatus: (s) => (status = s),
    };
  }

  function waitArgs(): { expectVersion?: string; signal?: AbortSignal } | undefined {
    return vi.mocked(waitForHubRestart).mock.calls[0]?.[0];
  }

  it('starts the wait after two failed polls while the run is running', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    const hub = await startUpdate('nightly-20261023');
    const statusLoads = statusFetchCount();

    hub.setOps('down');
    await el!.loadData(true);
    await settle(el!);
    // One failure may be a blip: no wait yet.
    expect(waitForHubRestart).not.toHaveBeenCalled();

    await el!.loadData(true);
    await settle(el!);
    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(waitArgs()?.expectVersion).toBe('nightly-20261023');
    // The header keeps the last known status during the wait.
    expect(statusFetchCount()).toBe(statusLoads);
    expect(versionSection(el!).querySelector('.update-status-error')).toBeNull();
    expect(versionSection(el!).querySelector('.current-version')?.textContent).toBe(
      'nightly-20261022'
    );
    expect(versionSection(el!).querySelector('.update-reconnecting')?.textContent).toContain(
      'nightly-20261023'
    );
  });

  it('shows the journalctl alert when the hub does not come back after failed polls', async () => {
    vi.mocked(waitForHubRestart).mockResolvedValue({ ok: false, observed: null });
    const hub = await startUpdate('nightly-20261023');
    hub.setOps('down');
    await el!.loadData(true);
    await el!.loadData(true);
    await settle(el!);

    const alert = versionSection(el!).querySelector('sl-alert.update-reconnect-error');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('Hub did not come back on nightly-20261023');
    expect(alert!.textContent).toContain('journalctl -u scion-hub');
  });

  it('waits at once when a poll sees restarting, then reports the matching lastUpdate', async () => {
    let hub: Awaited<ReturnType<typeof startUpdate>> | null = null;
    vi.mocked(waitForHubRestart).mockImplementation(() => {
      // The hub restarts on the target and settles the run on boot.
      hub!.setOps(withRun('completed'));
      hub!.setStatus(verified('run-1'));
      return Promise.resolve({
        ok: true,
        observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
      });
    });
    hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(waitArgs()?.expectVersion).toBe('nightly-20261023');
    expect(versionSection(el!).querySelector('.current-version')?.textContent).toBe(
      'nightly-20261023'
    );
    expect(
      versionSection(el!).querySelector('.last-update .status-badge')?.textContent?.trim()
    ).toBe('verified');
    expect(showToast).toHaveBeenCalledWith(
      'Hub updated to nightly-20261023 (verified).',
      'success'
    );
  });

  it('does not report a lastUpdate that belongs to another run', async () => {
    let hub: Awaited<ReturnType<typeof startUpdate>> | null = null;
    vi.mocked(waitForHubRestart).mockImplementation(() => {
      hub!.setOps(withRun('completed'));
      hub!.setStatus(verified('run-0'));
      return Promise.resolve({
        ok: true,
        observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
      });
    });
    hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);

    const toasts = vi.mocked(showToast).mock.calls.map((c) => String(c[0]));
    expect(toasts).toContain('Hub restarted; the update result is not recorded yet.');
    expect(toasts.some((t) => t.includes('(verified)'))).toBe(false);
  });

  it('reports the result when the restart happened between two polls', async () => {
    const hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('completed'));
    hub.setStatus(verified('run-1'));
    await el!.loadData(true);
    await settle(el!);

    expect(waitForHubRestart).not.toHaveBeenCalled();
    expect(showToast).toHaveBeenCalledWith(
      'Hub updated to nightly-20261023 (verified).',
      'success'
    );
  });

  it('resolves a latest:<channel> run from pendingUpdate without touching the header', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    const hub = await startUpdate('latest:nightly');
    hub.setStatus(updateStatus({ pendingUpdate: PENDING }));
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);

    expect(waitArgs()?.expectVersion).toBe('nightly-20261023');
  });

  it('keeps the header when the pendingUpdate peek fails', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    const hub = await startUpdate('latest:nightly');
    hub.setStatus(502);
    hub.setOps('down');
    await el!.loadData(true);
    await el!.loadData(true);
    await settle(el!);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(waitArgs()?.expectVersion).toBeUndefined();
    expect(versionSection(el!).querySelector('.update-status-error')).toBeNull();
    expect(cardTitles(el!)).toEqual(['Update Binary']);
  });

  it('follows a restarting run after a page reload when pendingUpdate names it', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    el = await mount({
      operations: () => withRun('restarting'),
      status: updateStatus({ pendingUpdate: PENDING }),
    });
    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(waitArgs()?.expectVersion).toBe('nightly-20261023');
  });

  it('shows a stale restarting run as in progress without waiting', async () => {
    el = await mount({ operations: () => withRun('restarting') });
    expect(waitForHubRestart).not.toHaveBeenCalled();
    const card = opCard(el, 'Update Binary');
    expect(findButton(card!, 'Restarting...')).toBeDefined();
    expect(findButton(card!, 'Run')).toBeUndefined();
    expect(card!.querySelector('.status-badge.restarting')).not.toBeNull();
  });

  it('does not follow a restarting run that pendingUpdate does not name', async () => {
    el = await mount({
      operations: () => withRun('restarting', 'run-7'),
      status: updateStatus({ pendingUpdate: PENDING }),
    });
    expect(waitForHubRestart).not.toHaveBeenCalled();
  });

  const failed = (runId: string): Json =>
    updateStatus({
      lastUpdate: {
        runId,
        from: 'nightly-20261022',
        to: 'nightly-20261023',
        status: 'failed',
        at: '2026-10-24T00:01:00Z',
      },
    });

  /** Mounts with an update run already in progress (started elsewhere). */
  async function mountInProgress(): Promise<{
    setOps(o: Json[] | 'down'): void;
    setStatus(s: Json | number): void;
  }> {
    let ops: Json[] | 'down' = withRun('running');
    let status: Json | number = updateStatus();
    el = await mount({ operations: () => ops, status: () => status });
    return { setOps: (o) => (ops = o), setStatus: (s) => (status = s) };
  }

  it('follows a run already running at page load through restarting', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    const hub = await mountInProgress();
    hub.setStatus(updateStatus({ pendingUpdate: PENDING }));
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(waitArgs()?.expectVersion).toBe('nightly-20261023');
  });

  it('follows a run already running at page load through failed polls to its result', async () => {
    let hub: Awaited<ReturnType<typeof mountInProgress>> | null = null;
    vi.mocked(waitForHubRestart).mockImplementation(() => {
      hub!.setOps(withRun('completed'));
      hub!.setStatus(verified('run-1'));
      return Promise.resolve({
        ok: true,
        observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
      });
    });
    hub = await mountInProgress();
    hub.setOps('down');
    await el!.loadData(true);
    await el!.loadData(true);
    await settle(el!);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(versionSection(el!).querySelector('.current-version')?.textContent).toBe(
      'nightly-20261023'
    );
    expect(showToast).toHaveBeenCalledWith(
      'Hub updated to nightly-20261023 (verified).',
      'success'
    );
  });

  it('keeps polling a restarting run it does not wait for', async () => {
    el = await mount({ operations: () => withRun('restarting') });
    expect(waitForHubRestart).not.toHaveBeenCalled();
    expect((el as unknown as { pollTimer: unknown }).pollTimer).not.toBeNull();
  });

  it('reports the recorded failure when the hub answers on another version', async () => {
    let hub: Awaited<ReturnType<typeof startUpdate>> | null = null;
    vi.mocked(waitForHubRestart).mockImplementation(() => {
      // Boot reconciliation found the old version running and failed the run.
      hub!.setOps(withRun('failed'));
      hub!.setStatus(failed('run-1'));
      return Promise.resolve({
        ok: false,
        observed: { version: 'nightly-20261022', uptime: '5s', uptimeSeconds: 5 },
      });
    });
    hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);

    expect(showToast).toHaveBeenCalledWith('Update to nightly-20261023 failed.', 'warning');
    expect(versionSection(el!).querySelector('sl-alert.update-reconnect-error')).toBeNull();
    expect(
      versionSection(el!).querySelector('.last-update .status-badge')?.textContent?.trim()
    ).toBe('failed');
  });

  it('goes back to polling when the wait ends with the run still running', async () => {
    let hub: Awaited<ReturnType<typeof startUpdate>> | null = null;
    vi.mocked(waitForHubRestart).mockImplementation(() => {
      // Two polls failed as a blip; the download is still going.
      hub!.setOps(withRun('running'));
      return Promise.resolve({
        ok: false,
        observed: { version: 'nightly-20261022', uptime: '2h0m0s', uptimeSeconds: 7200 },
      });
    });
    hub = await startUpdate('nightly-20261023');
    hub.setOps('down');
    await el!.loadData(true);
    await el!.loadData(true);
    await settle(el!);

    expect(versionSection(el!).querySelector('sl-alert.update-reconnect-error')).toBeNull();
    expect(versionSection(el!).querySelector('.update-reconnecting')).toBeNull();
    expect((el as unknown as { pollTimer: unknown }).pollTimer).not.toBeNull();
  });

  it('keeps the update wait when Restart Hub is used during it', async () => {
    let finishUpdateWait: (() => void) | null = null;
    let hub: Awaited<ReturnType<typeof startUpdate>> | null = null;
    vi.mocked(waitForHubRestart).mockImplementation((opts) => {
      if (opts?.expectVersion === 'nightly-20261023') {
        return new Promise((resolve) => {
          finishUpdateWait = (): void => {
            hub!.setOps(withRun('completed'));
            hub!.setStatus(verified('run-1'));
            resolve({
              ok: true,
              observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
            });
          };
        });
      }
      return Promise.resolve({
        ok: true,
        observed: { version: 'nightly-20261022', uptime: '2s', uptimeSeconds: 2 },
      });
    });
    hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);
    const updateSignal = waitArgs()?.signal;

    findButton(root(el!), 'Restart')!.click();
    await settle(el!);
    root(el!)
      .querySelector<HTMLElement>('sl-dialog[label="Restart Hub"] sl-button[variant="warning"]')!
      .click();
    await settle(el!);

    expect(waitForHubRestart).toHaveBeenCalledTimes(2);
    expect(updateSignal?.aborted).toBe(false);
    expect(showToast).toHaveBeenCalledWith('Server restarted successfully.', 'success');

    finishUpdateWait!();
    await settle(el!);
    expect(versionSection(el!).querySelector('.update-reconnecting')).toBeNull();
    expect(showToast).toHaveBeenCalledWith(
      'Hub updated to nightly-20261023 (verified).',
      'success'
    );
  });

  it('does not start a wait from a poll that resolves after the page was left', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    let release: ((o: Json[]) => void) | null = null;
    let ops: Json[] | 'down' | Promise<Json[]> = withRun('running');
    el = await mount({
      operations: () => ops,
      status: updateStatus({ pendingUpdate: PENDING }),
    });
    ops = new Promise<Json[]>((resolve) => (release = resolve));
    const poll = el.loadData(true);
    el.remove();
    release!(withRun('restarting'));
    await poll;
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(waitForHubRestart).not.toHaveBeenCalled();
    expect((el as unknown as { pollTimer: unknown }).pollTimer).toBeNull();
  });

  it('recovers the page when the list load right after the POST fails', async () => {
    let ops: Json[] | 'down' = idle();
    let status: Json | number = updateStatus();
    vi.mocked(waitForHubRestart).mockImplementation(() => {
      ops = withRun('completed');
      status = verified('run-1');
      return Promise.resolve({
        ok: true,
        observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
      });
    });
    el = await mount({ operations: () => ops, status: () => status });
    el.updateConfirmTarget = 'nightly-20261023';
    await settle(el);
    // The hub is already going down when the POST returns.
    ops = 'down';
    confirmUpdate(el);
    await settle(el);
    expect(root(el).querySelector('.error-state')).toBeNull();
    expect(cardRoot(el).querySelector('.section.server-version')).not.toBeNull();

    await el.loadData(true);
    await settle(el);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(root(el).querySelector('.error-state')).toBeNull();
    expect(versionSection(el).querySelector('.current-version')?.textContent).toBe(
      'nightly-20261023'
    );
    expect(showToast).toHaveBeenCalledWith(
      'Hub updated to nightly-20261023 (verified).',
      'success'
    );
  });

  it('clears the error view after a later successful load', async () => {
    let ops: Json[] | 'down' = 'down';
    el = await mount({ operations: () => ops });
    expect(root(el).querySelector('.error-state')).not.toBeNull();
    ops = idle();
    await el.loadData(true);
    await settle(el);
    expect(root(el).querySelector('.error-state')).toBeNull();
  });

  it('ends the wait early when the run fails while the hub keeps answering', async () => {
    let hub: Awaited<ReturnType<typeof startUpdate>> | null = null;
    let stopAnswer: boolean | undefined;
    vi.mocked(waitForHubRestart).mockImplementation(async (opts) => {
      const old = { version: 'nightly-20261022', uptime: '2h0m0s', uptimeSeconds: 7200 };
      // Restart() failed: the run is marked failed, the old hub stays up.
      hub!.setOps(withRun('failed'));
      hub!.setStatus(failed('run-1'));
      stopAnswer = await opts!.stopWhen!(old);
      return { ok: false, observed: old, stopped: true };
    });
    hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);

    expect(stopAnswer).toBe(true);
    expect(showToast).toHaveBeenCalledWith('Update to nightly-20261023 failed.', 'warning');
    expect(versionSection(el!).querySelector('sl-alert.update-reconnect-error')).toBeNull();
  });

  it('does not end the wait while the run is still restarting', async () => {
    let stopAnswer: boolean | undefined;
    vi.mocked(waitForHubRestart).mockImplementation(async (opts) => {
      stopAnswer = await opts!.stopWhen!({
        version: 'nightly-20261022',
        uptime: '2h0m0s',
        uptimeSeconds: 7200,
      });
      return new Promise(() => {});
    });
    const hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);
    expect(stopAnswer).toBe(false);
  });

  it('does not tie a result to the update when the POST returns no run id', async () => {
    let ops: Json[] = [
      op('update-binary', 'Update Binary', { id: 'run-0', status: 'completed', startedAt: '' }),
    ];
    el = await mount({
      operations: () => ops,
      status: () => verified('run-0'),
      run: { status: 200, body: { status: 'running' } },
    });
    el.updateConfirmTarget = 'nightly-20261023';
    await settle(el);
    confirmUpdate(el);
    await settle(el);
    ops = [...ops];
    await el.loadData(true);
    await settle(el);

    const toasts = vi.mocked(showToast).mock.calls.map((c) => String(c[0]));
    expect(toasts).toContain('The hub did not return a run id; follow the update in Run History.');
    expect(toasts.some((t) => t.includes('(verified)'))).toBe(false);
    expect((el as unknown as { trackedUpdate: unknown }).trackedUpdate).toBeNull();
  });

  it('does not wait again on a run still restarting after a timed-out wait', async () => {
    vi.mocked(waitForHubRestart).mockResolvedValue({
      ok: false,
      observed: { version: 'nightly-20261022', uptime: '2h0m0s', uptimeSeconds: 7200 },
    });
    const hub = await startUpdate('nightly-20261023');
    // Restart() returned but the process never restarted: the run stays
    // restarting and pendingUpdate keeps naming it.
    hub.setStatus(updateStatus({ pendingUpdate: PENDING }));
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);
    await settle(el!);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    const alert = versionSection(el!).querySelector('sl-alert.update-reconnect-error');
    expect(alert?.textContent).toContain('journalctl -u scion-hub');
    expect(versionSection(el!).querySelector('.update-reconnecting')).toBeNull();
  });

  it('passes the wait signal to the run status check', async () => {
    let checkSignal: AbortSignal | null | undefined;
    vi.mocked(waitForHubRestart).mockImplementation(async (opts) => {
      await opts!.stopWhen!({ version: 'nightly-20261022', uptime: '2h0m0s', uptimeSeconds: 7200 });
      const call = fetchMock.mock.calls
        .filter((c) => pathOf(c[0] as string) === '/api/v1/admin/maintenance/operations')
        .at(-1);
      checkSignal = (call?.[1] as RequestInit | undefined)?.signal;
      return new Promise(() => {});
    });
    const hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);

    expect(checkSignal).toBeDefined();
    expect(checkSignal).toBe(waitArgs()?.signal);
  });

  it('aborts the wait when the page is left', async () => {
    vi.mocked(waitForHubRestart).mockReturnValue(new Promise(() => {}));
    const hub = await startUpdate('nightly-20261023');
    hub.setOps(withRun('restarting'));
    await el!.loadData(true);
    await settle(el!);
    const signal = waitArgs()?.signal;
    expect(signal?.aborted).toBe(false);
    el!.remove();
    expect(signal?.aborted).toBe(true);
  });

  // ptone/scion#6557: after the hub comes back on a new version, the
  // "Update available" notices computed against the old version go away
  // without a manual re-check.
  describe('Update notices after the hub restarts', () => {
    const CHECK_AVAILABLE: Json = {
      tier: 'binary',
      update_available: true,
      current_version: 'nightly-20261022',
      latest_version: 'nightly-20261023',
      channel: 'nightly',
    };
    const NOTIFIED: Json = {
      update_available: true,
      update: {
        version: 'nightly-20261023',
        channel: 'nightly',
        detected_at: '2026-10-23T00:00:00Z',
      },
    };

    type Notices = { updateCheckResult: unknown; updateAvailable: unknown };
    const notices = (e: Page): Notices => e as unknown as Notices;

    /** The top "Update available" banner (not the unit-drift banner). */
    function banner(e: Page): Element | null {
      return root(e).querySelector('.update-available-banner:not(.unit-drift-banner)');
    }

    function availableFetchCount(): number {
      return fetchMock.mock.calls.filter(
        (c) => pathOf(c[0] as string) === '/api/v1/admin/maintenance/update-available'
      ).length;
    }

    /**
     * Starts an update with a binary check result shown on the Update
     * Binary card, then lets the hub come back. comeBack runs inside the
     * wait and returns the observed answer.
     */
    async function updateAndComeBack(
      available: () => Json | number | 'down',
      comeBack: (
        hub: Awaited<ReturnType<typeof startUpdate>>
      ) => Awaited<ReturnType<typeof waitForHubRestart>>
    ): Promise<void> {
      let hub: Awaited<ReturnType<typeof startUpdate>> | null = null;
      vi.mocked(waitForHubRestart).mockImplementation(() => Promise.resolve(comeBack(hub!)));
      hub = await startUpdate('nightly-20261023', {
        checkUpdates: CHECK_AVAILABLE,
        updateAvailable: available,
      });
      await (el as unknown as { checkForUpdates(): Promise<void> }).checkForUpdates();
      await settle(el!);
      expect(root(el!).querySelector('.update-check-banner')?.textContent).toContain(
        'nightly-20261023'
      );
      hub.setOps(withRun('restarting'));
      await el!.loadData(true);
      await settle(el!);
    }

    it('clears the check result and re-reads the notification on a new version', async () => {
      let calls = 0;
      // On connect the hub still notifies nightly-20261023; after the
      // update it has nothing newer.
      await updateAndComeBack(
        () => (calls++ === 0 ? NOTIFIED : { update_available: false }),
        (hub) => {
          hub.setOps(withRun('completed'));
          hub.setStatus(verified('run-1'));
          return {
            ok: true,
            observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
          };
        }
      );

      expect(versionSection(el!).querySelector('.current-version')?.textContent).toBe(
        'nightly-20261023'
      );
      expect(notices(el!).updateCheckResult).toBeNull();
      expect(root(el!).querySelector('.update-check-banner')).toBeNull();
      expect(availableFetchCount()).toBe(2);
      expect(notices(el!).updateAvailable).toEqual({ update_available: false });
      expect(banner(el!)).toBeNull();
      expect(showToast).toHaveBeenCalledWith(
        'Hub updated to nightly-20261023 (verified).',
        'success'
      );
    });

    it('also clears them when a poll sees the run finish between restarts', async () => {
      let calls = 0;
      const hub = await startUpdate('nightly-20261023', {
        checkUpdates: CHECK_AVAILABLE,
        updateAvailable: () => (calls++ === 0 ? NOTIFIED : { update_available: false }),
      });
      await (el as unknown as { checkForUpdates(): Promise<void> }).checkForUpdates();
      await settle(el!);
      hub.setOps(withRun('completed'));
      hub.setStatus(verified('run-1'));
      await el!.loadData(true);
      await settle(el!);

      expect(waitForHubRestart).not.toHaveBeenCalled();
      expect(versionSection(el!).querySelector('.current-version')?.textContent).toBe(
        'nightly-20261023'
      );
      expect(notices(el!).updateCheckResult).toBeNull();
      expect(root(el!).querySelector('.update-check-banner')).toBeNull();
      expect(availableFetchCount()).toBe(2);
      expect(notices(el!).updateAvailable).toEqual({ update_available: false });
      expect(banner(el!)).toBeNull();
    });

    it('keeps the check result and brings the banner back when a run fails before the restart', async () => {
      const hub = await startUpdate('nightly-20261023', {
        checkUpdates: CHECK_AVAILABLE,
        updateAvailable: () => NOTIFIED,
      });
      await (el as unknown as { checkForUpdates(): Promise<void> }).checkForUpdates();
      await settle(el!);
      // Starting the update hid the banner.
      expect(banner(el!)).toBeNull();
      // The poll sees the run failed on the old version (no restart).
      hub.setOps(withRun('failed'));
      hub.setStatus(failed('run-1'));
      await el!.loadData(true);
      await settle(el!);

      expect(waitForHubRestart).not.toHaveBeenCalled();
      expect(versionSection(el!).querySelector('.current-version')?.textContent).toBe(
        'nightly-20261022'
      );
      expect(notices(el!).updateCheckResult).toEqual(CHECK_AVAILABLE);
      expect(root(el!).querySelector('.update-check-banner')?.textContent).toContain(
        'nightly-20261023'
      );
      // The stored notification is re-read and shown again for a retry.
      expect(availableFetchCount()).toBe(2);
      expect(notices(el!).updateAvailable).toEqual(NOTIFIED);
      expect(banner(el!)?.textContent).toContain('nightly-20261023');
      expect(showToast).toHaveBeenCalledWith('Update to nightly-20261023 failed.', 'warning');
    });

    it('clears them when a status reload during the wait already shows the new version', async () => {
      let calls = 0;
      let hub: Awaited<ReturnType<typeof startUpdate>> | null = null;
      vi.mocked(waitForHubRestart).mockImplementation(async () => {
        hub!.setOps(withRun('completed'));
        hub!.setStatus(verified('run-1'));
        // The user presses Reload status while the wait runs.
        root(el!)
          .querySelector('scion-admin-maintenance-update-card')!
          .dispatchEvent(new CustomEvent('reload-status', { bubbles: true, composed: true }));
        await settle(el!);
        expect(versionSection(el!).querySelector('.current-version')?.textContent).toBe(
          'nightly-20261023'
        );
        return {
          ok: true,
          observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
        };
      });
      hub = await startUpdate('nightly-20261023', {
        checkUpdates: CHECK_AVAILABLE,
        updateAvailable: () => (calls++ === 0 ? NOTIFIED : { update_available: false }),
      });
      await (el as unknown as { checkForUpdates(): Promise<void> }).checkForUpdates();
      await settle(el!);
      hub.setOps(withRun('restarting'));
      await el!.loadData(true);
      await settle(el!);

      expect(notices(el!).updateCheckResult).toBeNull();
      expect(root(el!).querySelector('.update-check-banner')).toBeNull();
      expect(availableFetchCount()).toBe(2);
      expect(banner(el!)).toBeNull();
    });

    it('keeps the check result and brings the banner back when the hub comes back on the same version', async () => {
      await updateAndComeBack(
        () => NOTIFIED,
        (hub) => {
          hub.setOps(withRun('failed'));
          hub.setStatus(failed('run-1'));
          return {
            ok: false,
            observed: { version: 'nightly-20261022', uptime: '5s', uptimeSeconds: 5 },
          };
        }
      );

      expect(versionSection(el!).querySelector('.current-version')?.textContent).toBe(
        'nightly-20261022'
      );
      expect(notices(el!).updateCheckResult).toEqual(CHECK_AVAILABLE);
      expect(root(el!).querySelector('.update-check-banner')?.textContent).toContain(
        'nightly-20261023'
      );
      // Re-read (not cleared): the hub keeps the notification for a retry.
      expect(availableFetchCount()).toBe(2);
      expect(notices(el!).updateAvailable).toEqual(NOTIFIED);
      expect(banner(el!)?.textContent).toContain('nightly-20261023');
      expect(showToast).toHaveBeenCalledWith('Update to nightly-20261023 failed.', 'warning');
    });

    it('resolves an unknown before-version from lastUpdate for a run adopted on first load', async () => {
      // The operations list answers before update-status, so the running
      // run is adopted with no version known.
      let releaseStatus: (b: Json) => void = () => {};
      const firstStatus = new Promise<Json>((resolve) => {
        releaseStatus = resolve;
      });
      let ops: Json[] = withRun('running');
      let status: Json | Promise<Json> = firstStatus;
      let calls = 0;
      el = await mount({
        operations: () => ops,
        status: () => status,
        checkUpdates: CHECK_AVAILABLE,
        updateAvailable: () => (calls++ === 0 ? NOTIFIED : { update_available: false }),
      });
      const tracked = (el as unknown as { trackedUpdate: { fromVersion?: string } | null })
        .trackedUpdate;
      expect(tracked).not.toBeNull();
      expect(tracked!.fromVersion).toBeUndefined();
      releaseStatus(updateStatus());
      await settle(el);
      await (el as unknown as { checkForUpdates(): Promise<void> }).checkForUpdates();
      await settle(el);

      // The poll sees the run finish; the hub verified nightly-20261023.
      ops = withRun('completed');
      status = verified('run-1');
      await el.loadData(true);
      await settle(el);

      expect(versionSection(el).querySelector('.current-version')?.textContent).toBe(
        'nightly-20261023'
      );
      expect(notices(el).updateCheckResult).toBeNull();
      expect(availableFetchCount()).toBe(2);
      expect(notices(el).updateAvailable).toEqual({ update_available: false });
    });

    it('takes the before-version of an adopted restarting run from pendingUpdate.from', async () => {
      // The status read during the restart already shows the new binary;
      // only pendingUpdate.from says what ran before.
      let finishWait: () => void = () => {};
      let ops: Json[] = withRun('restarting');
      let status: Json = updateStatus({
        current: {
          version: 'nightly-20261023',
          channel: 'nightly',
          commit: 'f00',
          isGitBuild: false,
        },
        pendingUpdate: PENDING,
      });
      let calls = 0;
      vi.mocked(waitForHubRestart).mockImplementation(
        () =>
          new Promise<Awaited<ReturnType<typeof waitForHubRestart>>>((resolve) => {
            finishWait = (): void =>
              resolve({
                ok: true,
                observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
              });
          })
      );
      el = await mount({
        operations: () => ops,
        status: () => status,
        checkUpdates: CHECK_AVAILABLE,
        updateAvailable: () => (calls++ === 0 ? NOTIFIED : { update_available: false }),
      });
      expect(waitForHubRestart).toHaveBeenCalledTimes(1);
      const tracked = (el as unknown as { trackedUpdate: { fromVersion?: string } | null })
        .trackedUpdate;
      expect(tracked?.fromVersion).toBe('nightly-20261022');
      await (el as unknown as { checkForUpdates(): Promise<void> }).checkForUpdates();
      await settle(el);

      ops = withRun('completed');
      status = verified('run-1');
      finishWait();
      await settle(el);

      expect(notices(el).updateCheckResult).toBeNull();
      expect(availableFetchCount()).toBe(2);
    });

    it.each([
      [
        'not verified yet',
        { lastUpdate: null, pendingUpdate: PENDING },
        'Hub restarted; the update result is not recorded yet.',
      ],
      [
        'rolled back',
        {
          lastUpdate: {
            runId: 'run-1',
            from: 'nightly-20261022',
            to: 'nightly-20261023',
            status: 'rolled_back',
            at: '2026-10-24T00:01:00Z',
          },
        },
        'Update to nightly-20261023 rolled back.',
      ],
    ] as Array<[string, Json, string]>)(
      'keeps the check result when the new binary answers but the run is %s',
      async (_name, record, toast) => {
        await updateAndComeBack(
          () => NOTIFIED,
          (hub) => {
            hub.setOps(withRun('completed'));
            hub.setStatus(
              updateStatus({
                current: {
                  version: 'nightly-20261023',
                  channel: 'nightly',
                  commit: 'f00',
                  isGitBuild: false,
                },
                ...record,
              })
            );
            return {
              ok: true,
              observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
            };
          }
        );

        expect(notices(el!).updateCheckResult).toEqual(CHECK_AVAILABLE);
        expect(root(el!).querySelector('.update-check-banner')?.textContent).toContain(
          'nightly-20261023'
        );
        // Safe branch: the stored notification is re-read, not cleared.
        expect(availableFetchCount()).toBe(2);
        expect(notices(el!).updateAvailable).toEqual(NOTIFIED);
        expect(showToast).toHaveBeenCalledWith(toast, 'warning');
      }
    );

    it.each([
      ['an HTTP error', 500],
      ['a network error', 'down'],
    ] as Array<[string, number | 'down']>)(
      'finishes the reconnect when re-reading the notification fails with %s',
      async (_name, failure) => {
        let calls = 0;
        await updateAndComeBack(
          () => (calls++ === 0 ? NOTIFIED : failure),
          (hub) => {
            hub.setOps(withRun('completed'));
            hub.setStatus(verified('run-1'));
            return {
              ok: true,
              observed: { version: 'nightly-20261023', uptime: '3s', uptimeSeconds: 3 },
            };
          }
        );

        expect(availableFetchCount()).toBe(2);
        // The old notice does not stay up when the re-read fails.
        expect(notices(el!).updateAvailable).toBeNull();
        expect(banner(el!)).toBeNull();
        expect(notices(el!).updateCheckResult).toBeNull();
        expect(root(el!).querySelector('.error-state')).toBeNull();
        expect(versionSection(el!).querySelector('sl-alert.update-reconnect-error')).toBeNull();
        expect(versionSection(el!).querySelector('.update-reconnecting')).toBeNull();
        expect(showToast).toHaveBeenCalledWith(
          'Hub updated to nightly-20261023 (verified).',
          'success'
        );
      }
    );
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

  async function restartHub(e: Page): Promise<void> {
    findButton(root(e), 'Restart')!.click();
    await settle(e);
    root(e)
      .querySelector<HTMLElement>('sl-dialog[label="Restart Hub"] sl-button[variant="warning"]')!
      .click();
    await settle(e);
  }

  it('shows the hub refusal and does not wait when restart returns 409', async () => {
    const message =
      'Cannot restart the hub while update-binary is in progress (run run-1, running); wait for it to finish';
    el = await mount({
      restart: {
        status: 409,
        body: {
          error: {
            code: 'conflict',
            message,
            details: { runId: 'run-1', operationKey: 'update-binary', status: 'running' },
          },
        },
      },
    });
    await restartHub(el);

    expect(waitForHubRestart).not.toHaveBeenCalled();
    expect(showToast).toHaveBeenCalledWith(`Restart refused: ${message}`, 'warning');
    expect((el as unknown as { restartLoading: boolean }).restartLoading).toBe(false);
    expect(findButton(root(el), 'Restart')).toBeDefined();
  });

  it('still waits when the restart request fails at the network', async () => {
    vi.mocked(waitForHubRestart).mockResolvedValue({
      ok: true,
      observed: { version: 'v1', uptime: '2s', uptimeSeconds: 2 },
    });
    el = await mount({ restart: 'down' });
    await restartHub(el);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith('Server restarted successfully.', 'success');
  });

  it('still waits on a 502 with a JSON body that is not the hub envelope', async () => {
    vi.mocked(waitForHubRestart).mockResolvedValue({
      ok: true,
      observed: { version: 'v1', uptime: '2s', uptimeSeconds: 2 },
    });
    el = await mount({ restart: { status: 502, body: { message: 'Bad Gateway' } } });
    await restartHub(el);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
  });

  it('leaves a 403 to the access-denied toast and does not wait', async () => {
    el = await mount({
      restart: {
        status: 403,
        body: { error: { code: 'forbidden', message: 'Access denied' } },
      },
    });
    await restartHub(el);

    expect(waitForHubRestart).not.toHaveBeenCalled();
    const toasts = vi.mocked(showToast).mock.calls.map((c) => String(c[0]));
    expect(toasts.some((t) => t.startsWith('Restart refused'))).toBe(false);
    expect((el as unknown as { restartLoading: boolean }).restartLoading).toBe(false);
  });

  it('still waits on a reverse proxy 502 page', async () => {
    vi.mocked(waitForHubRestart).mockResolvedValue({
      ok: true,
      observed: { version: 'v1', uptime: '2s', uptimeSeconds: 2 },
    });
    el = await mount({ restart: { status: 502, text: '<html>502 Bad Gateway</html>' } });
    await restartHub(el);

    expect(waitForHubRestart).toHaveBeenCalledTimes(1);
  });
});

// ptone/scion#6134: the binary check result's release_url comes from the
// release manifest; only an https URL becomes a link.
describe('Release notes link', () => {
  it.each([
    ['https', 'https://github.com/GoogleCloudPlatform/scion/releases/tag/nightly-20261023', true],
    ['javascript:', 'javascript:alert(document.cookie)', false],
    ['data:', 'data:text/html,<script>alert(1)</script>', false],
    ['http:', 'http://example.com/releases/nightly-20261023', false],
    ['relative', '/releases/nightly-20261023', false],
    ['malformed', 'https://', false],
  ])('renders a %s release_url safely', async (_name, url, linked) => {
    el = await mount({
      checkUpdates: {
        tier: 'binary',
        update_available: true,
        current_version: 'nightly-20261022',
        latest_version: 'nightly-20261023',
        channel: 'nightly',
        release_url: url,
      },
    });
    await (el as unknown as { checkForUpdates(): Promise<void> }).checkForUpdates();
    await settle(el);
    const link = root(el).querySelector<HTMLAnchorElement>('a.release-notes-link');
    const text = root(el).querySelector<HTMLElement>('.release-notes-text');
    if (linked) {
      expect(link?.getAttribute('href')).toBe(url);
      expect(text).toBeNull();
    } else {
      expect(link).toBeNull();
      expect(text?.textContent).toBe(url);
      const hrefs = Array.from(root(el).querySelectorAll('a')).map((a) => a.getAttribute('href'));
      expect(hrefs).not.toContain(url);
    }
  });
});

// Phase 2: the release picker in the card asks the page to update; the page
// confirms and posts {target_version, channel, pin}.
describe('Release picker updates', () => {
  const CATALOG: Json = {
    repo: 'GoogleCloudPlatform/scion',
    fetchedAt: '2026-10-23T10:00:00Z',
    stale: false,
    channels: {
      nightly: {
        latest: 'nightly-20261023',
        releases: [
          { tag: 'nightly-20261023', installable: true, class: 'upgrade', local: false },
          { tag: 'nightly-20261022', installable: true, class: 'same', local: true },
        ],
      },
      preview: {
        latest: 'v0.3.0-preview.4',
        releases: [{ tag: 'v0.3.0-preview.4', installable: true, class: 'switch-newer' }],
      },
      stable: { latest: null, releases: [] },
    },
  };

  type Select = HTMLElement & { value: string };

  function pick(e: Page, cls: string, value: string): void {
    const sel = cardRoot(e).querySelector<Select>(`sl-select.${cls}`)!;
    sel.value = value;
    sel.dispatchEvent(new Event('sl-change'));
  }

  function updateButton(e: Page): HTMLElement {
    return cardRoot(e).querySelector<HTMLElement>('sl-button.update-release')!;
  }

  function dialogText(e: Page): string {
    return root(e).querySelector('sl-dialog[label="Update Hub"]')?.textContent ?? '';
  }

  it('posts the latest tag with its channel and pin latest', async () => {
    el = await mount({ releases: CATALOG });
    updateButton(el).click();
    await settle(el);
    expect(el.updateConfirmTarget).toBe('nightly-20261023');
    expect(dialogText(el)).toContain('follows the latest');
    expect(dialogText(el)).not.toContain('switches the hub');
    confirmUpdate(el);
    await settle(el);
    expect(runBodies).toEqual([
      { params: { target_version: 'nightly-20261023', channel: 'nightly', pin: 'latest' } },
    ]);
  });

  it('posts a picked tag in another channel pinned to that tag', async () => {
    el = await mount({ releases: CATALOG });
    pick(el, 'channel-select', 'preview');
    await settle(el);
    pick(el, 'release-select', 'v0.3.0-preview.4');
    await settle(el);
    updateButton(el).click();
    await settle(el);
    expect(el.updateConfirmTarget).toBe('v0.3.0-preview.4');
    const text = dialogText(el).replace(/\s+/g, ' ');
    expect(text).toContain('switches the hub from the nightly channel to the preview channel');
    expect(text).toContain('stays pinned to v0.3.0-preview.4');
    confirmUpdate(el);
    await settle(el);
    expect(runBodies).toEqual([
      {
        params: {
          target_version: 'v0.3.0-preview.4',
          channel: 'preview',
          pin: 'v0.3.0-preview.4',
        },
      },
    ]);
  });

  it('a banner update after a cancelled picker update posts the P1 payload', async () => {
    el = await mount({
      releases: CATALOG,
      updateAvailable: {
        update_available: true,
        update: {
          version: 'nightly-20261023',
          channel: 'nightly',
          detected_at: '2026-10-23T00:00:00Z',
        },
      },
    });
    updateButton(el).click();
    await settle(el);
    root(el)
      .querySelector<HTMLElement>('sl-dialog[label="Update Hub"] sl-button[variant="default"]')!
      .click();
    await settle(el);
    expect(el.updateConfirmTarget).toBeNull();
    findButton(root(el), 'Apply Update')!.click();
    await settle(el);
    confirmUpdate(el);
    await settle(el);
    expect(runBodies).toEqual([{ params: { target_version: 'nightly-20261023', pin: 'latest' } }]);
  });

  it('disables Update while an update run is in progress', async () => {
    el = await mount({
      releases: CATALOG,
      operations: () => [
        op('update-binary', 'Update Binary', {
          id: 'run-7',
          operationKey: 'update-binary',
          status: 'running',
          startedAt: '2026-10-23T10:00:00Z',
        }),
      ],
    });
    expect(updateButton(el).hasAttribute('disabled')).toBe(true);
    expect(cardRoot(el).querySelector('.update-blocked')?.textContent).toContain(
      'An update is in progress'
    );
  });

  it("opens the run detail from the card's View run link", async () => {
    el = await mount({
      status: updateStatus({
        lastUpdate: {
          runId: 'run-9',
          from: 'nightly-20261021',
          to: 'nightly-20261022',
          status: 'verified',
          at: '2026-10-22T01:00:00Z',
        },
      }),
    });
    versionSection(el).querySelector<HTMLElement>('a.last-update-run')!.click();
    await settle(el);
    expect(
      fetchMock.mock.calls.some(
        (c) =>
          pathOf(c[0] as string) === '/api/v1/admin/maintenance/operations/update-binary/runs/run-9'
      )
    ).toBe(true);
  });

  it('does not load releases in git mode', async () => {
    el = await mount({
      releases: CATALOG,
      status: updateStatus({ mode: { effective: 'git', repoConfigured: true } }),
    });
    expect(cardRoot(el).querySelector('.release-panel')).toBeNull();
    expect(
      fetchMock.mock.calls.some(
        (c) => pathOf(c[0] as string) === '/api/v1/admin/maintenance/releases'
      )
    ).toBe(false);
  });
});
