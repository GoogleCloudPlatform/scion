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
import { showToast } from '../../utils/toast.js';

// The page reports a save with a toast; the Shoelace alert behind it is not
// available in the test environment, so the toast is mocked (as in the
// other maintenance page tests) and its calls are asserted.
vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

// The maintenance message editor on the Maintenance page (ptone/scion#3925):
// it shows the message the hub reports, saves a message without touching
// the mode, and resets to the hub default with an empty message.

const DEFAULT_MESSAGE = 'System offline for maintenance';

interface Hub {
  enabled: boolean;
  message: string;
  puts: Record<string, unknown>[];
}

function handler(hub: Hub) {
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const path = new URL(url, 'http://localhost').pathname;
    let body: unknown = {};
    if (path === '/api/v1/admin/maintenance') {
      if (init?.method === 'PUT') {
        const req = JSON.parse(init.body as string) as { enabled?: boolean; message?: string };
        hub.puts.push(req);
        if (typeof req.enabled === 'boolean') hub.enabled = req.enabled;
        if (typeof req.message === 'string') hub.message = req.message || DEFAULT_MESSAGE;
      }
      body = { enabled: hub.enabled, message: hub.message };
    } else if (path.endsWith('/operations')) {
      body = { migrations: [], operations: [] };
    }
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

beforeAll(async () => {
  await import('./admin-maintenance.js');
}, 60_000);

type PageElement = HTMLElement & { updateComplete: Promise<unknown> };

let el: PageElement | null = null;
afterEach(() => {
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
  vi.mocked(showToast).mockClear();
});

async function mount(hub: Hub): Promise<PageElement> {
  vi.stubGlobal('fetch', vi.fn(handler(hub)));
  const e = document.createElement('scion-page-admin-maintenance') as PageElement;
  document.body.appendChild(e);
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 200));
  await e.updateComplete;
  return e;
}

function textarea(e: PageElement): HTMLTextAreaElement {
  const t = e.shadowRoot!.querySelector('.maintenance-message sl-textarea');
  if (!t) throw new Error('no maintenance message textarea');
  return t as HTMLTextAreaElement;
}

function button(e: PageElement, cls: string): HTMLButtonElement {
  const b = e.shadowRoot!.querySelector(`.maintenance-message .${cls}`);
  if (!b) throw new Error(`no button .${cls}`);
  return b as HTMLButtonElement;
}

async function type(e: PageElement, value: string): Promise<void> {
  const t = textarea(e);
  t.value = value;
  t.dispatchEvent(new Event('sl-input'));
  await e.updateComplete;
}

async function click(e: PageElement, cls: string): Promise<void> {
  button(e, cls).click();
  await vi.waitFor(() => {
    expect((e as unknown as { maintenanceMessageSaving: boolean }).maintenanceMessageSaving).toBe(
      false
    );
  });
  await e.updateComplete;
}

describe('admin maintenance message', () => {
  it('shows the message the hub reports', async () => {
    el = await mount({ enabled: false, message: 'Back at noon', puts: [] });
    expect(textarea(el).value).toBe('Back at noon');
    expect(button(el, 'save-maintenance-message').hasAttribute('disabled')).toBe(true);
  });

  it('saves an edited message without sending the mode', async () => {
    const hub: Hub = { enabled: true, message: DEFAULT_MESSAGE, puts: [] };
    el = await mount(hub);
    await type(el, 'Upgrading the database');
    expect(button(el, 'save-maintenance-message').hasAttribute('disabled')).toBe(false);
    await click(el, 'save-maintenance-message');
    expect(hub.puts).toEqual([{ message: 'Upgrading the database' }]);
    expect(hub.enabled).toBe(true);
    expect(showToast).toHaveBeenCalledWith('Maintenance message saved.', 'success');
    expect(textarea(el).value).toBe('Upgrading the database');
    expect(button(el, 'save-maintenance-message').hasAttribute('disabled')).toBe(true);
  });

  it('does not save a blank message (use the default button instead)', async () => {
    el = await mount({ enabled: false, message: 'Back at noon', puts: [] });
    await type(el, '   ');
    expect(button(el, 'save-maintenance-message').hasAttribute('disabled')).toBe(true);
  });

  it('resets to the default with an empty message', async () => {
    const hub: Hub = { enabled: false, message: 'Back at noon', puts: [] };
    el = await mount(hub);
    await click(el, 'reset-maintenance-message');
    expect(hub.puts).toEqual([{ message: '' }]);
    expect(textarea(el).value).toBe(DEFAULT_MESSAGE);
    expect(showToast).toHaveBeenCalledWith('Maintenance message reset to the default.', 'success');
  });

  it('a mode toggle keeps an unsaved edit', async () => {
    const hub: Hub = { enabled: false, message: 'Back at noon', puts: [] };
    el = await mount(hub);
    await type(el, 'Draft text');
    (el.shadowRoot!.querySelector('.maintenance-mode-toggle .toggle-track') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(hub.puts.length).toBe(1);
    });
    await el.updateComplete;
    expect(hub.puts[0]).toEqual({ enabled: true });
    expect(textarea(el).value).toBe('Draft text');
  });
});
