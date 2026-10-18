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

// The maintenance page shows a "re-provision needed" banner while the last
// Rebuild from git found the installed systemd unit different from the
// template (unitStatus in the operations list). The banner carries the full
// remediation itself.

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

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

function handler(unitStatus: unknown) {
  return (input: string | URL | Request): Promise<Response> => {
    const path = pathOf(input);
    if (path === '/api/v1/admin/maintenance/operations') {
      const body: Record<string, unknown> = { migrations: [], operations: [] };
      if (unitStatus !== undefined) body.unitStatus = unitStatus;
      return json(body);
    }
    if (path === '/api/v1/admin/maintenance') {
      return json({ enabled: false, message: '' });
    }
    return json({});
  };
}

type Page = HTMLElement & {
  updateComplete: Promise<unknown>;
  stopPolling(): void;
};

beforeAll(async () => {
  await import('./admin-maintenance.js');
}, 60_000);

let el: Page | null = null;

afterEach(() => {
  el?.stopPolling();
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
});

async function mount(unitStatus: unknown): Promise<Page> {
  vi.stubGlobal('fetch', vi.fn(handler(unitStatus)));
  const e = document.createElement('scion-page-admin-maintenance') as unknown as Page;
  document.body.appendChild(e);
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 200));
  await e.updateComplete;
  return e;
}

function banner(e: Page): HTMLElement | null {
  return e.shadowRoot?.querySelector<HTMLElement>('.unit-drift-banner') ?? null;
}

function text(node: HTMLElement): string {
  return (node.textContent ?? '').replace(/\s+/g, ' ');
}

const DRIFT = {
  status: 'drift',
  checkedAt: '2026-10-18T12:00:00Z',
  commit: 'abc1234',
  branch: 'main',
  service: 'scion-hub.service',
  summary: ['installed: ExecStartPre=/bin/true', 'template:  Restart=always'],
};

describe('admin maintenance unit drift banner', () => {
  it('shows the full remediation on drift', async () => {
    el = await mount(DRIFT);
    const b = banner(el);
    expect(b).not.toBeNull();
    const t = text(b!);
    expect(t).toContain('Re-provision needed:');
    expect(t).toContain(
      'the installed systemd unit for scion-hub.service differs from the template'
    );
    expect(t).toContain(
      'run scripts/starter-hub/gce-start-hub.sh --full from the workstation and checkout used to provision this hub, at commit abc1234 on branch main, with the same flags'
    );
    expect(t).toContain('--no-tls');
    expect(t).toContain('can reconfigure the hub');
    expect(t).toContain('a drop-in (scion-hub.service.d)');
    expect(t).toContain('This notice clears after a Rebuild from git reports a match.');
    expect(b!.querySelector('sl-icon')?.getAttribute('name')).toBe('exclamation-triangle');
    expect(b!.querySelector('.unit-drift-summary')?.textContent).toBe(
      'installed: ExecStartPre=/bin/true\ntemplate:  Restart=always'
    );
  });

  it('names the checked-out commit when no commit or branch is known', async () => {
    el = await mount({ ...DRIFT, commit: '', branch: '' });
    expect(text(banner(el)!)).toContain('at the checked-out commit, with the same flags');
  });

  it('names the commit alone when no branch is known', async () => {
    el = await mount({ ...DRIFT, branch: '' });
    expect(text(banner(el)!)).toContain('at commit abc1234, with the same flags');
  });

  it('names the branch when no commit is known', async () => {
    el = await mount({ ...DRIFT, commit: '' });
    expect(text(banner(el)!)).toContain(
      'at the checked-out commit on branch main, with the same flags'
    );
  });

  it('renders summary markup as text', async () => {
    el = await mount({ ...DRIFT, summary: ['installed: X=<img src=x onerror=alert(1)>'] });
    const summary = banner(el)!.querySelector('.unit-drift-summary')!;
    expect(summary.querySelector('img')).toBeNull();
    expect(summary.textContent).toBe('installed: X=<img src=x onerror=alert(1)>');
  });

  it('shows nothing on match or without a status', async () => {
    el = await mount({ ...DRIFT, status: 'match', summary: [] });
    expect(banner(el)).toBeNull();
    el.stopPolling();
    el.remove();
    el = await mount(undefined);
    expect(banner(el)).toBeNull();
  });
});
