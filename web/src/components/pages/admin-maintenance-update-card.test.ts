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

// The "Server Version & Updates" card: the channel select, the release
// picker over GET .../releases (contents, badges, disabled states),
// refresh with "as of" and the stale warning, and when Update is enabled.

import {
  NO_SNAPSHOTS_REASON,
  channelSourceLabel,
  rollbackTarget,
  isReleasesResponse,
  pinLabel,
  releaseBadges,
  releaseDisabledReason,
  type CatalogRelease,
  type UpdateRequestDetail,
  type UpdateStatus,
} from './admin-maintenance-update-card.js';
import { formatInstantWithZone } from '../../utils/time.js';

type Json = Record<string, unknown>;

function status(overrides: Partial<UpdateStatus> = {}): UpdateStatus {
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
    downgrade: { floor: 'nightly-20261009' },
    pendingUpdate: null,
    lastUpdate: null,
    ...overrides,
  };
}

const NIGHTLY_RELEASES: Json[] = [
  {
    tag: 'nightly-20261023',
    publishedAt: '2026-10-23T04:00:00Z',
    installable: true,
    class: 'upgrade',
    local: false,
    notesUrl: 'https://github.com/GoogleCloudPlatform/scion/releases/tag/nightly-20261023',
  },
  {
    tag: 'nightly-20261022',
    publishedAt: '2026-10-22T04:00:00Z',
    installable: true,
    class: 'same',
    local: true,
  },
  {
    tag: 'nightly-20261021',
    publishedAt: '2026-10-21T04:00:00Z',
    installable: true,
    class: 'downgrade',
    local: true,
  },
  {
    tag: 'nightly-20261020',
    publishedAt: '2026-10-20T04:00:00Z',
    installable: false,
    reason: 'no checksum for scion-linux-amd64',
  },
  {
    tag: 'nightly-20261008',
    publishedAt: '2026-10-08T04:00:00Z',
    installable: true,
    class: 'below-floor',
  },
];

function catalog(overrides: Json = {}): Json {
  return {
    repo: 'GoogleCloudPlatform/scion',
    fetchedAt: '2026-10-23T10:00:00Z',
    stale: false,
    channels: {
      nightly: { latest: 'nightly-20261023', releases: NIGHTLY_RELEASES },
      preview: {
        latest: 'v0.3.0-preview.4',
        releases: [
          {
            tag: 'v0.3.0-preview.4',
            publishedAt: '2026-10-20T00:00:00Z',
            installable: true,
            class: 'switch-newer',
          },
          {
            tag: 'v0.3.0-preview.1',
            publishedAt: '2026-10-01T00:00:00Z',
            installable: true,
            class: 'switch-older',
          },
          {
            tag: 'v0.3.0-preview.3',
            publishedAt: '2026-10-10T00:00:00Z',
            installable: false,
            reason: 'no SHA256SUMS',
          },
        ],
      },
      stable: { latest: null, releases: [] },
    },
    ...overrides,
  };
}

function json(body: unknown, statusCode = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status: statusCode,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

type Card = HTMLElement & {
  updateComplete: Promise<unknown>;
  status: UpdateStatus | null;
  statusError: string | null;
  busy: boolean;
};

beforeAll(async () => {
  await import('./admin-maintenance-update-card.js');
}, 60_000);

let el: Card | null = null;
let fetchMock: ReturnType<typeof vi.fn>;
let releaseRequests: string[] = [];

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

/** Each GET releases answers the next body (the last one repeats). */
async function mount(
  bodies: Array<Json | number | 'down' | Promise<Json>> = [catalog()],
  st: UpdateStatus | null = status()
): Promise<Card> {
  releaseRequests = [];
  let n = 0;
  fetchMock = vi.fn((input: string | URL | Request) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    releaseRequests.push(url);
    const body = bodies[Math.min(n++, bodies.length - 1)];
    if (body === 'down') return Promise.reject(new TypeError('Failed to fetch'));
    if (body instanceof Promise) return body.then((b) => json(b));
    if (typeof body === 'number') {
      return json({ error: { code: 'internal_error', message: 'catalog unavailable' } }, body);
    }
    return json(body);
  });
  vi.stubGlobal('fetch', fetchMock);
  const e = document.createElement('scion-admin-maintenance-update-card') as unknown as Card;
  e.status = st;
  document.body.appendChild(e);
  await settle(e);
  return e;
}

