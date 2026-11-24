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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { AutoUpdateStatus, UpdateStatus } from './admin-maintenance-update-card.js';

type Json = Record<string, unknown>;

function autoUpdate(overrides: Partial<AutoUpdateStatus> = {}): AutoUpdateStatus {
  return {
    enabled: false,
    source: 'default',
    locked: false,
    lockedReason: '',
    active: false,
    inactiveReason: '',
    windowStartHourUtc: 4,
    ...overrides,
  };
}

function makeStatus(overrides: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    current: {
      version: 'nightly-20261022',
      channel: 'nightly',
      commit: 'abc123def4567890',
      isGitBuild: false,
    },
    mode: { effective: 'release', repoConfigured: false },
    channel: { effective: 'nightly', configured: 'nightly', source: 'config' },
    pin: 'latest',
    installer: { strategy: 'symlink', supported: true },
    updatePolicy: 'notify',
    autoUpdate: autoUpdate(),
    downgrade: { floor: 'nightly-20261009' },
    pendingUpdate: null,
    lastUpdate: null,
    ...overrides,
  };
}

const CATALOG: Json = {
  repo: 'GoogleCloudPlatform/scion',
  fetchedAt: '2026-10-23T10:00:00Z',
  stale: false,
  channels: {
    nightly: {
      latest: 'nightly-20261023',
      releases: [
        {
          tag: 'nightly-20261023',
          publishedAt: '2026-10-23T04:00:00Z',
          installable: true,
          class: 'upgrade',
        },
        {
          tag: 'nightly-20261022',
          publishedAt: '2026-10-22T04:00:00Z',
          installable: true,
          class: 'same',
        },
      ],
    },
    preview: {
      latest: 'v0.3.0-preview.4',
      releases: [
        {
          tag: 'v0.3.0-preview.4',
          publishedAt: '2026-10-20T00:00:00Z',
          installable: true,
          class: 'switch-newer',
        },
      ],
    },
    stable: { latest: null, releases: [] },
  },
};

function json(body: unknown, statusCode = 200): Response {
  return new Response(JSON.stringify(body), {
    status: statusCode,
    headers: { 'Content-Type': 'application/json' },
  });
}

type Card = HTMLElement & {
  updateComplete: Promise<unknown>;
  status: UpdateStatus | null;
};

type Checkbox = HTMLElement & { checked: boolean; disabled: boolean };
type Select = HTMLElement & { value: string };

beforeAll(async () => {
  await import('./admin-maintenance-update-card.js');
}, 60_000);

let el: Card | null = null;

afterEach(() => {
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
});

async function settle(e: Card): Promise<void> {
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 30));
  await e.updateComplete;
}

async function mount(
  st: UpdateStatus,
  putHandler?: (init?: RequestInit) => Promise<Response>
): Promise<{ card: Card; fetchMock: ReturnType<typeof vi.fn> }> {
  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (url.includes('/api/v1/admin/maintenance/auto-update') && init?.method === 'PUT') {
      if (putHandler) return putHandler(init);
      const req = JSON.parse(typeof init.body === 'string' ? init.body : '') as {
        enabled: boolean;
      };
      return Promise.resolve(
        json(
          autoUpdate({
            enabled: req.enabled,
            source: 'setting',
            active: req.enabled,
          })
        )
      );
    }
    return Promise.resolve(json(CATALOG));
  });
  vi.stubGlobal('fetch', fetchMock);
  const card = document.createElement('scion-admin-maintenance-update-card') as unknown as Card;
  card.status = st;
  document.body.appendChild(card);
  el = card;
  await settle(card);
  return { card, fetchMock };
}

