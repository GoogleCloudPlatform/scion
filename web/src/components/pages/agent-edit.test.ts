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
 * Edit agent page (ptone/scion#3972): the request bodies it sends (golden:
 * an untouched form sends no config keys, Clear sends null, Unlimited sends
 * 0, stateVersion is always sent), the start action per phase, a stale save
 * (409) offering a reload that keeps the edits, and the experiment gate.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

import type { Agent, AgentEditability, AgentFieldEditState } from '../../shared/types.js';
import type { ScionAgentConfigForm } from '../shared/agent-config-form.js';
import type { ScionPageAgentEdit } from './agent-edit.js';
import { buildAgentEditPatchBody, dispositionSummary } from './agent-edit.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

/**
 * Shared golden bodies: TestAgentEditGoldens in
 * pkg/hub/agent_config_mutability_http_test.go sends the SAME files to the
 * real PATCH handler, so a body the form emits that the hub cannot accept
 * fails there, and a change to what the form emits fails here until the
 * files are updated.
 */
function golden(name: string): Record<string, unknown> {
  const file = path.join(
    path.dirname(fileURLToPath(import.meta.url)),
    `../../../../pkg/hub/testdata/agent-edit-${name}-body.json`
  );
  return JSON.parse(readFileSync(file, 'utf-8')) as Record<string, unknown>;
}

const toasts: string[] = [];
vi.mock('../../utils/toast.js', () => ({
  showToast: (message: string) => {
    toasts.push(message);
  },
}));

const NOW: AgentFieldEditState = { tier: 'T1', disposition: 'now', clearNeedsReincarnate: true };
const RUNNING_LOCK: AgentFieldEditState = {
  tier: 'T1',
  disposition: 'locked',
  clearNeedsReincarnate: true,
  reason: 'Editing a running agent is not available yet.',
};

function editabilityFor(phase: string): AgentEditability {
  const st = phase === 'running' ? RUNNING_LOCK : NOW;
  return {
    phase,
    fields: { 'config.model': st, 'config.max_turns': st, 'config.max_duration': st },
  };
}

function makeAgent(phase: string, stateVersion: number): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 't',
    phase,
    stateVersion,
    appliedConfig: { model: 'claude-opus', inlineConfig: { max_turns: 20 } },
    editability: editabilityFor(phase),
    _capabilities: { actions: ['read', 'update', 'lifecycle'] },
  } as unknown as Agent;
}

interface Call {
  method: string;
  url: string;
  body?: Record<string, unknown>;
}

let calls: Call[] = [];
let agentResponses: Agent[] = [];
let patchStatus = 200;
let navigated: string[] = [];

function stubFetch(): void {
  calls = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      const call: Call = { method, url };
      if (typeof init?.body === 'string') call.body = JSON.parse(init.body);
      calls.push(call);
      if (method === 'PATCH') {
        if (patchStatus === 409) {
          return Promise.resolve({
            ok: false,
            status: 409,
            json: () =>
              Promise.resolve({ error: { message: 'Version conflict - resource was modified' } }),
          } as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            Promise.resolve({
              disposition: {
                applied: Object.keys(call.body?.config ?? {}).map((k) => `config.${k}`),
                held: [],
                heldForReincarnate: [],
              },
              warnings: [],
            }),
        } as Response);
      }
      if (method === 'POST') {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({}),
        } as Response);
      }
      const agent = agentResponses.length > 1 ? agentResponses.shift()! : agentResponses[0];
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve(agent),
      } as Response);
    })
  );
}

beforeAll(async () => {
  await import('./agent-edit.js');
});

beforeEach(() => {
  window.history.pushState({}, '', '/agents/a-1/edit');
  window.__SCION_FEATURES__ = { 'web.agent_edit': true };
  toasts.length = 0;
  navigated = [];
  patchStatus = 200;
  document.addEventListener('nav-click', onNav);
  stubFetch();
});

function onNav(e: Event): void {
  navigated.push((e as CustomEvent<{ path: string }>).detail.path);
}