function root(e: Card): ShadowRoot {
  return e.shadowRoot!;
}

type Select = HTMLElement & { value: string };
type Option = HTMLElement;

function select(e: Card, cls: string): Select {
  return root(e).querySelector<Select>(`sl-select.${cls}`)!;
}

function options(e: Card, cls: string): Option[] {
  return Array.from(select(e, cls).querySelectorAll<Option>('sl-option'));
}

function option(e: Card, value: string): Option {
  const o = options(e, 'release-select').find((x) => x.getAttribute('value') === value);
  expect(o).toBeDefined();
  return o!;
}

/**
 * The label sl-select shows for a picked option. Mirrors Shoelace 2.x
 * SlOption.getTextLabel(): text nodes plus the text of child elements
 * without a slot attribute (Shoelace is not registered under vitest).
 */
function selectedLabel(o: Option): string {
  let label = '';
  for (const node of Array.from(o.childNodes)) {
    if (node.nodeType === Node.ELEMENT_NODE && !(node as Element).hasAttribute('slot')) {
      label += node.textContent ?? '';
    }
    if (node.nodeType === Node.TEXT_NODE) label += node.textContent ?? '';
  }
  return label.trim();
}

function badges(o: Option): string[] {
  return Array.from(o.querySelectorAll('sl-badge')).map((b) => b.textContent?.trim() ?? '');
}

function tooltip(o: Option, label: string): string | null {
  const badge = Array.from(o.querySelectorAll('sl-badge')).find(
    (b) => b.textContent?.trim() === label
  );
  return badge?.closest('sl-tooltip')?.getAttribute('content') ?? null;
}

async function pick(e: Card, cls: string, value: string): Promise<void> {
  const sel = select(e, cls);
  sel.value = value;
  sel.dispatchEvent(new Event('sl-change'));
  await settle(e);
}

function updateButton(e: Card): HTMLElement {
  return root(e).querySelector<HTMLElement>('sl-button.update-release')!;
}

function blockedText(e: Card): string {
  return root(e).querySelector('.update-blocked')?.textContent?.trim() ?? '';
}

describe('releaseDisabledReason', () => {
  const r = (over: Partial<CatalogRelease>): CatalogRelease => ({
    tag: 'nightly-20261023',
    installable: true,
    ...over,
  });

  it.each(['same', 'upgrade', 'switch-newer', 'downgrade', 'switch-older'])(
    'allows class %s',
    (cls) => {
      expect(releaseDisabledReason(r({ class: cls }))).toBeNull();
      expect(releaseDisabledReason(r({ class: cls }), 'f-1', true)).toBeNull();
    }
  );

  it.each(['downgrade', 'switch-older'])(
    'disables older class %s when the hub cannot take snapshots',
    (cls) => {
      expect(releaseDisabledReason(r({ class: cls }), 'f-1', false)).toBe(NO_SNAPSHOTS_REASON);
    }
  );

  it.each(['same', 'upgrade', 'switch-newer'])(
    'still allows class %s when the hub cannot take snapshots',
    (cls) => {
      expect(releaseDisabledReason(r({ class: cls }), 'f-1', false)).toBeNull();
    }
  );

  it('keeps below-floor and not-installable disabled whatever snapshotsSupported says', () => {
    expect(releaseDisabledReason(r({ class: 'below-floor' }), 'f-1', true)).toContain('floor');
    expect(releaseDisabledReason(r({ installable: false, class: 'downgrade' }), 'f-1', true)).toBe(
      'Not installable: the release has no verifiable binary'
    );
  });

  it('disables below-floor with the floor', () => {
    expect(releaseDisabledReason(r({ class: 'below-floor' }), 'nightly-20261009')).toBe(
      "Below this hub's downgrade floor (nightly-20261009)"
    );
  });

  it('disables a non-installable release with its reason, whatever the class', () => {
    expect(releaseDisabledReason(r({ installable: false, reason: 'no SHA256SUMS' }))).toBe(
      'Not installable: no SHA256SUMS'
    );
    expect(
      releaseDisabledReason(r({ installable: false, class: 'upgrade', reason: 'bad entry' }))
    ).toBe('Not installable: bad entry');
  });

  it('disables a missing or unknown class', () => {
    expect(releaseDisabledReason(r({}))).not.toBeNull();
    expect(releaseDisabledReason(r({ class: 'sideways' }))).not.toBeNull();
  });
});

