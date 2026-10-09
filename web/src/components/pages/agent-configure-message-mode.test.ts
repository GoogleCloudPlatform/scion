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
 * Configure-page Message Mode (ptone/scion#3983): the agent PATCH has no
 * message-mode field, so the page no longer offers an editable select that
 * was silently dropped on save. Message mode is shown read-only, with a
 * link to the agent detail page, and neither Save nor Start
 * puts messageMode in the PATCH body. The mode changes only through the
 * set_message_mode endpoint.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/navigation.js', () => ({ navigateTo: vi.fn() }));

import { navigateTo } from '../../client/navigation.js';
import { requestBodyText, requestUrl } from '../../client/__fixtures__/request-url.js';

interface Call {
  method: string;
  url: string;
  body?: Record<string, unknown>;
}

interface ConfigureEl extends HTMLElement {
  loading: boolean;
  error: string | null;
  updateComplete: Promise<unknown>;
  handleSave(): Promise<void>;
  handleStart(): Promise<void>;
}

let calls: Call[] = [];

function json(body: unknown, status = 200): Response {
  return {
    ok: status < 400,
    status,
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as Response;
}

function stubFetch(agent: Record<string, unknown>): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      calls.push({
        method,
        url,
        body: init?.body
          ? (JSON.parse(requestBodyText(init.body)) as Record<string, unknown>)
          : undefined,
      });
      if (url.includes('/settings/public')) return Promise.resolve(json({}));
      if (method === 'PATCH') return Promise.resolve(json({ agent }));
      if (method === 'POST' && url.endsWith('/start')) return Promise.resolve(json({ agent }));
      if (method === 'GET' && url.includes('/agents/')) return Promise.resolve(json(agent));
      return Promise.resolve(json({}, 404));
    })
  );
}

function makeAgent(phase: string, messageMode?: string): Record<string, unknown> {
  return {
    id: 'agent-1',
    name: 'agent-1',
    projectId: 'project-1',
    phase,
    appliedConfig: {},
    ...(messageMode ? { messageMode } : {}),
  };
}

async function settle(el: ConfigureEl): Promise<void> {
  for (let i = 0; i < 5; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(agent: Record<string, unknown>): Promise<ConfigureEl> {
  stubFetch(agent);
  const el = document.createElement('scion-page-agent-configure') as ConfigureEl & {
    agentId: string;
  };
  el.agentId = 'agent-1';
  document.body.appendChild(el);
  const deadline = Date.now() + 2000;
  while (el.loading && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    await el.updateComplete;
  }
  await settle(el);
  calls = [];
  return el;
}

function q(el: ConfigureEl, selector: string): Element | null {
  return el.shadowRoot!.querySelector(selector);
}

/** Every sl-select whose field label reads "Message Mode". */
function messageModeSelects(el: ConfigureEl): Element[] {
  return Array.from(el.shadowRoot!.querySelectorAll('.form-field')).filter(
    (f) =>
      f.querySelector('label')?.textContent?.trim() === 'Message Mode' &&
      f.querySelector('sl-select')
  );
}

beforeAll(async () => {
  await import('./agent-configure.js');
});

beforeEach(() => {
  calls = [];
  vi.mocked(navigateTo).mockClear();
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('agent-configure Message Mode', () => {
  it('is read-only for a created agent, with a link to the detail page', async () => {
    const el = await mount(makeAgent('created', 'lineage'));
    expect(messageModeSelects(el)).toHaveLength(0);
    const field = q(el, '[data-testid="message-mode-readonly"]');
    expect(field, 'the read-only field is shown').not.toBeNull();
    expect(field!.querySelector('scion-message-mode-badge')?.getAttribute('mode')).toBe('lineage');
    const link = q(el, '[data-testid="message-mode-detail-link"]');
    expect(link?.getAttribute('href')).toBe('/agents/agent-1');
  });

  it.each(['running', 'stopped'])(
    'offers no Message Mode select for a %s agent (the form is not shown)',
    async (phase) => {
      const el = await mount(makeAgent(phase, 'lineage'));
      expect(q(el, '[data-testid="phase-notice"]')).not.toBeNull();
      expect(messageModeSelects(el)).toHaveLength(0);
    }
  );

  it('Save sends no messageMode in the PATCH body and no set_message_mode call', async () => {
    const el = await mount(makeAgent('created', 'branch'));
    await el.handleSave();
    expect(el.error).toBeNull();
    const patches = calls.filter((c) => c.method === 'PATCH');
    expect(patches).toHaveLength(1);
    expect(patches[0].url).toBe('/api/v1/agents/agent-1');
    expect(patches[0].body).not.toHaveProperty('messageMode');
    expect(calls.some((c) => c.url.includes('set_message_mode'))).toBe(false);
  });

  it('Start sends no messageMode in the PATCH body before starting', async () => {
    const el = await mount(makeAgent('created', 'branch'));
    await el.handleStart();
    expect(el.error).toBeNull();
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'PATCH /api/v1/agents/agent-1',
      'POST /api/v1/agents/agent-1/start',
    ]);
    expect(calls[0].body).not.toHaveProperty('messageMode');
    expect(navigateTo).toHaveBeenCalledWith('/agents/agent-1');
  });
});
