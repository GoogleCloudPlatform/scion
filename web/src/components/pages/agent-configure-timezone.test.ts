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

/**
 * Configure-page Timezone row (tz-refactor task 17): pin and unpin write the
 * agent PATCH's top-level explicitTimezone, the row shows resolvedTimezone
 * and timezoneSource from the PATCH response, a running agent gets the
 * "applies on next start" hint, and TZ never appears in (or leaves through)
 * the env table.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { browserTimeZone, setPreferredTimeZone } from '../../utils/time.js';

interface PatchCall {
  body: Record<string, unknown>;
}

interface PatchReply {
  status?: number;
  body: Record<string, unknown>;
}

interface ConfigureEl extends HTMLElement {
  loading: boolean;
  updateComplete: Promise<unknown>;
  envEntries: { key: string; value: string }[];
  requiredEnvKeys: string[];
  buildConfig(): { env?: Record<string, string> };
}

let patchCalls: PatchCall[] = [];
let patchReplies: PatchReply[] = [];

/**
 * Stubs fetch: GET returns `agent`, settings return defaults, and each PATCH
 * records its body and answers with the next queued reply.
 */
function stubFetch(agent: Record<string, unknown>): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString();
      if (url.includes('/settings/public')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({}),
        } as Response);
      }
      if (init?.method === 'PATCH') {
        patchCalls.push({ body: JSON.parse(String(init.body)) as Record<string, unknown> });
        const reply = patchReplies.shift() ?? { body: {} };
        const status = reply.status ?? 200;
        return Promise.resolve({
          ok: status < 400,
          status,
          json: async () => reply.body,
          text: async () => JSON.stringify(reply.body),
        } as Response);
      }
      if (url.includes('/agents/')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => agent,
        } as Response);
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        json: async () => ({}),
        text: async () => 'not found',
      } as Response);
    })
  );
}

function makeAgent(
  phase: string,
  appliedConfig: Record<string, unknown> = {}
): Record<string, unknown> {
  return { id: 'agent-1', name: 'agent-1', projectId: 'project-1', phase, appliedConfig };
}

