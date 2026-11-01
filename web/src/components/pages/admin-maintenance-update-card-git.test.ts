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

// The update card's "Build from Git" | "Release binary" toggle (shown only
// when a maintenance repo is configured) and its Git panel: the configured
// branch, the upstream commit check and Rebuild.

import {
  lastUpdateOperation,
  type UpdateStatus,
  type ViewRunDetail,
} from './admin-maintenance-update-card.js';

type Json = Record<string, unknown>;

function status(overrides: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    current: {
      version: '0123abcd',
      channel: '',
      commit: '0123abcd4567890123456789abcdef0123456789',
      isGitBuild: true,
    },
    mode: { effective: 'git', repoConfigured: true, repoBranch: 'main' },
    channel: { effective: 'nightly', configured: 'nightly', source: 'config' },
    pin: 'latest',
    installer: { strategy: 'sudo-install', supported: true },
    updatePolicy: 'notify',
    downgrade: { floor: 'nightly-20261009' },
    pendingUpdate: null,
    lastUpdate: null,
    ...overrides,
  };
}

const CATALOG: Json = {
  repo: 'GoogleCloudPlatform/scion',
  fetchedAt: '2026-11-01T03:00:00Z',
  stale: false,
  channels: {
    nightly: {
      latest: 'nightly-20261101',
      releases: [
        {
          tag: 'nightly-20261101',
          publishedAt: '2026-11-01T04:00:00Z',
          installable: true,
          class: 'switch-newer',
        },
      ],
    },
    preview: { latest: null, releases: [] },
    stable: { latest: null, releases: [] },
  },
};

const GIT_CHECK: Json = {
  tier: 'source',
  update_available: true,
  current_commit: '0123abcd4567890123456789abcdef0123456789',
  compared: 'running',
  latest_commit: '89abcdef',
  current_branch: 'main',
  tracking_ref: 'origin/main',
  commits_behind: 2,
  new_commits: [
    { hash: '89abcdef', subject: 'fix: the second thing' },
    { hash: '4567cdef', subject: 'feat: the first thing' },
  ],
};

type Card = HTMLElement & {
  updateComplete: Promise<unknown>;
  status: UpdateStatus | null;
  busy: boolean;
};

beforeAll(async () => {
  await import('./admin-maintenance-update-card.js');
}, 60_000);

let el: Card | null = null;
let requests: Array<{ url: string; method: string }> = [];

afterEach(() => {
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
});

function respond(body: unknown, statusCode = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status: statusCode,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

async function settle(e: Card): Promise<void> {
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 30));
  await e.updateComplete;
}

async function mount(st: UpdateStatus, gitCheck: Json | number = GIT_CHECK): Promise<Card> {
  requests = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      requests.push({ url, method: init?.method ?? 'GET' });
      if (url.includes('/check-updates')) {
        if (typeof gitCheck === 'number') {
          return respond({ error: { code: 'invalid_request', message: 'no repo' } }, gitCheck);
        }
        return respond(gitCheck);
      }
      return respond(CATALOG);
    })
  );
  const e = document.createElement('scion-admin-maintenance-update-card') as unknown as Card;
  e.status = st;
  document.body.appendChild(e);
  await settle(e);
  return e;
}

function root(e: Card): ShadowRoot {
  return e.shadowRoot!;
}

function toggle(e: Card): (HTMLElement & { value: string }) | null {
  return root(e).querySelector('sl-radio-group.mode-select');
}

async function pickMode(e: Card, value: string): Promise<void> {
  const group = toggle(e)!;
  group.value = value;
  group.dispatchEvent(new Event('sl-change'));
  await settle(e);
}

function releaseRequests(): string[] {
  return requests.filter((r) => r.url.includes('/releases')).map((r) => r.url);
}

function button(e: Card, cls: string): HTMLElement {
  return root(e).querySelector<HTMLElement>(`sl-button.${cls}`)!;
}

describe('Mode toggle', () => {
  it('is hidden without a maintenance repo', async () => {
    el = await mount(status({ mode: { effective: 'release', repoConfigured: false } }));
    expect(toggle(el)).toBeNull();
    expect(root(el).querySelector('.release-panel')).not.toBeNull();
    expect(root(el).querySelector('.git-panel')).toBeNull();
  });

  it('is shown with a maintenance repo and starts on the hub mode', async () => {
    el = await mount(status());
    const group = toggle(el);
    expect(group).not.toBeNull();
    expect(group!.value).toBe('git');
    const labels = Array.from(group!.querySelectorAll('sl-radio-button')).map((b) => [
      b.getAttribute('value'),
      b.textContent?.trim(),
    ]);
    expect(labels).toEqual([
      ['git', 'Build from Git'],
      ['release', 'Release binary'],
    ]);
    expect(root(el).querySelector('.git-panel')).not.toBeNull();
    expect(root(el).querySelector('.release-panel')).toBeNull();
    expect(releaseRequests()).toEqual([]);
    expect(root(el).querySelector('.mode-note')).toBeNull();
  });

  it('starts on the release panel when the hub runs a release', async () => {
    el = await mount(status({ mode: { effective: 'release', repoConfigured: true } }));
    expect(toggle(el)!.value).toBe('release');
    expect(root(el).querySelector('.release-panel')).not.toBeNull();
    expect(root(el).querySelector('.git-panel')).toBeNull();
  });

  it('switches panels without persisting anything', async () => {
    el = await mount(status());
    await pickMode(el, 'release');
    expect(root(el).querySelector('.git-panel')).toBeNull();
    expect(root(el).querySelector('.release-panel')).not.toBeNull();
    // The release list loads lazily, once the panel is shown.
    expect(releaseRequests()).toEqual(['/api/v1/admin/maintenance/releases']);
    expect(root(el).querySelector('.mode-note')?.textContent?.replace(/\s+/g, ' ')).toContain(
      'once an update has been installed and verified'
    );
    // Only GETs: the toggle writes nothing to the hub.
    expect(requests.every((r) => r.method === 'GET')).toBe(true);

    await pickMode(el, 'git');
    expect(root(el).querySelector('.git-panel')).not.toBeNull();
    expect(root(el).querySelector('.release-panel')).toBeNull();
    expect(root(el).querySelector('.mode-note')).toBeNull();
  });
});

