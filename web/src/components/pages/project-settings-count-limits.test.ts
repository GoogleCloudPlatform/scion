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

/**
 * Project default count limits (ptone/scion#3925): each is Inherit (null),
 * Unlimited (0) or a limit. The page loads a stored 0 as Unlimited, sends 0
 * for Unlimited and null to clear, and leaves an untouched field out of the
 * save, so a save from this page never changes a limit it did not edit.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { countLimitOrNull, isCountLimitWarning, parseCountLimitInput } from './project-settings.js';

interface Internals {
  projectId: string;
  updateComplete: Promise<unknown>;
  handleSaveConfig: () => Promise<void>;
  configDefaultMaxTurns: number | null;
  configDefaultMaxModelCalls: number | null;
  countLimitErrors: Record<string, string>;
  countLimitWarnings: string[];
  settingsError: string | null;
  loadResolvedSettings: () => Promise<void>;
}

let putWarnings: string[] = [];

let stored: Record<string, unknown> = {};
let puts: Array<Record<string, unknown>> = [];

function stubFetch(): void {
  const json = (body: unknown, status = 200): Promise<Response> =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(requestUrl(input), 'http://hub.example.com').pathname;
      if (init?.method === 'PUT' && path.endsWith('/settings') && typeof init.body === 'string') {
        const body = JSON.parse(init.body) as Record<string, unknown>;
        puts.push(body);
        stored = { ...stored, ...body };
        for (const k of Object.keys(stored)) if (stored[k] === null) delete stored[k];
        return json(putWarnings.length > 0 ? { ...stored, warnings: putWarnings } : stored);
      }
      if (path.endsWith('/settings/resolved')) {
        return json({ projectId: 'proj-1', project: stored, settings: {} });
      }
      if (path.endsWith('/settings')) return json(stored);
      if (path.includes('/templates')) {
        return json({ templates: [], page: 1, pageSize: 100, total: 0 });
      }
      if (/\/api\/v1\/projects\/[^/]+$/.test(path)) {
        return json({
          id: 'proj-1',
          name: 'Test Project',
          slug: 'test-project',
          _capabilities: { actions: ['update', 'manage'] },
        });
      }
      return json({});
    })
  );
}

beforeAll(async () => {
  stubFetch();
  await import('./project-settings.js');
}, 30_000);

afterEach(() => {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function mount(settings: Record<string, unknown>): Promise<HTMLElement & Internals> {
  stored = settings;
  puts = [];
  putWarnings = [];
  stubFetch();
  vi.spyOn(console, 'error').mockImplementation(() => {});
  const el = document.createElement('scion-page-project-settings') as HTMLElement & Internals;
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 400));
  await el.updateComplete;
  return el;
}

describe('count-limit helpers', () => {
  it('maps stored values to page state', () => {
    expect(countLimitOrNull(undefined)).toBeNull();
    expect(countLimitOrNull(null)).toBeNull();
    expect(countLimitOrNull(0)).toBe(0);
    expect(countLimitOrNull(12)).toBe(12);
    expect(countLimitOrNull(-1)).toBeNull();
  });

  it('parses a positive integer, empty as Inherit, and anything else as an error', () => {
    expect(parseCountLimitInput('7')).toEqual({ value: 7 });
    expect(parseCountLimitInput(' 40 ')).toEqual({ value: 40 });
    expect(parseCountLimitInput('')).toEqual({ value: null });
    expect(parseCountLimitInput('0').error).toContain('Unlimited');
    for (const bad of ['-3', '1.5', '1e3', 'abc']) {
      expect(parseCountLimitInput(bad).error).toBe(
        'Must be a whole number greater than 0, or Unlimited.'
      );
    }
  });

  it('recognises count-limit warnings', () => {
    expect(isCountLimitWarning('defaultMaxTurns: Unlimited (0) applies only ...')).toBe(true);
    expect(isCountLimitWarning('defaultMaxModelCalls: ...')).toBe(true);
    expect(isCountLimitWarning('defaultGCPIdentityServiceAccountIDByProfile[a]: ...')).toBe(false);
  });
});

describe('project settings count limits', () => {
  it('loads a stored 0 as Unlimited and an absent value as Inherit', async () => {
    const el = await mount({ defaultMaxTurns: 0 });
    expect(el.configDefaultMaxTurns).toBe(0);
    expect(el.configDefaultMaxModelCalls).toBeNull();
    const boxes = Array.from(el.shadowRoot!.querySelectorAll('sl-checkbox.unlimited'));
    expect(boxes).toHaveLength(2);
    expect(boxes[0]!.hasAttribute('checked')).toBe(true);
    expect(boxes[1]!.hasAttribute('checked')).toBe(false);
  });

  it('leaves untouched count limits out of a save', async () => {
    const el = await mount({ defaultMaxTurns: 0, defaultMaxModelCalls: 20 });
    await el.handleSaveConfig();
    expect(puts).toHaveLength(1);
    expect('defaultMaxTurns' in puts[0]!).toBe(false);
    expect('defaultMaxModelCalls' in puts[0]!).toBe(false);
  });

  it('never sends 0 for an empty, untouched field (the pre-upgrade stale-tab case)', async () => {
    const el = await mount({});
    await el.handleSaveConfig();
    expect(puts).toHaveLength(1);
    expect('defaultMaxTurns' in puts[0]!).toBe(false);
    expect('defaultMaxModelCalls' in puts[0]!).toBe(false);
  });

  it('sends 0 for Unlimited, null to clear and n for a limit', async () => {
    const el = await mount({ defaultMaxModelCalls: 20 });
    el.configDefaultMaxTurns = 0;
    el.configDefaultMaxModelCalls = null;
    await el.handleSaveConfig();
    expect(puts[0]!.defaultMaxTurns).toBe(0);
    expect(puts[0]!.defaultMaxModelCalls).toBeNull();

    const el2 = await mount({ defaultMaxTurns: 0 });
    el2.configDefaultMaxTurns = 15;
    await el2.handleSaveConfig();
    expect(puts[0]!.defaultMaxTurns).toBe(15);
    expect('defaultMaxModelCalls' in puts[0]!).toBe(false);
  });

  it('uses the saved values as the next baseline (two saves in one session)', async () => {
    const el = await mount({});
    el.configDefaultMaxTurns = 5;
    await el.handleSaveConfig();
    expect(puts[0]!.defaultMaxTurns).toBe(5);
    el.configDefaultMaxTurns = null;
    await el.handleSaveConfig();
    expect(puts).toHaveLength(2);
    expect(puts[1]!.defaultMaxTurns).toBeNull();

    el.configDefaultMaxModelCalls = 5;
    await el.handleSaveConfig();
    el.configDefaultMaxModelCalls = 0;
    await el.handleSaveConfig();
    el.configDefaultMaxModelCalls = 5;
    await el.handleSaveConfig();
    expect(puts).toHaveLength(5);
    expect(puts[3]!.defaultMaxModelCalls).toBe(0);
    expect(puts[4]!.defaultMaxModelCalls).toBe(5);
  });

  it('refuses to save while a count limit input is invalid, keeping the stored value', async () => {
    const el = await mount({ defaultMaxTurns: 20 });
    const input = el.shadowRoot!.querySelector('sl-input[inputmode="numeric"]') as HTMLElement & {
      value: string;
    };
    expect(input).toBeTruthy();
    input.value = '1.5';
    input.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(el.configDefaultMaxTurns).toBe(20);
    expect(Object.values(el.countLimitErrors)).toEqual([
      'Must be a whole number greater than 0, or Unlimited.',
    ]);
    expect(el.shadowRoot!.querySelector('.count-limit-error')?.textContent).toContain(
      'whole number'
    );
    await el.handleSaveConfig();
    expect(puts).toHaveLength(0);
    expect(el.settingsError).toContain('default count limits');

    input.value = '30';
    input.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(el.countLimitErrors).toEqual({});
    await el.handleSaveConfig();
    expect(puts[0]!.defaultMaxTurns).toBe(30);
  });

  it('shows count-limit warnings with the count limits, not in the profile section', async () => {
    const el = await mount({});
    putWarnings = [
      'defaultMaxTurns: Unlimited (0) applies only on Runtime Brokers that support it',
    ];
    el.configDefaultMaxTurns = 0;
    await el.handleSaveConfig();
    await el.updateComplete;
    expect(el.countLimitWarnings).toEqual(putWarnings);
    expect(el.shadowRoot!.querySelector('.count-limit-warning')?.textContent).toContain(
      'Unlimited (0) applies only'
    );
  });

  it('clears count-limit input errors and warnings when settings reload', async () => {
    const el = await mount({ defaultMaxTurns: 20 });
    el.countLimitErrors = {
      'scion.io/default-max-turns': 'Must be a whole number greater than 0, or Unlimited.',
    };
    el.countLimitWarnings = ['defaultMaxTurns: stale'];
    await el.loadResolvedSettings();
    await el.updateComplete;
    expect(el.countLimitErrors).toEqual({});
    expect(el.countLimitWarnings).toEqual([]);
    await el.handleSaveConfig();
    expect(puts).toHaveLength(1);
    expect(el.settingsError).toBeNull();
  });
});