describe('releaseBadges', () => {
  it.each([
    [{ class: 'same', local: true }, ['Current', 'Local']],
    [{ class: 'upgrade' }, []],
    [{ class: 'switch-newer' }, []],
    [{ class: 'downgrade', local: true }, ['Local', 'Older']],
    [{ class: 'switch-older' }, ['Older']],
    [{ class: 'below-floor' }, ['Below floor']],
    [{ installable: false, reason: 'no SHA256SUMS' }, ['Not installable']],
  ] as Array<[Partial<CatalogRelease>, string[]]>)('%j -> %j', (over, labels) => {
    const r: CatalogRelease = { tag: 't', installable: true, ...over };
    expect(releaseBadges(r).map((b) => b.label)).toEqual(labels);
  });

  it('explains Not installable and Below floor in a tooltip', () => {
    const ni = releaseBadges({ tag: 't', installable: false, reason: 'no SHA256SUMS' });
    expect(ni[0].tooltip).toBe('no SHA256SUMS');
    const bf = releaseBadges({ tag: 't', installable: true, class: 'below-floor' }, 'f-1');
    expect(bf[0].tooltip).toContain('f-1');
  });

  it('explains Older: typed confirmation, or no snapshots', () => {
    const older: CatalogRelease = { tag: 't', installable: true, class: 'downgrade' };
    expect(releaseBadges(older)[0].tooltip).toContain('typing its tag');
    expect(releaseBadges(older, 'f-1', false)[0].tooltip).toBe(NO_SNAPSHOTS_REASON);
  });
});

describe('isReleasesResponse', () => {
  it('accepts the contract shape and rejects others', () => {
    expect(isReleasesResponse(catalog())).toBe(true);
    expect(isReleasesResponse({})).toBe(false);
    expect(isReleasesResponse(null)).toBe(false);
    expect(isReleasesResponse({ channels: { nightly: { latest: null } } })).toBe(false);
    expect(isReleasesResponse({ channels: { nightly: null } })).toBe(false);
  });
});