describe('scion-admin-maintenance-update-card auto-update checkbox', () => {
  it('is enabled for committed release + latest with matching picker', async () => {
    const { card } = await mount(makeStatus());
    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox');
    expect(cb).not.toBeNull();
    expect(cb!.hasAttribute('disabled')).toBe(false);
    expect(cb!.checked).toBe(false);
    expect(card.shadowRoot!.querySelector('.auto-update-note')).toBeNull();
    expect(card.shadowRoot!.querySelector('.auto-update-hint')?.textContent).toContain(
      'Installs the newest nightly release once a day'
    );
  });

  it('disables with note when picker channel differs from committed channel', async () => {
    const { card } = await mount(makeStatus());
    const chSelect = card.shadowRoot!.querySelector<Select>('sl-select.channel-select')!;
    chSelect.value = 'preview';
    chSelect.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await settle(card);

    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;
    expect(cb.hasAttribute('disabled')).toBe(true);
    const note = card.shadowRoot!.querySelector('.auto-update-note')?.textContent?.trim();
    expect(note).toContain("Applies to the hub's current channel (nightly, latest)");
    expect(note).toContain('Update to change channel first');
  });

  it('disables with note when picker release is a concrete tag instead of latest', async () => {
    const { card } = await mount(makeStatus());
    const relSelect = card.shadowRoot!.querySelector<Select>('sl-select.release-select')!;
    relSelect.value = 'nightly-20261022';
    relSelect.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await settle(card);

    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;
    expect(cb.hasAttribute('disabled')).toBe(true);
    expect(card.shadowRoot!.querySelector('.auto-update-note')?.textContent).toContain(
      "Applies to the hub's current channel (nightly, latest)"
    );
  });

  it('disables with "Paused" note when pinned while showing stored checked value', async () => {
    const { card } = await mount(
      makeStatus({
        pin: 'nightly-20261022',
        autoUpdate: autoUpdate({
          enabled: true,
          source: 'setting',
          active: false,
          inactiveReason: 'pinned',
        }),
      })
    );
    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;
    expect(cb.hasAttribute('disabled')).toBe(true);
    expect(cb.checked).toBe(true);
    expect(card.shadowRoot!.querySelector('.auto-update-note')?.textContent).toContain(
      'Paused while the hub is pinned to nightly-20261022.'
    );
  });

  it('disables with note when committed mode is git and user views release panel', async () => {
    const { card } = await mount(
      makeStatus({
        mode: { effective: 'git', repoConfigured: true, repoBranch: 'main' },
        autoUpdate: autoUpdate({
          enabled: true,
          source: 'setting',
          active: false,
          inactiveReason: 'git_mode',
        }),
      })
    );
    // Switch mode toggle to release panel.
    const modeSelect = card.shadowRoot!.querySelector<Select>('sl-radio-group.mode-select')!;
    modeSelect.value = 'release';
    modeSelect.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await settle(card);

    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;
    expect(cb).not.toBeNull();
    expect(cb.hasAttribute('disabled')).toBe(true);
    expect(card.shadowRoot!.querySelector('.auto-update-note')?.textContent).toContain('Git mode');
  });

  it('is hidden in git mode without the release panel', async () => {
    const { card } = await mount(
      makeStatus({
        mode: { effective: 'git', repoConfigured: false },
        autoUpdate: autoUpdate({ inactiveReason: 'git_mode' }),
      })
    );
    expect(card.shadowRoot!.querySelector('sl-checkbox.auto-update-checkbox')).toBeNull();
  });

  it('disables with installer reason when installer is unsupported', async () => {
    const { card } = await mount(
      makeStatus({
        installer: {
          strategy: 'unsupported',
          supported: false,
          reason: 'Running on darwin; binary updates require Linux.',
        },
        autoUpdate: autoUpdate({ inactiveReason: 'installer_unsupported' }),
      })
    );
    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;
    expect(cb.hasAttribute('disabled')).toBe(true);
    expect(card.shadowRoot!.querySelector('.auto-update-note')?.textContent).toContain(
      'Running on darwin; binary updates require Linux.'
    );
  });

  it('disables with locked reason when config-locked', async () => {
    const { card } = await mount(
      makeStatus({
        autoUpdate: autoUpdate({
          enabled: false,
          source: 'config-locked',
          locked: true,
          lockedReason: 'Automatic updates are disabled by server configuration.',
          inactiveReason: 'config_locked',
        }),
      })
    );
    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;
    expect(cb.hasAttribute('disabled')).toBe(true);
    expect(card.shadowRoot!.querySelector('.auto-update-note')?.textContent).toContain(
      'Automatic updates are disabled by server configuration.'
    );
  });

  it('sends PUT immediately on toggle, disables in flight, and keeps updated value', async () => {
    let resolvePut!: (res: Response) => void;
    const putPromise = new Promise<Response>((r) => {
      resolvePut = r;
    });
    const { card, fetchMock } = await mount(makeStatus(), () => putPromise);
    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;

    cb.checked = true;
    cb.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await card.updateComplete;

    // Disabled while in flight.
    expect(cb.hasAttribute('disabled')).toBe(true);
    const putCall = fetchMock.mock.calls.find(
      (c) =>
        String(c[0]).includes('/api/v1/admin/maintenance/auto-update') &&
        (c[1] as RequestInit | undefined)?.method === 'PUT'
    );
    expect(putCall).toBeDefined();
    expect(JSON.parse(typeof putCall![1]?.body === 'string' ? putCall![1].body : '')).toEqual({
      enabled: true,
    });

    resolvePut(
      json(
        autoUpdate({
          enabled: true,
          source: 'setting',
          active: true,
        })
      )
    );
    await settle(card);

    expect(cb.hasAttribute('disabled')).toBe(false);
    expect(cb.checked).toBe(true);
    expect(card.shadowRoot!.querySelector('.auto-update-error')).toBeNull();
  });

  it('reverts checkbox and shows sl-alert on PUT error, reloading status on 409', async () => {
    const { card } = await mount(makeStatus(), () =>
      Promise.resolve(
        json(
          { error: { code: 'auto_update_locked', message: 'Disabled by server configuration.' } },
          409
        )
      )
    );
    const reloadSpy = vi.fn();
    card.addEventListener('reload-status', reloadSpy);

    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;
    cb.checked = true;
    cb.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await settle(card);

    expect(cb.hasAttribute('disabled')).toBe(false);
    expect(cb.checked).toBe(false);
    const alert = card.shadowRoot!.querySelector('sl-alert.auto-update-error');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('Disabled by server configuration.');
    expect(reloadSpy).toHaveBeenCalledTimes(1);
  });

  it('keeps the error alert visible across the status reload that follows a 409 conflict', async () => {
    const { card } = await mount(makeStatus(), () =>
      Promise.resolve(
        json(
          {
            error: {
              code: 'revision_conflict',
              message:
                'The auto-update setting was modified concurrently. Please refresh and retry.',
            },
          },
          409
        )
      )
    );
    const reloadSpy = vi.fn();
    card.addEventListener('reload-status', reloadSpy);

    const cb = card.shadowRoot!.querySelector<Checkbox>('sl-checkbox.auto-update-checkbox')!;
    cb.checked = true;
    cb.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await settle(card);

    expect(reloadSpy).toHaveBeenCalledTimes(1);

    // Simulate parent reloading update-status with a fresh status object.
    card.status = makeStatus({
      autoUpdate: autoUpdate({ enabled: false, source: 'setting', active: false }),
    });
    await settle(card);

    const alert = card.shadowRoot!.querySelector('sl-alert.auto-update-error');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('modified concurrently');
  });
});