describe('Git panel', () => {
  it('shows the configured branch read-only', async () => {
    el = await mount(status());
    const branch = root(el).querySelector('.git-branch');
    expect(branch?.tagName).toBe('CODE');
    expect(branch?.textContent).toBe('main');
    expect(root(el).querySelector('.git-panel sl-input, .git-panel sl-select')).toBeNull();
  });

  it('names the current branch when none is configured', async () => {
    el = await mount(status({ mode: { effective: 'git', repoConfigured: true, repoBranch: '' } }));
    expect(root(el).querySelector('.git-branch')?.textContent).toBe(
      "the checkout's current branch"
    );
  });

  it('checks for upstream commits with check-updates?mode=git', async () => {
    el = await mount(status());
    button(el, 'check-upstream').click();
    await settle(el);
    expect(requests).toContainEqual({
      url: '/api/v1/admin/maintenance/check-updates?mode=git',
      method: 'POST',
    });
    expect(
      root(el).querySelector('.git-check-summary')?.textContent?.replace(/\s+/g, ' ').trim()
    ).toBe('2 new commits on origin/main that the running build (0123abcd4567) does not have.');
    expect(root(el).querySelector('.git-check-note')).toBeNull();
    const commits = Array.from(root(el).querySelectorAll('.git-check-commits li')).map((li) =>
      li.textContent?.replace(/\s+/g, ' ').trim()
    );
    expect(commits).toEqual(['89abcdef fix: the second thing', '4567cdef feat: the first thing']);
  });

  it('says up to date', async () => {
    el = await mount(status(), { ...GIT_CHECK, update_available: false, commits_behind: 0 });
    button(el, 'check-upstream').click();
    await settle(el);
    expect(
      root(el)
        .querySelector('.git-check.up-to-date .git-check-summary')
        ?.textContent?.replace(/\s+/g, ' ')
        .trim()
    ).toBe('The running build (0123abcd4567) is up to date with origin/main.');
  });

  it('says when it compared the checkout instead of the running build', async () => {
    el = await mount(status(), {
      ...GIT_CHECK,
      update_available: false,
      commits_behind: 0,
      compared: 'checkout',
      current_commit: '89abcdef01234567',
    });
    button(el, 'check-upstream').click();
    await settle(el);
    expect(
      root(el)
        .querySelector('.git-check .git-check-summary')
        ?.textContent?.replace(/\s+/g, ' ')
        .trim()
    ).toBe('The repository checkout (89abcdef0123) is up to date with origin/main.');
    expect(root(el).querySelector('.git-check-note')?.textContent?.replace(/\s+/g, ' ')).toContain(
      "running build's commit could not be found in the repository checkout"
    );
  });

  it('shows a failed check', async () => {
    el = await mount(status(), 400);
    button(el, 'check-upstream').click();
    await settle(el);
    expect(root(el).querySelector('sl-alert.git-check-error')?.textContent).toContain('no repo');
  });

  it('asks the page to rebuild', async () => {
    el = await mount(status());
    const requested = vi.fn();
    el.addEventListener('rebuild-request', requested);
    button(el, 'rebuild').click();
    expect(requested).toHaveBeenCalledTimes(1);
  });

  it('disables Rebuild while busy or when the installer is unsupported', async () => {
    el = await mount(status());
    el.busy = true;
    await settle(el);
    expect(button(el, 'rebuild').hasAttribute('disabled')).toBe(true);
    expect(root(el).querySelector('.git-panel .update-blocked')?.textContent).toContain(
      'in progress'
    );

    el.remove();
    el = await mount(
      status({
        installer: { strategy: 'unsupported', supported: false, reason: 'no sudoers rule' },
      })
    );
    expect(button(el, 'rebuild').hasAttribute('disabled')).toBe(true);
  });

  it('has no git panel without a maintenance repo', async () => {
    // Rebuild needs the checkout; the page keeps it in Routine Operations.
    el = await mount(status({ mode: { effective: 'git', repoConfigured: false } }));
    expect(toggle(el)).toBeNull();
    expect(root(el).querySelector('.git-panel')).toBeNull();
    expect(root(el).querySelector('.release-panel')).toBeNull();
  });
});

describe('View run of the last update', () => {
  it('names the rebuild-server run of a rebuild', async () => {
    const last = { runId: 'r', from: '', status: 'verified', at: '' };
    expect(lastUpdateOperation({ ...last, to: 'git-0123abcd4567', mode: 'git' })).toBe(
      'rebuild-server'
    );
    expect(lastUpdateOperation({ ...last, to: 'nightly-20261101' })).toBe('update-binary');

    el = await mount(
      status({
        lastUpdate: {
          runId: 'run-7',
          from: 'nightly-20261031',
          to: 'git-0123abcd4567',
          status: 'verified',
          at: '2026-11-01T03:00:00Z',
          mode: 'git',
        },
      })
    );
    const seen: ViewRunDetail[] = [];
    el.addEventListener('view-run', (e) => seen.push((e as CustomEvent<ViewRunDetail>).detail));
    root(el).querySelector<HTMLElement>('a.last-update-run')!.click();
    expect(seen).toEqual([{ runId: 'run-7', operationKey: 'rebuild-server' }]);
  });
});