describe('Release picker', () => {
  it('lists Latest (<tag>) first, then every release with its publish date', async () => {
    el = await mount();
    const opts = options(el, 'release-select');
    expect(opts[0].getAttribute('value')).toBe('latest');
    expect(opts[0].textContent?.trim()).toBe('Latest (nightly-20261023)');
    expect(opts.slice(1).map((o) => o.getAttribute('value'))).toEqual(
      NIGHTLY_RELEASES.map((r) => r.tag)
    );
    const date = formatInstantWithZone('2026-10-23T04:00:00Z', 'date');
    expect(option(el, 'nightly-20261023').querySelector('.release-date')?.textContent).toBe(date);
    expect(select(el, 'release-select').value).toBe('latest');
  });

  it('keeps the date and badges out of the label shown for a picked release', async () => {
    el = await mount();
    const o = option(el, 'nightly-20261022');
    expect(o.querySelector('.release-date')?.getAttribute('slot')).toBe('suffix');
    expect(selectedLabel(o)).toBe('nightly-20261022');
    expect(selectedLabel(option(el, 'nightly-20261020'))).toBe('nightly-20261020');
    expect(selectedLabel(option(el, 'latest'))).toBe('Latest (nightly-20261023)');
  });

  it('renders no date for a release without publishedAt', async () => {
    el = await mount([
      catalog({
        channels: {
          nightly: {
            latest: 'nightly-20261024',
            releases: [
              { tag: 'nightly-20261024', installable: true, class: 'upgrade' },
              {
                tag: 'nightly-20261023',
                publishedAt: 'not a date',
                installable: true,
                class: 'upgrade',
              },
            ],
          },
        },
      }),
    ]);
    expect(option(el, 'nightly-20261024').querySelector('.release-date')).toBeNull();
    expect(option(el, 'nightly-20261023').querySelector('.release-date')).toBeNull();
    expect(option(el, 'nightly-20261024').textContent).not.toContain('Invalid');
  });

  it('shows the badges for each entry', async () => {
    el = await mount();
    expect(badges(option(el, 'nightly-20261023'))).toEqual([]);
    expect(badges(option(el, 'nightly-20261022'))).toEqual(['Current', 'Local']);
    expect(badges(option(el, 'nightly-20261021'))).toEqual(['Local', 'Older']);
    expect(badges(option(el, 'nightly-20261020'))).toEqual(['Not installable']);
    expect(badges(option(el, 'nightly-20261008'))).toEqual(['Below floor']);
  });

  it('disables not-installable and below-floor entries with the reason, not older ones', async () => {
    el = await mount();
    const ni = option(el, 'nightly-20261020');
    expect(ni.hasAttribute('disabled')).toBe(true);
    expect(tooltip(ni, 'Not installable')).toBe('no checksum for scion-linux-amd64');
    expect(ni.getAttribute('title')).toBe('Not installable: no checksum for scion-linux-amd64');

    const bf = option(el, 'nightly-20261008');
    expect(bf.hasAttribute('disabled')).toBe(true);
    expect(tooltip(bf, 'Below floor')).toBe("Below this hub's downgrade floor (nightly-20261009)");

    const older = option(el, 'nightly-20261021');
    expect(older.hasAttribute('disabled')).toBe(false);
    expect(tooltip(older, 'Older')).toContain('typing its tag');

    expect(option(el, 'latest').hasAttribute('disabled')).toBe(false);
    expect(option(el, 'nightly-20261023').hasAttribute('disabled')).toBe(false);
    expect(option(el, 'nightly-20261022').hasAttribute('disabled')).toBe(false);
  });

  it('enables switch-older and switch-newer entries in another channel', async () => {
    el = await mount();
    await pick(el, 'channel-select', 'preview');
    expect(option(el, 'latest').textContent?.trim()).toBe('Latest (v0.3.0-preview.4)');
    expect(option(el, 'v0.3.0-preview.4').hasAttribute('disabled')).toBe(false);
    expect(option(el, 'v0.3.0-preview.1').hasAttribute('disabled')).toBe(false);
    expect(badges(option(el, 'v0.3.0-preview.1'))).toEqual(['Older']);
    expect(option(el, 'v0.3.0-preview.3').hasAttribute('disabled')).toBe(true);
    expect(tooltip(option(el, 'v0.3.0-preview.3'), 'Not installable')).toBe('no SHA256SUMS');
  });

  it('disables empty channels with "(no releases)"', async () => {
    el = await mount();
    const chans = options(el, 'channel-select');
    expect(chans.map((o) => o.getAttribute('value'))).toEqual(['nightly', 'preview', 'stable']);
    const stable = chans[2];
    expect(stable.hasAttribute('disabled')).toBe(true);
    expect(stable.textContent?.trim()).toBe('stable (no releases)');
    expect(chans[0].hasAttribute('disabled')).toBe(false);
    expect(chans[1].hasAttribute('disabled')).toBe(false);
  });

  it("starts on the hub's effective channel", async () => {
    el = await mount(
      [catalog()],
      status({ channel: { effective: 'preview', configured: 'nightly', source: 'selection' } })
    );
    expect(select(el, 'channel-select').value).toBe('preview');
    expect(select(el, 'release-select').value).toBe('latest');
  });

  it('falls back to the first channel with releases when the effective one is empty', async () => {
    el = await mount(
      [catalog()],
      status({ channel: { effective: 'stable', configured: 'stable', source: 'config' } })
    );
    expect(select(el, 'channel-select').value).toBe('nightly');
  });

  it('does not preselect Latest when the latest release is older than the running one', async () => {
    el = await mount([
      catalog({
        channels: {
          nightly: {
            latest: 'nightly-20261021',
            releases: [{ tag: 'nightly-20261021', installable: true, class: 'downgrade' }],
          },
        },
      }),
    ]);
    expect(option(el, 'latest').hasAttribute('disabled')).toBe(false);
    expect(select(el, 'release-select').value).toBe('');
    expect(updateButton(el).hasAttribute('disabled')).toBe(true);
    expect(blockedText(el)).toBe('Pick a release');
  });

  it('disables older entries and an older Latest when the hub cannot take snapshots', async () => {
    el = await mount(
      [
        catalog({
          channels: {
            nightly: { latest: 'nightly-20261023', releases: NIGHTLY_RELEASES },
            stable: {
              latest: 'v0.2.0',
              releases: [{ tag: 'v0.2.0', installable: true, class: 'switch-older' }],
            },
          },
        }),
      ],
      status({ downgrade: { floor: 'nightly-20261009', snapshotsSupported: false } })
    );
    const older = option(el, 'nightly-20261021');
    expect(older.hasAttribute('disabled')).toBe(true);
    expect(older.getAttribute('title')).toBe(NO_SNAPSHOTS_REASON);
    expect(tooltip(older, 'Older')).toBe(NO_SNAPSHOTS_REASON);
    // Newer entries are unaffected.
    expect(option(el, 'latest').hasAttribute('disabled')).toBe(false);
    expect(option(el, 'nightly-20261023').hasAttribute('disabled')).toBe(false);

    await pick(el, 'channel-select', 'stable');
    expect(option(el, 'latest').hasAttribute('disabled')).toBe(true);
    expect(option(el, 'latest').getAttribute('title')).toBe(NO_SNAPSHOTS_REASON);
    expect(option(el, 'v0.2.0').hasAttribute('disabled')).toBe(true);
    await pick(el, 'release-select', 'v0.2.0');
    expect(updateButton(el).hasAttribute('disabled')).toBe(true);
    expect(blockedText(el)).toBe(NO_SNAPSHOTS_REASON);
  });

  it('a channel with only non-installable releases has no Latest entry', async () => {
    el = await mount([
      catalog({
        channels: {
          nightly: { latest: 'nightly-20261023', releases: NIGHTLY_RELEASES },
          preview: {
            latest: null,
            releases: [{ tag: 'v0.3.0-preview.3', installable: false, reason: 'no SHA256SUMS' }],
          },
        },
      }),
    ]);
    await pick(el, 'channel-select', 'preview');
    const opts = options(el, 'release-select');
    expect(opts.map((o) => o.getAttribute('value'))).toEqual(['v0.3.0-preview.3']);
    expect(updateButton(el).hasAttribute('disabled')).toBe(true);
  });

  it('shows the release-notes link only for an https notesUrl', async () => {
    el = await mount([
      catalog({
        channels: {
          nightly: {
            latest: 'nightly-20261023',
            releases: [
              { ...NIGHTLY_RELEASES[0] },
              { ...NIGHTLY_RELEASES[1], notesUrl: 'javascript:alert(1)' },
            ],
          },
        },
      }),
    ]);
    expect(root(el).querySelector('a.release-notes-link')?.getAttribute('href')).toBe(
      NIGHTLY_RELEASES[0].notesUrl
    );
    await pick(el, 'release-select', 'nightly-20261022');
    expect(root(el).querySelector('a.release-notes-link')).toBeNull();
    expect(root(el).querySelector('.release-notes-text')?.textContent).toBe('javascript:alert(1)');
  });
});

