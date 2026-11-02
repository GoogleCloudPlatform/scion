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

// Focus of the typed downgrade confirmation. The real Shoelace sl-dialog
// and sl-input do not run under happy-dom (cloning sl-input throws "Cannot
// read from private field", and ShadowRoot.activeElement throws for the
// nested shadow roots), so this file registers stand-ins that follow the
// Shoelace 2.20 sl-dialog contract (dialog.component.ts):
// - initial focus runs only in handleOpenChange, @watch('open',
//   { waitUntilFirstUpdate: true }): a dialog created already open never
//   focuses anything;
// - when open turns true after the first update, the [autofocus] child is
//   focused in the next animation frame.
// The page must therefore render the dialog closed and open it after its
// first update, and on a re-prompt focus the input itself.

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

/** The element the stand-ins last focused. */
let focused: Element | null = null;

function recordFocus(e: Element): void {
  focused = e;
}

/**
 * Like sl-input: focus() goes to an inner input, which follows the
 * disabled attribute only in the element's own (later) update and cannot
 * take focus while disabled.
 */
class StandInInput extends HTMLElement {
  static observedAttributes = ['disabled'];
  value = '';
  updateComplete: Promise<boolean> = Promise.resolve(true);
  private innerDisabled = false;

  attributeChangedCallback(): void {
    const disabled = this.hasAttribute('disabled');
    this.updateComplete = new Promise((resolve) =>
      setTimeout(() => {
        this.innerDisabled = disabled;
        resolve(true);
      }, 0)
    );
  }

  override focus(): void {
    if (!this.innerDisabled) recordFocus(this);
  }
}

class StandInDialog extends HTMLElement {
  static observedAttributes = ['open'];
  /** Whether open was already set when the first update ran. */
  bornOpen: boolean | null = null;
  updateComplete: Promise<boolean> = Promise.resolve(true);
  private firstUpdateDone = false;

  connectedCallback(): void {
    this.updateComplete = Promise.resolve().then(() => {
      if (!this.firstUpdateDone) {
        this.firstUpdateDone = true;
        this.bornOpen = this.hasAttribute('open');
      }
      return true;
    });
  }

  attributeChangedCallback(name: string, oldValue: string | null, value: string | null): void {
    if (name !== 'open' || !this.firstUpdateDone || oldValue !== null || value === null) return;
    requestAnimationFrame(() => {
      this.querySelector<HTMLElement>('[autofocus]')?.focus();
    });
  }

  get open(): boolean {
    return this.hasAttribute('open');
  }
}

type Json = Record<string, unknown>;

const STATUS: Json = {
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
  downgrade: { floor: 'nightly-20261009', snapshotsSupported: true },
  pendingUpdate: null,
  lastUpdate: null,
};

const CATALOG: Json = {
  fetchedAt: '2026-10-23T10:00:00Z',
  stale: false,
  channels: {
    nightly: { latest: null, releases: [] },
    preview: { latest: null, releases: [] },
    stable: { latest: null, releases: [] },
  },
};