async function settle(el: ConfigureEl): Promise<void> {
  for (let i = 0; i < 5; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(agent: Record<string, unknown>): Promise<ConfigureEl> {
  stubFetch(agent);
  await import('./agent-configure.js');
  const el = document.createElement('scion-page-agent-configure') as ConfigureEl;
  document.body.appendChild(el);
  const deadline = Date.now() + 2000;
  while (el.loading && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    await el.updateComplete;
  }
  await settle(el);
  return el;
}

function q(el: ConfigureEl, testId: string): HTMLElement | null {
  return el.shadowRoot!.querySelector(`[data-testid="${testId}"]`);
}

function text(el: ConfigureEl, testId: string): string {
  return (q(el, testId)?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

async function click(el: ConfigureEl, testId: string): Promise<void> {
  const target = q(el, testId);
  expect(target, `${testId} is rendered`).not.toBeNull();
  target!.click();
  await settle(el);
}

/** Opens Pin…, picks `zone` in the shared picker, and confirms. */
async function pin(el: ConfigureEl, zone: string): Promise<void> {
  await click(el, 'timezone-pin-open');
  const picker = el.shadowRoot!.querySelector('scion-timezone-picker');
  expect(picker, 'the shared timezone picker is shown').not.toBeNull();
  picker!.dispatchEvent(
    new CustomEvent('timezone-change', {
      detail: { timezone: zone },
      bubbles: true,
      composed: true,
    })
  );
  await settle(el);
  await click(el, 'timezone-pin-confirm');
}

/** A display preference that differs from the browser zone. */
const DISPLAY_ZONE = browserTimeZone() === 'Asia/Kathmandu' ? 'Pacific/Auckland' : 'Asia/Kathmandu';

beforeAll(async () => {
  await import('./agent-configure.js');
});

beforeEach(() => {
  patchCalls = [];
  patchReplies = [];
  setPreferredTimeZone(DISPLAY_ZONE);
});

afterEach(() => {
  vi.unstubAllGlobals();
  setPreferredTimeZone('');
  document.body.innerHTML = '';
});

describe('agent-configure Timezone row', () => {
  it('uses a display preference that differs from the browser zone', () => {
    expect(DISPLAY_ZONE).not.toBe(browserTimeZone());
  });

  it('on load shows an unpinned agent as "Not pinned" with the resolution order', async () => {
    const el = await mount(makeAgent('created'));
    expect(text(el, 'timezone-value')).toBe('Not pinned');
    expect(text(el, 'timezone-source')).toContain('hub default timezone');
    expect(q(el, 'timezone-unpin')).toBeNull();
    expect(q(el, 'timezone-next-start')).toBeNull();
  });

  it('on load shows an explicit pin, and a legacy pin with its own label', async () => {
    let el = await mount(makeAgent('created', { explicitTimezone: 'Europe/Paris' }));
    expect(text(el, 'timezone-value')).toBe('Europe/Paris');
    expect(text(el, 'timezone-source')).toBe('Pinned on this agent');
    expect(q(el, 'timezone-unpin')).not.toBeNull();
    el.remove();

    el = await mount(
      makeAgent('created', { explicitTimezone: 'Europe/Paris', explicitTimezoneLegacy: true })
    );
    expect(text(el, 'timezone-source')).toBe('Pinned (kept from an earlier TZ setting)');
  });

  it('pins through the shared picker and shows the PATCH response', async () => {
    const el = await mount(makeAgent('created'));
    patchReplies.push({
      body: {
        id: 'agent-1',
        phase: 'created',
        appliedConfig: { explicitTimezone: 'Asia/Kathmandu' },
        resolvedTimezone: 'Asia/Kathmandu',
        timezoneSource: 'explicit',
      },
    });

    // The picker has no "Auto" (empty) entry.
    await click(el, 'timezone-pin-open');
    const picker = el.shadowRoot!.querySelector('scion-timezone-picker')!;
    expect(picker.getAttribute('empty-label')).toBeNull();
    expect((picker as unknown as { emptyLabel: string }).emptyLabel).toBe('');
    await click(el, 'timezone-pin-cancel');

    await pin(el, 'Asia/Kathmandu');

    expect(patchCalls).toHaveLength(1);
    // Only explicitTimezone: pinning never sends (or changes) the form's config.
    expect(patchCalls[0].body).toEqual({ explicitTimezone: 'Asia/Kathmandu' });
    expect(text(el, 'timezone-value')).toBe('Asia/Kathmandu');
    expect(text(el, 'timezone-source')).toBe('Pinned on this agent');
    expect(q(el, 'timezone-unpin')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('scion-timezone-picker')).toBeNull();
    expect(q(el, 'timezone-next-start')).toBeNull();
  });

  it('unpins with explicitTimezone "" and shows the source the hub falls back to', async () => {
    const el = await mount(makeAgent('created', { explicitTimezone: 'Europe/Paris' }));
    patchReplies.push({
      body: {
        id: 'agent-1',
        phase: 'created',
        appliedConfig: {},
        resolvedTimezone: 'Asia/Tokyo',
        timezoneSource: 'hub-default',
      },
    });

    await click(el, 'timezone-unpin');

    expect(patchCalls).toEqual([{ body: { explicitTimezone: '' } }]);
    expect(text(el, 'timezone-value')).toBe('Asia/Tokyo');
    expect(text(el, 'timezone-source')).toBe('Hub default timezone');
    expect(q(el, 'timezone-unpin')).toBeNull();
  });

  it('round-trips pin then unpin', async () => {
    const el = await mount(makeAgent('created'));
    patchReplies.push(
      {
        body: {
          appliedConfig: { explicitTimezone: 'Europe/Lisbon' },
          resolvedTimezone: 'Europe/Lisbon',
          timezoneSource: 'explicit',
        },
      },
      {
        body: { appliedConfig: {}, resolvedTimezone: '', timezoneSource: 'none' },
      }
    );

    await pin(el, 'Europe/Lisbon');
    expect(text(el, 'timezone-value')).toBe('Europe/Lisbon');
    await click(el, 'timezone-unpin');

    expect(patchCalls.map((c) => c.body)).toEqual([
      { explicitTimezone: 'Europe/Lisbon' },
      { explicitTimezone: '' },
    ]);
    // resolvedTimezone "" with source "none": no TZ is sent, so UTC.
    expect(text(el, 'timezone-value')).toBe('UTC');
    expect(text(el, 'timezone-source')).toBe('Not set (container default)');
  });

  it.each([
    ['user', 'Your TZ environment variable'],
    ['project', 'Project TZ environment variable'],
    ['hub', 'Hub TZ environment variable'],
    ['broker', 'Broker TZ environment variable'],
    ['progeny', 'Inherited TZ environment variable'],
  ])('labels source %s from the PATCH response', async (source, label) => {
    const el = await mount(makeAgent('created', { explicitTimezone: 'Europe/Paris' }));
    patchReplies.push({
      body: { appliedConfig: {}, resolvedTimezone: 'Europe/Berlin', timezoneSource: source },
    });
    await click(el, 'timezone-unpin');
    expect(text(el, 'timezone-value')).toBe('Europe/Berlin');
    expect(text(el, 'timezone-source')).toBe(label);
  });

  it('falls back to the pinned value when a pin response lacks the timezone fields', async () => {
    const el = await mount(makeAgent('created', { explicitTimezone: 'Europe/Paris' }));
    patchReplies.push({ body: {} });
    await pin(el, 'Asia/Tokyo');
    expect(patchCalls).toEqual([{ body: { explicitTimezone: 'Asia/Tokyo' } }]);
    expect(text(el, 'timezone-value')).toBe('Asia/Tokyo');
    expect(text(el, 'timezone-source')).toBe('Pinned on this agent');
    expect(q(el, 'timezone-unpin')).not.toBeNull();
  });

  it('shows "Not pinned" when an unpin response lacks the timezone fields', async () => {
    const el = await mount(makeAgent('created', { explicitTimezone: 'Europe/Paris' }));
    patchReplies.push({ body: {} });
    await click(el, 'timezone-unpin');
    expect(patchCalls).toEqual([{ body: { explicitTimezone: '' } }]);
    expect(text(el, 'timezone-value')).toBe('Not pinned');
    expect(text(el, 'timezone-source')).toContain('hub default timezone');
    expect(q(el, 'timezone-unpin')).toBeNull();
  });

  it('rejects an invalid zone without sending a PATCH', async () => {
    const el = await mount(makeAgent('created'));
    await pin(el, 'Mars/Olympus');
    expect(patchCalls).toHaveLength(0);
    expect(text(el, 'timezone-error')).toContain('not a valid IANA timezone');
    expect(text(el, 'timezone-value')).toBe('Not pinned');
  });

  it('shows the hub error and keeps the old value when the PATCH fails', async () => {
    const el = await mount(makeAgent('created', { explicitTimezone: 'Europe/Paris' }));
    patchReplies.push({ status: 400, body: { error: { message: 'bad zone' } } });
    await pin(el, 'Asia/Tokyo');
    expect(patchCalls).toHaveLength(1);
    expect(q(el, 'timezone-error')).not.toBeNull();
    expect(text(el, 'timezone-value')).toBe('Europe/Paris');
  });

  it('Save updates the row from its PATCH response', async () => {
    const el = await mount(makeAgent('created'));
    patchReplies.push({
      body: { appliedConfig: {}, resolvedTimezone: 'Europe/Lisbon', timezoneSource: 'user' },
    });
    await (el as unknown as { handleSave(): Promise<void> }).handleSave();
    await settle(el);
    expect(patchCalls).toHaveLength(1);
    expect(patchCalls[0].body).not.toHaveProperty('explicitTimezone');
    expect(text(el, 'timezone-value')).toBe('Europe/Lisbon');
    expect(text(el, 'timezone-source')).toBe('Your TZ environment variable');
  });
});

/**
 * Outside "created" the page must offer no path that sends config: no form
 * fields, and no button other than the Timezone row's own (in particular no
 * Save or Start).
 */
function expectTimezoneOnlyView(el: ConfigureEl): void {
  const root = el.shadowRoot!;
  expect(root.querySelector('sl-tab-group')).toBeNull();
  expect(root.querySelector('sl-input, sl-textarea, sl-select, sl-checkbox, sl-switch')).toBeNull();
  const buttons = Array.from(root.querySelectorAll('sl-button'));
  expect(buttons.length).toBeGreaterThan(0);
  for (const button of buttons) {
    expect(['timezone-pin-open', 'timezone-unpin']).toContain(button.getAttribute('data-testid'));
  }
  const labels = buttons.map((b) => b.textContent?.trim() ?? '');
  expect(labels.some((l) => /\b(Save|Start)\b/.test(l))).toBe(false);
  expect(q(el, 'phase-notice')?.textContent?.replace(/\s+/g, ' ')).toContain(
    'This page edits other settings only while an agent is in "created" phase.'
  );
}

describe('agent-configure Timezone row on a non-created agent', () => {
  it('a running agent shows the row and "applies on next start", and can pin', async () => {
    const el = await mount(makeAgent('running', { explicitTimezone: 'Europe/Paris' }));
    // Only the timezone is editable in this phase: a neutral notice says so,
    // no error banner, and none of the other form fields render.
    expect(q(el, 'phase-notice')?.textContent).toContain('only its timezone can be changed');
    expect(el.shadowRoot!.querySelector('.error-banner')).toBeNull();
    expectTimezoneOnlyView(el);
    expect(q(el, 'timezone-unpin')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('a.back-link')?.getAttribute('href')).toBe(
      '/agents/agent-1'
    );
    expect(text(el, 'timezone-value')).toBe('Europe/Paris');
    expect(text(el, 'timezone-next-start')).toBe(
      "A timezone change applies on the agent's next start."
    );

    patchReplies.push({
      body: {
        appliedConfig: { explicitTimezone: 'Asia/Kathmandu' },
        resolvedTimezone: 'Asia/Kathmandu',
        timezoneSource: 'explicit',
        warnings: ["explicitTimezone applies at the agent's next start"],
      },
    });
    await pin(el, 'Asia/Kathmandu');
    expect(patchCalls).toEqual([{ body: { explicitTimezone: 'Asia/Kathmandu' } }]);
    expect(text(el, 'timezone-value')).toBe('Asia/Kathmandu');
    expect(text(el, 'timezone-source')).toBe('Pinned on this agent');
    expect(q(el, 'timezone-next-start')).not.toBeNull();
  });

  it('a legacy-pinned running agent can be unpinned and shows the resolved value', async () => {
    const el = await mount(
      makeAgent('running', { explicitTimezone: 'Europe/Paris', explicitTimezoneLegacy: true })
    );
    expect(text(el, 'timezone-value')).toBe('Europe/Paris');
    expect(text(el, 'timezone-source')).toBe('Pinned (kept from an earlier TZ setting)');
    expect(q(el, 'timezone-next-start')).not.toBeNull();

    patchReplies.push({
      body: {
        appliedConfig: {},
        resolvedTimezone: 'America/Chicago',
        timezoneSource: 'project',
        warnings: ["explicitTimezone applies at the agent's next start"],
      },
    });
    await click(el, 'timezone-unpin');
    expect(patchCalls).toEqual([{ body: { explicitTimezone: '' } }]);
    expect(text(el, 'timezone-value')).toBe('America/Chicago');
    expect(text(el, 'timezone-source')).toBe('Project TZ environment variable');
    expect(q(el, 'timezone-unpin')).toBeNull();
    expect(q(el, 'timezone-next-start')).not.toBeNull();
  });

  it.each(['stopped', 'starting', 'suspended', 'error'])(
    'a %s agent shows only the row, with the phase-neutral next-start hint',
    async (phase) => {
      const el = await mount(makeAgent(phase));
      expectTimezoneOnlyView(el);
      expect(q(el, 'timezone-pin-open')).not.toBeNull();
      expect(text(el, 'timezone-value')).toBe('Not pinned');
      expect(text(el, 'timezone-next-start')).toBe(
        "A timezone change applies on the agent's next start."
      );
    }
  );

  it('shows the next-start hint when the PATCH reports it, whatever the loaded phase', async () => {
    const el = await mount(makeAgent('stopped'));
    patchReplies.push({
      body: {
        appliedConfig: { explicitTimezone: 'Europe/Paris' },
        resolvedTimezone: 'Europe/Paris',
        timezoneSource: 'explicit',
        warnings: ["explicitTimezone applies at the agent's next start"],
      },
    });
    await pin(el, 'Europe/Paris');
    expect(q(el, 'timezone-next-start')).not.toBeNull();
  });
});

describe('agent-configure env table never owns TZ', () => {
  it('filters TZ out on load, so an empty gathered TZ is not a required row', async () => {
    const el = await mount(
      makeAgent('created', {
        env: { TZ: '', NEEDED: '', FOO: 'bar' },
        inlineConfig: { env: { TZ: '', NEEDED: '', FOO: 'bar' } },
      })
    );
    expect(el.envEntries.map((e) => e.key).sort()).toEqual(['FOO', 'NEEDED']);
    expect(el.requiredEnvKeys).toEqual(['NEEDED']);
  });

  it('a loaded TZ is not re-sent, and its absence is not an edit', async () => {
    const el = await mount(makeAgent('created', { env: { TZ: 'Europe/Paris', FOO: 'bar' } }));
    expect(el.buildConfig()).not.toHaveProperty('env');
  });

  it('a TZ row typed into the table is never sent', async () => {
    const el = await mount(makeAgent('created', { env: { FOO: 'bar' } }));
    el.envEntries = [...el.envEntries, { key: 'TZ', value: 'Asia/Tokyo' }];
    expect(el.buildConfig()).not.toHaveProperty('env');

    el.envEntries = [
      { key: 'FOO', value: 'baz' },
      { key: 'TZ', value: 'Asia/Tokyo' },
    ];
    expect(el.buildConfig().env).toEqual({ FOO: 'baz' });
  });

  it('Save never sends a TZ key in config.env', async () => {
    const el = await mount(makeAgent('created', { env: { TZ: 'Europe/Paris', FOO: 'bar' } }));
    el.envEntries = [
      { key: 'FOO', value: 'changed' },
      { key: 'TZ', value: 'Asia/Tokyo' },
    ];
    patchReplies.push({ body: { resolvedTimezone: 'Europe/Paris', timezoneSource: 'legacy' } });
    await (el as unknown as { handleSave(): Promise<void> }).handleSave();
    expect(patchCalls).toHaveLength(1);
    const config = patchCalls[0].body.config as { env?: Record<string, string> };
    expect(config.env).toEqual({ FOO: 'changed' });
    expect(patchCalls[0].body).not.toHaveProperty('explicitTimezone');
  });
});