describe('Update button', () => {
  function captureRequests(e: Card): UpdateRequestDetail[] {
    const got: UpdateRequestDetail[] = [];
    e.addEventListener('update-request', (ev) =>
      got.push((ev as CustomEvent<UpdateRequestDetail>).detail)
    );
    return got;
  }

  it('requests the latest tag with pin latest', async () => {
    el = await mount();
    const got = captureRequests(el);
    expect(updateButton(el).hasAttribute('disabled')).toBe(false);
    updateButton(el).click();
    expect(got).toEqual([
      { target: 'nightly-20261023', channel: 'nightly', pin: 'latest', releaseClass: 'upgrade' },
    ]);
  });

  it('requests a picked switch-newer tag pinned to that tag', async () => {
    el = await mount();
    const got = captureRequests(el);
    await pick(el, 'channel-select', 'preview');
    await pick(el, 'release-select', 'v0.3.0-preview.4');
    updateButton(el).click();
    expect(got).toEqual([
      {
        target: 'v0.3.0-preview.4',
        channel: 'preview',
        pin: 'v0.3.0-preview.4',
        releaseClass: 'switch-newer',
      },
    ]);
  });

  it('requests a picked downgrade with its class and says it asks for a typed confirmation', async () => {
    el = await mount();
    const got = captureRequests(el);
    await pick(el, 'release-select', 'nightly-20261021');
    expect(updateButton(el).hasAttribute('disabled')).toBe(false);
    expect(root(el).querySelector('.older-note')?.textContent).toContain('type the tag');
    updateButton(el).click();
    expect(got).toEqual([
      {
        target: 'nightly-20261021',
        channel: 'nightly',
        pin: 'nightly-20261021',
        releaseClass: 'downgrade',
      },
    ]);
  });

  it('requests an older Latest in another channel with pin latest', async () => {
    el = await mount([
      catalog({
        channels: {
          nightly: { latest: 'nightly-20261023', releases: NIGHTLY_RELEASES },
          stable: {
            latest: 'v0.2.0',
            releases: [{ tag: 'v0.2.0', installable: true, class: 'switch-older' }],
          },
        },
      }),
    ]);
    const got = captureRequests(el);
    await pick(el, 'channel-select', 'stable');
    expect(select(el, 'release-select').value).toBe('');
    await pick(el, 'release-select', 'latest');
    updateButton(el).click();
    expect(got).toEqual([
      { target: 'v0.2.0', channel: 'stable', pin: 'latest', releaseClass: 'switch-older' },
    ]);
  });

  it('keeps below-floor entries disabled with the floor', async () => {
    el = await mount();
    const got = captureRequests(el);
    await pick(el, 'release-select', 'nightly-20261008');
    expect(updateButton(el).hasAttribute('disabled')).toBe(true);
    expect(blockedText(el)).toBe("Below this hub's downgrade floor (nightly-20261009)");
    updateButton(el).click();
    expect(got).toEqual([]);
  });

  it('is disabled for the running release (class same)', async () => {
    el = await mount();
    const got = captureRequests(el);
    await pick(el, 'release-select', 'nightly-20261022');
    expect(updateButton(el).hasAttribute('disabled')).toBe(true);
    expect(blockedText(el)).toBe('The hub is already running nightly-20261022');
    updateButton(el).click();
    expect(got).toEqual([]);
  });

  it('is disabled when the installer is unsupported', async () => {
    el = await mount(
      [catalog()],
      status({
        installer: { strategy: 'unsupported', supported: false, reason: 'not a symlink' },
      })
    );
    expect(updateButton(el).hasAttribute('disabled')).toBe(true);
    expect(blockedText(el)).toBe('This hub cannot install updates itself');
    expect(root(el).querySelector('sl-alert.installer-unsupported')).not.toBeNull();
  });

  it('is disabled while busy', async () => {
    el = await mount();
    el.busy = true;
    await settle(el);
    expect(updateButton(el).hasAttribute('disabled')).toBe(true);
    expect(blockedText(el)).toBe('An update is in progress');
  });
});