function needsConfirmation(target: string): { status: number; body: Json } {
  return {
    status: 409,
    body: {
      error: {
        code: 'downgrade_requires_confirmation',
        message: `${target} is older than the running version`,
        details: {
          target,
          current: 'nightly-20261022',
          class: 'downgrade',
          floor: 'nightly-20261009',
          snapshotPath: '/opt/scion/snapshots',
          risks: ['The database schema is not migrated down.'],
        },
      },
    },
  };
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

type Page = HTMLElement & {
  updateComplete: Promise<unknown>;
  updateConfirmTarget: string | null;
  stopPolling(): void;
};

const runBodies: unknown[] = [];

beforeAll(async () => {
  customElements.define('sl-dialog', StandInDialog);
  customElements.define('sl-input', StandInInput);
  await import('./admin-maintenance.js');
}, 60_000);

let el: Page | null = null;

afterEach(() => {
  el?.stopPolling();
  el?.remove();
  el = null;
  runBodies.length = 0;
  focused = null;
  vi.unstubAllGlobals();
});

async function settle(e: Page): Promise<void> {
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await e.updateComplete;
}

async function mount(runs: Array<{ status: number; body: Json }>): Promise<Page> {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request, init?: RequestInit): Promise<Response> => {
      const path = pathOf(input);
      if (path === '/api/v1/admin/maintenance/update-status') return json(STATUS);
      if (path === '/api/v1/admin/maintenance/releases') return json(CATALOG);
      if (path === '/api/v1/admin/maintenance/update-available') {
        return json({ update_available: false });
      }
      if (path === '/api/v1/admin/maintenance/operations') {
        return json({ migrations: [], operations: [] });
      }
      if (path === '/api/v1/admin/maintenance') return json({ enabled: false });
      if ((init?.method ?? 'GET').toUpperCase() === 'POST' && path.endsWith('/run')) {
        runBodies.push(JSON.parse(init?.body as string));
        const r = runs[runBodies.length - 1] ?? runs[runs.length - 1];
        // Slower than a microtask, like the network: the input is disabled
        // while the request is in flight.
        return new Promise((resolve) => setTimeout(resolve, 20)).then(() => json(r.body, r.status));
      }
      return json({});
    })
  );
  const e = document.createElement('scion-page-admin-maintenance') as unknown as Page;
  document.body.appendChild(e);
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 200));
  await e.updateComplete;
  return e;
}

function dialog(e: Page): HTMLElement | null {
  return e.shadowRoot!.querySelector<HTMLElement>('sl-dialog.downgrade-confirm');
}

function typedInput(e: Page): HTMLElement & { value: string } {
  return dialog(e)!.querySelector<HTMLElement & { value: string }>('sl-input.downgrade-typed')!;
}

/** Whether the typed input was the last element focused. */
function typedInputFocused(e: Page): boolean {
  return focused !== null && focused === typedInput(e);
}

/** Confirms the Update Hub dialog, which gets the 409 and opens the confirmation. */
async function openConfirmation(e: Page): Promise<void> {
  e.updateConfirmTarget = 'nightly-20261020';
  await settle(e);
  e.shadowRoot!.querySelector<HTMLElement>(
    'sl-dialog[label="Update Hub"] sl-button.confirm-update'
  )!.click();
  await settle(e);
}

describe('Downgrade confirmation focus', () => {
  it('opens the dialog after it rendered and moves focus into the typed input', async () => {
    el = await mount([needsConfirmation('nightly-20261020')]);
    await openConfirmation(el);

    const d = dialog(el) as StandInDialog | null;
    expect(d).not.toBeNull();
    // Created closed: a dialog born open would never move focus.
    expect(d!.bornOpen).toBe(false);
    await vi.waitFor(() => expect(d!.open).toBe(true));
    await vi.waitFor(() => expect(typedInputFocused(el!)).toBe(true));
  });

  it('moves focus back into the input when the hub asks again for another tag', async () => {
    el = await mount([
      needsConfirmation('nightly-20261020'),
      needsConfirmation('nightly-20261019'),
    ]);
    await openConfirmation(el);
    await vi.waitFor(() => expect(typedInputFocused(el!)).toBe(true));

    const input = typedInput(el);
    input.value = 'nightly-20261020';
    input.dispatchEvent(new Event('sl-input'));
    await settle(el);
    // Focus leaves the input (a click on Confirm, or the input disabled
    // while the request is in flight).
    focused = null;
    dialog(el)!.querySelector<HTMLElement>('sl-button.confirm-downgrade')!.click();
    await settle(el);

    expect(runBodies).toHaveLength(2);
    expect(typedInput(el).getAttribute('label')).toBe('Type nightly-20261019 to confirm');
    await vi.waitFor(() => expect(typedInputFocused(el!)).toBe(true));
  });
});