afterEach(() => {
  document.removeEventListener('nav-click', onNav);
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
  window.__SCION_FEATURES__ = {};
});

async function flush(el: LitLike): Promise<void> {
  for (let i = 0; i < 5; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

interface LitLike extends HTMLElement {
  updateComplete: Promise<unknown>;
}

async function mount(...agents: Agent[]): Promise<ScionPageAgentEdit> {
  agentResponses = agents;
  const el = document.createElement('scion-page-agent-edit');
  document.body.appendChild(el);
  await flush(el);
  const form = formOf(el);
  if (form) await flush(form);
  return el;
}

function formOf(el: ScionPageAgentEdit): ScionAgentConfigForm | null {
  return el.shadowRoot!.querySelector('scion-agent-config-form');
}

function button(el: ScionPageAgentEdit, testid: string): HTMLElement | null {
  return el.shadowRoot!.querySelector(`[data-testid="${testid}"]`);
}

async function click(el: ScionPageAgentEdit, testid: string): Promise<void> {
  button(el, testid)!.click();
  await flush(el);
}

async function editField(
  el: ScionPageAgentEdit,
  key: string,
  action: 'clear' | 'unlimited' | { type: string }
): Promise<void> {
  const form = formOf(el)!;
  const f = form.shadowRoot!.querySelector(`.field[data-key="${key}"]`)!;
  if (action === 'clear') {
    (f.querySelector('sl-button.clear') as HTMLElement).click();
  } else if (action === 'unlimited') {
    const box = f.querySelector('sl-checkbox') as HTMLInputElement;
    box.checked = true;
    box.dispatchEvent(new Event('sl-change'));
  } else {
    const input = f.querySelector('sl-input') as HTMLInputElement;
    input.value = action.type;
    input.dispatchEvent(new Event('sl-input'));
  }
  await flush(form);
  await flush(el);
}

const patches = () => calls.filter((c) => c.method === 'PATCH');
const posts = () => calls.filter((c) => c.method === 'POST');

describe('agent edit page request bodies (golden)', () => {
  it('untouched form: Save & Start sends no PATCH, only the start', async () => {
    const el = await mount(makeAgent('stopped', 5));
    expect(button(el, 'save')!.hasAttribute('disabled')).toBe(true);
    await click(el, 'save-start');
    expect(patches()).toEqual([]);
    expect(posts().map((c) => c.url)).toEqual(['/api/v1/agents/a-1/start']);
    expect(navigated).toEqual(['/agents/a-1']);
  });

  it('Clear sends null with the stateVersion', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    await editField(el, 'config.max_turns', 'clear');
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([
      { stateVersion: 5, config: { max_turns: null } },
    ]);
    expect(toasts).toEqual(['Saved max_turns.']);
  });

  it('Unlimited sends 0', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    await editField(el, 'config.max_duration', 'unlimited');
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([
      { stateVersion: 5, config: { max_duration: '0' } },
    ]);
  });

  it('Save & Resume on a suspended agent saves the touched field, then starts', async () => {
    const el = await mount(makeAgent('suspended', 9));
    expect(button(el, 'save-start')!.textContent).toContain('Save & Resume');
    await editField(el, 'config.model', { type: 'claude-sonnet' });
    await click(el, 'save-start');
    expect(calls.filter((c) => c.method !== 'GET').map((c) => [c.method, c.body])).toEqual([
      ['PATCH', { stateVersion: 9, config: { model: 'claude-sonnet' } }],
      ['POST', undefined],
    ]);
    expect(navigated).toEqual(['/agents/a-1']);
  });

  it.each(['created', 'stopped', 'error'])('offers Save & Start for a %s agent', async (phase) => {
    const el = await mount(makeAgent(phase, 1));
    expect(button(el, 'save-start')!.textContent).toContain('Save & Start');
  });
});