describe('Refresh and staleness', () => {
  it('shows "as of" and no stale warning for a fresh list', async () => {
    el = await mount();
    expect(releaseRequests).toEqual(['/api/v1/admin/maintenance/releases']);
    const asOf = root(el).querySelector('.catalog-as-of');
    expect(asOf?.textContent).toMatch(/^as of /);
    expect(asOf?.getAttribute('title')).toBe(formatInstantWithZone('2026-10-23T10:00:00Z'));
    expect(root(el).querySelector('sl-alert.catalog-stale')).toBeNull();
  });

  it('shows the stale warning when the hub serves a stale copy', async () => {
    el = await mount([catalog({ stale: true })]);
    const alert = root(el).querySelector('sl-alert.catalog-stale');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('could not reach GitHub');
  });

  it('refresh asks the hub with refresh=1 and shows the new list', async () => {
    el = await mount([
      catalog(),
      catalog({
        fetchedAt: '2026-10-23T11:00:00Z',
        channels: {
          nightly: {
            latest: 'nightly-20261024',
            releases: [
              { tag: 'nightly-20261024', installable: true, class: 'upgrade' },
              ...NIGHTLY_RELEASES,
            ],
          },
        },
      }),
    ]);
    root(el).querySelector<HTMLElement>('sl-icon-button.refresh-releases')!.click();
    await settle(el);
    expect(releaseRequests).toEqual([
      '/api/v1/admin/maintenance/releases',
      '/api/v1/admin/maintenance/releases?refresh=1',
    ]);
    expect(option(el, 'latest').textContent?.trim()).toBe('Latest (nightly-20261024)');
    // nightly-20261024 has no publishedAt.
    expect(option(el, 'nightly-20261024').querySelector('.release-date')).toBeNull();
  });

  it('keeps the previous list and shows the error when a refresh fails', async () => {
    el = await mount([catalog(), 502]);
    root(el).querySelector<HTMLElement>('sl-icon-button.refresh-releases')!.click();
    await settle(el);
    expect(root(el).querySelector('sl-alert.releases-error')?.textContent).toContain(
      'catalog unavailable'
    );
    expect(option(el, 'latest').textContent?.trim()).toBe('Latest (nightly-20261023)');
  });

  it('shows an error when the first load fails', async () => {
    el = await mount(['down']);
    expect(root(el).querySelector('sl-alert.releases-error')).not.toBeNull();
    expect(root(el).querySelector('sl-select.release-select')).toBeNull();
  });

  it('does not start a second load while a retry is in flight', async () => {
    let release: (b: Json) => void = () => {};
    const pending = new Promise<Json>((resolve) => {
      release = resolve;
    });
    el = await mount(['down', pending, catalog()]);
    const retry = root(el).querySelector<HTMLElement>('sl-button.retry-releases')!;
    retry.click();
    await settle(el);
    expect(releaseRequests).toHaveLength(2);
    const busy = root(el).querySelector<HTMLElement>('sl-button.retry-releases')!;
    expect(busy.hasAttribute('disabled')).toBe(true);
    expect(busy.hasAttribute('loading')).toBe(true);
    busy.click();
    await settle(el);
    expect(releaseRequests).toHaveLength(2);
    release(catalog());
    await settle(el);
    expect(releaseRequests).toHaveLength(2);
    expect(option(el, 'latest').textContent?.trim()).toBe('Latest (nightly-20261023)');
  });

  it('retries a failed first load from the error state', async () => {
    el = await mount(['down', catalog()]);
    const retry = root(el).querySelector<HTMLElement>('sl-button.retry-releases');
    expect(retry).not.toBeNull();
    retry!.click();
    await settle(el);
    expect(releaseRequests).toEqual([
      '/api/v1/admin/maintenance/releases',
      '/api/v1/admin/maintenance/releases',
    ]);
    expect(root(el).querySelector('sl-alert.releases-error')).toBeNull();
    expect(root(el).querySelector('sl-button.retry-releases')).toBeNull();
    expect(option(el, 'latest').textContent?.trim()).toBe('Latest (nightly-20261023)');
  });

  it('rejects a malformed body', async () => {
    el = await mount([{ channels: 'nope' }]);
    expect(root(el).querySelector('sl-alert.releases-error')?.textContent).toContain(
      'Unexpected releases response'
    );
  });

  it('reloads the list and resets the picks when the running version changes', async () => {
    el = await mount([
      catalog(),
      catalog({
        channels: {
          preview: {
            latest: 'v0.3.0-preview.4',
            releases: [{ tag: 'v0.3.0-preview.4', installable: true, class: 'same' }],
          },
        },
      }),
    ]);
    await pick(el, 'channel-select', 'preview');
    await pick(el, 'release-select', 'v0.3.0-preview.4');
    el.status = status({
      current: {
        version: 'v0.3.0-preview.4',
        channel: 'preview',
        commit: 'def',
        isGitBuild: false,
      },
      channel: { effective: 'preview', configured: 'nightly', source: 'selection' },
      pin: 'v0.3.0-preview.4',
    });
    await settle(el);
    expect(releaseRequests).toHaveLength(2);
    expect(select(el, 'channel-select').value).toBe('preview');
    expect(select(el, 'release-select').value).toBe('latest');
    expect(blockedText(el)).toBe('The hub is already running v0.3.0-preview.4');
  });

  it('does not reload when the status changes but the version does not', async () => {
    el = await mount();
    el.status = status({ updatePolicy: 'auto' });
    await settle(el);
    expect(releaseRequests).toHaveLength(1);
  });
});

// Release-manager requirement: the card says where the effective channel
// came from (update-status channel.source) and whether the hub is pinned.
describe('Channel source and pin', () => {
  it.each([
    ['selection', "selection (chosen by an administrator's update)"],
    ['config', 'config (from the server settings)'],
    ['detected', 'detected (detected from the running version)'],
    ['other', 'other'],
    [undefined, 'unknown'],
    ['', 'unknown'],
  ])('channelSourceLabel(%s)', (source, label) => {
    expect(channelSourceLabel(source)).toBe(label);
  });

  it.each([
    ['latest', 'latest (follows the newest release)'],
    ['', 'latest (follows the newest release)'],
    [undefined, 'latest (follows the newest release)'],
    ['v0.3.0-preview.4', 'pinned to v0.3.0-preview.4 (automatic updates do not move it)'],
  ])('pinLabel(%s)', (pin, label) => {
    expect(pinLabel(pin)).toBe(label);
  });

  function text(e: Card, selector: string): string {
    return root(e).querySelector(selector)?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
  }

  it('shows a configured channel that follows latest', async () => {
    el = await mount();
    expect(text(el, '.channel-source')).toBe('config (from the server settings)');
    expect(text(el, '.pin-state')).toBe('latest (follows the newest release)');
    expect(text(el, '.channel-source-note')).toBe("The hub's channel is nightly (from: config).");
  });

  it('shows a selected channel pinned to a tag', async () => {
    el = await mount(
      [catalog()],
      status({
        channel: { effective: 'preview', configured: 'nightly', source: 'selection' },
        pin: 'v0.3.0-preview.4',
      })
    );
    expect(text(el, '.channel-source')).toBe("selection (chosen by an administrator's update)");
    expect(text(el, '.pin-state')).toBe(
      'pinned to v0.3.0-preview.4 (automatic updates do not move it)'
    );
    expect(text(el, '.channel-source-note')).toBe(
      "The hub's channel is preview (from: selection), pinned to v0.3.0-preview.4."
    );
  });

  it('shows a detected channel', async () => {
    el = await mount(
      [catalog()],
      status({ channel: { effective: 'nightly', source: 'detected' } })
    );
    expect(text(el, '.channel-source')).toBe('detected (detected from the running version)');
    expect(text(el, '.channel-source-note')).toBe("The hub's channel is nightly (from: detected).");
  });

  it('shows the channel source but no pin or release panel in git mode', async () => {
    el = await mount([catalog()], status({ mode: { effective: 'git', repoConfigured: true } }));
    expect(text(el, '.channel-source')).toBe('config (from the server settings)');
    expect(root(el).querySelector('.pin-state')).toBeNull();
    expect(root(el).querySelector('.channel-source-note')).toBeNull();
  });
});