describe('agent edit page shared goldens (replayed by the hub tests)', () => {
  it('untouched form', async () => {
    const el = await mount(makeAgent('stopped', 5));
    expect(buildAgentEditPatchBody(formOf(el)!.collectConfigPatch(), 5)).toEqual(
      golden('untouched')
    );
  });

  it('Clear on every field', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    await editField(el, 'config.model', 'clear');
    await editField(el, 'config.max_turns', 'clear');
    await editField(el, 'config.max_duration', 'clear');
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([golden('clear')]);
  });

  it('Unlimited on both limits', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    await editField(el, 'config.max_turns', 'unlimited');
    await editField(el, 'config.max_duration', 'unlimited');
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([golden('unlimited')]);
  });

  it('typed values', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    await editField(el, 'config.model', { type: 'claude-sonnet' });
    await editField(el, 'config.max_turns', { type: '40' });
    await editField(el, 'config.max_duration', { type: '2h' });
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([golden('typed')]);
  });
});

describe('agent edit page stale save', () => {
  it('a 409 offers a reload that keeps the edits and saves against the new version', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 8));
    await editField(el, 'config.max_turns', { type: '40' });
    patchStatus = 409;
    await click(el, 'save');
    expect(button(el, 'conflict')).not.toBeNull();
    expect(posts()).toEqual([]);

    await click(el, 'reload');
    expect(button(el, 'conflict')).toBeNull();
    expect(formOf(el)!.collectConfigPatch()).toEqual({ max_turns: 40 });

    patchStatus = 200;
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([
      { stateVersion: 5, config: { max_turns: 40 } },
      { stateVersion: 8, config: { max_turns: 40 } },
    ]);
  });

  it('a 409 on Save & Start does not start the agent', async () => {
    const el = await mount(makeAgent('stopped', 5));
    await editField(el, 'config.max_turns', { type: '40' });
    patchStatus = 409;
    await click(el, 'save-start');
    expect(posts()).toEqual([]);
    expect(navigated).toEqual([]);
    expect(button(el, 'conflict')).not.toBeNull();
  });
});

describe('agent edit page phases and gate', () => {
  it('locks the fields of a running agent and offers no start action', async () => {
    const el = await mount(makeAgent('running', 3));
    expect(button(el, 'save-start')).toBeNull();
    const form = formOf(el)!;
    for (const key of ['config.model', 'config.max_turns', 'config.max_duration']) {
      const f = form.shadowRoot!.querySelector(`.field[data-key="${key}"]`) as HTMLElement;
      expect(f.dataset.disposition).toBe('locked');
      expect(f.textContent).toContain('Editing a running agent is not available yet.');
    }
  });

  it('validation errors block the save', async () => {
    const el = await mount(makeAgent('stopped', 5));
    await editField(el, 'config.max_turns', { type: 'many' });
    await click(el, 'save');
    expect(patches()).toEqual([]);
    expect(button(el, 'error')!.textContent).toContain('Max turns');
  });

  it('renders not found, without loading, while the experiment is off', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_edit': false };
    const el = await mount(makeAgent('stopped', 5));
    expect(el.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
    expect(calls).toEqual([]);
  });
});

describe('dispositionSummary', () => {
  it('names the keys by when they take effect', () => {
    expect(
      dispositionSummary({
        applied: ['config.max_duration'],
        held: [],
        heldForReincarnate: ['config.max_turns', 'config.system_prompt'],
      })
    ).toBe(
      'Saved max_duration. Saved max_turns, system_prompt; takes effect at the next reincarnation.'
    );
  });

  it('reports a save of only reincarnation-held keys as saved', () => {
    expect(
      dispositionSummary({ applied: [], held: [], heldForReincarnate: ['config.model'] })
    ).toBe('Saved model; takes effect at the next reincarnation.');
  });

  it('names held keys', () => {
    expect(
      dispositionSummary({ applied: [], held: ['config.model'], heldForReincarnate: [] })
    ).toBe('Saved model for the next start.');
  });

  it('says nothing was saved when every list is empty or missing', () => {
    expect(dispositionSummary({ applied: [], held: [], heldForReincarnate: [] })).toBe(
      'Nothing to save.'
    );
    expect(dispositionSummary(undefined)).toBe('Nothing to save.');
  });
});