describe('Last update', () => {
  function lastUpdateText(e: Card): string {
    return (root(e).querySelector('.last-update')?.textContent ?? '').replace(/\s+/g, ' ').trim();
  }

  it('shows a rolled-back update as a rollback to the previous release', async () => {
    el = await mount(
      [catalog()],
      status({
        lastUpdate: {
          runId: 'run-3',
          from: 'nightly-20261021',
          to: 'nightly-20261022',
          status: 'rolled_back',
          at: '2026-10-22T01:00:00Z',
        },
      })
    );
    expect(root(el).querySelector('.last-update-rollback')?.textContent?.replace(/\s+/g, ' ')).toBe(
      'Rolled back to nightly-20261021 after nightly-20261022 failed to start'
    );
    const badge = root(el).querySelector('.last-update .status-badge');
    expect(badge?.textContent?.trim()).toBe('rolled back');
    expect(badge?.classList.contains('failed')).toBe(true);
    expect(lastUpdateText(el)).not.toContain('→');
  });

  it('names the restored release when the hub reports it', async () => {
    el = await mount(
      [catalog()],
      status({
        lastUpdate: {
          runId: 'run-4',
          from: 'nightly-20261021',
          to: 'nightly-20261022',
          status: 'rolled_back',
          at: '2026-10-22T01:00:00Z',
          restored: 'nightly-20261020',
        },
      })
    );
    expect(root(el).querySelector('.last-update-rollback')?.textContent?.replace(/\s+/g, ' ')).toBe(
      'Rolled back to nightly-20261020 after nightly-20261022 failed to start'
    );
  });

  it('shows a verified update as from → to', async () => {
    el = await mount(
      [catalog()],
      status({
        lastUpdate: {
          runId: 'run-2',
          from: 'nightly-20261021',
          to: 'nightly-20261022',
          status: 'verified',
          at: '2026-10-22T01:00:00Z',
        },
      })
    );
    expect(root(el).querySelector('.last-update-rollback')).toBeNull();
    expect(lastUpdateText(el)).toContain('nightly-20261021 → nightly-20261022');
    expect(root(el).querySelector('.last-update .status-badge.completed')).not.toBeNull();
  });
});

describe('rollbackTarget', () => {
  const base = { runId: 'r', from: 'v1', to: 'v2', status: 'rolled_back', at: '' };

  it('prefers restored, then from, then a placeholder', () => {
    expect(rollbackTarget({ ...base, restored: 'v0' })).toBe('v0');
    expect(rollbackTarget(base)).toBe('v1');
    expect(rollbackTarget({ ...base, restored: '' })).toBe('v1');
    expect(rollbackTarget({ ...base, from: '' })).toBe('the previous release');
  });
});
