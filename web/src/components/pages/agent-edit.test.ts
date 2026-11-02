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
 * Edit agent page (ptone/scion#3972, ptone/scion#3975): the request bodies it
 * sends (golden: an untouched form sends no config keys, Clear sends null,
 * Unlimited sends 0, stateVersion is always sent, name, labels, timezone,
 * env and task), the actions per phase in all five phases, Reincarnate…
 * for held edits, the remembered grouping, a stale save (409) offering a
 * reload that keeps the edits, and the experiment gate.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

import type { Agent } from '../../shared/types.js';
import type { ScionAgentConfigForm } from '../shared/agent-config-form.js';
import type { ScionPageAgentEdit } from './agent-edit.js';
import {
  AGENT_EDIT_GROUPING_KEY,
  agentEditTopValues,
  buildAgentEditPatchBody,
  dispositionSummary,
} from './agent-edit.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { EDIT_PHASES, hubEditability } from '../../shared/__fixtures__/agent-editability.js';
import { REINCARNATE_CONFIRM_MESSAGE } from '../../shared/agent-reincarnate.js';

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

const confirms: string[] = [];
let confirmAnswer = true;
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: (message: string) => {
    confirms.push(message);
    return Promise.resolve(confirmAnswer);
  },
}));

function makeAgent(phase: string, stateVersion: number, extra: Partial<Agent> = {}): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 't',
    phase,
    stateVersion,
    labels: { team: 'platform' },
    appliedConfig: {
      model: 'claude-opus',
      env: { FOO: 'bar' },
      // Like the hub golden's agent: model, max_turns and max_duration are
      // the agent's own values, so Clear reverts them.
      inlineConfig: { model: 'claude-opus', max_turns: 20, max_duration: '1h' },
    },
    editability: hubEditability(phase),
    _capabilities: { actions: ['read', 'update', 'lifecycle', 'attach'] },
    ...extra,
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
/** The disposition the stub hub answers a PATCH with; null applies every config key. */
let nextDisposition: Record<string, string[]> | null = null;
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
              disposition: nextDisposition ?? {
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
      if (!url.startsWith('/api/v1/agents/')) {
        // Service accounts and the runtime broker: none.
        return Promise.resolve({
          ok: false,
          status: 404,
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
  confirms.length = 0;
  confirmAnswer = true;
  localStorage.removeItem(AGENT_EDIT_GROUPING_KEY);
  navigated = [];
  patchStatus = 200;
  nextDisposition = null;
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

async function editLabel(el: ScionPageAgentEdit, index: number, value: string): Promise<void> {
  const form = formOf(el)!;
  const f = form.shadowRoot!.querySelector('.field[data-key="labels"]')!;
  const input = f.querySelectorAll('sl-input.label-value')[index] as HTMLInputElement;
  input.value = value;
  input.dispatchEvent(new Event('sl-input'));
  await flush(form);
}

async function addLabel(el: ScionPageAgentEdit, key: string, value: string): Promise<void> {
  const form = formOf(el)!;
  const f = () => form.shadowRoot!.querySelector('.field[data-key="labels"]')!;
  (f().querySelector('sl-button.add-label') as HTMLElement).click();
  await flush(form);
  const keys = f().querySelectorAll('sl-input.label-key');
  const k = keys[keys.length - 1] as HTMLInputElement;
  k.value = key;
  k.dispatchEvent(new Event('sl-input'));
  await flush(form);
  await editLabel(el, keys.length - 1, value);
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

  it('name and labels', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    await editField(el, 'name', { type: 'renamed-agent' });
    await editLabel(el, 0, 'web');
    await addLabel(el, 'tier', 'gold');
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([golden('metadata')]);
  });

  it('timezone pin', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    const form = formOf(el)!;
    form
      .shadowRoot!.querySelector('.field[data-key="explicitTimezone"] scion-timezone-picker')!
      .dispatchEvent(new CustomEvent('timezone-change', { detail: { timezone: 'Asia/Tokyo' } }));
    await flush(form);
    await flush(el);
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([golden('timezone')]);
  });

  it('an added env row sends the whole env', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    const form = formOf(el)!;
    form.shadowRoot!.querySelector('.field[data-key="config.env"] scion-env-editor')!.dispatchEvent(
      new CustomEvent('env-change', {
        detail: {
          entries: [
            { key: 'FOO', value: 'bar' },
            { key: 'NEW', value: '1' },
          ],
        },
      })
    );
    await flush(form);
    await flush(el);
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([golden('env')]);
  });

  it('an emptied next task is not an edit: Save stays disabled and nothing is sent', async () => {
    const agent = makeAgent('stopped', 5);
    agent.appliedConfig!.task = 'old task';
    const el = await mount(agent);
    const form = formOf(el)!;
    const input = form.shadowRoot!.querySelector(
      '.field[data-key="config.task"] sl-textarea'
    ) as HTMLInputElement;
    expect(input.value).toBe('old task');
    input.value = '';
    input.dispatchEvent(new Event('sl-input'));
    await flush(form);
    await flush(el);
    expect(form.touchedKeys).toEqual([]);
    expect(button(el, 'save')!.hasAttribute('disabled')).toBe(true);
    await click(el, 'save');
    expect(patches()).toEqual([]);
  });

  it('an emptied name is not an edit: Save stays disabled', async () => {
    const el = await mount(makeAgent('stopped', 5));
    await editField(el, 'name', { type: '' });
    expect(formOf(el)!.touchedKeys).toEqual([]);
    expect(button(el, 'save')!.hasAttribute('disabled')).toBe(true);
  });

  it('next task', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    const form = formOf(el)!;
    const input = form.shadowRoot!.querySelector(
      '.field[data-key="config.task"] sl-textarea'
    ) as HTMLInputElement;
    input.value = 'fix the bug';
    input.dispatchEvent(new Event('sl-input'));
    await flush(form);
    await flush(el);
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([golden('task')]);
  });
});

describe('agent edit page in every phase', () => {
  const ACTIONS: Record<string, string | null> = {
    created: 'Save & Start',
    stopped: 'Save & Start',
    error: 'Save & Start',
    suspended: 'Save & Resume',
    running: null,
  };

  it.each(EDIT_PHASES)('%s: the page renders every field with the phase action', async (phase) => {
    const el = await mount(makeAgent(phase, 2));
    const form = formOf(el)!;
    expect(form.mode).toBe('edit');
    expect(form.fieldKeys).toBeNull();
    expect(form.shadowRoot!.querySelector('.field[data-key="name"]')).not.toBeNull();
    expect(form.shadowRoot!.querySelector('.field[data-key="config.env"]')).not.toBeNull();
    const action = ACTIONS[phase];
    if (action) expect(button(el, 'save-start')!.textContent).toContain(action);
    else expect(button(el, 'save-start')).toBeNull();
    expect(button(el, 'reincarnate')).toBeNull();
  });

  it.each(EDIT_PHASES)('%s: name and labels are saved and apply immediately', async (phase) => {
    const el = await mount(makeAgent(phase, 4), makeAgent(phase, 5));
    await editField(el, 'name', { type: 'renamed-agent' });
    await editLabel(el, 0, 'web');
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([
      { stateVersion: 4, name: 'renamed-agent', labels: { team: 'web' } },
    ]);
    expect(posts()).toEqual([]);
  });

  it('locks the env fields for a caller without attach access, who cannot see the env', async () => {
    const el = await mount(
      makeAgent('stopped', 1, { _capabilities: { actions: ['read', 'update', 'lifecycle'] } })
    );
    const f = formOf(el)!.shadowRoot!.querySelector('.field[data-key="config.env"]') as HTMLElement;
    expect(f.dataset.disposition).toBe('locked');
  });

  it('has no Delete, and Back goes to the agent page', async () => {
    const el = await mount(makeAgent('stopped', 1));
    const text = el.shadowRoot!.textContent ?? '';
    expect(text).not.toContain('Delete');
    expect(el.shadowRoot!.querySelector('sl-icon[name="trash"]')).toBeNull();
    expect(button(el, 'back')!.getAttribute('href')).toBe('/agents/a-1');
  });

  it('refuses Save & Start while a required env value is empty', async () => {
    const agent = makeAgent('stopped', 1);
    agent.appliedConfig!.env = { FOO: 'bar', NEEDED: '' };
    const el = await mount(agent);
    await click(el, 'save-start');
    expect(button(el, 'error')!.textContent).toContain('NEEDED');
    expect(calls.filter((c) => c.method !== 'GET')).toEqual([]);
  });
});

describe('agent edit page Reincarnate… for held edits', () => {
  function heldAgent(phase: string): Agent {
    return makeAgent(phase, 3, {
      pendingConfig: { config: { system_prompt: 'be brief', max_turns: 9 }, savedBy: 'u' },
    });
  }

  it.each(['stopped', 'suspended', 'running'])(
    '%s: offered next to the phase action when a held prompt edit exists',
    async (phase) => {
      const el = await mount(heldAgent(phase));
      expect(button(el, 'reincarnate')!.textContent).toContain('Reincarnate…');
    }
  );

  it('the confirm names the held keys only a reincarnation applies, then reincarnates', async () => {
    const el = await mount(heldAgent('stopped'));
    await click(el, 'reincarnate');
    expect(confirms).toHaveLength(1);
    expect(confirms[0]).toContain(REINCARNATE_CONFIRM_MESSAGE);
    expect(confirms[0]).toContain('system_prompt');
    expect(confirms[0]).not.toContain('max_turns');
    expect(posts().map((c) => c.url)).toEqual(['/api/v1/agents/a-1/reincarnate']);
    expect(navigated).toEqual(['/agents/a-1']);
  });

  it('a declined confirm sends nothing', async () => {
    confirmAnswer = false;
    const el = await mount(heldAgent('stopped'));
    await click(el, 'reincarnate');
    expect(posts()).toEqual([]);
  });

  it('is not offered to a caller without the lifecycle permission', async () => {
    const agent = heldAgent('stopped');
    agent._capabilities = { actions: ['read', 'update', 'attach'] };
    const el = await mount(agent);
    expect(button(el, 'reincarnate')).toBeNull();
  });

  it('is not offered when only container-creation edits are held', async () => {
    const el = await mount(
      makeAgent('running', 3, { pendingConfig: { config: { max_turns: 9 } } })
    );
    expect(button(el, 'reincarnate')).toBeNull();
  });
});

describe('agent edit page grouping', () => {
  function grouping(el: ScionPageAgentEdit): HTMLElement & { value: string } {
    return button(el, 'grouping') as HTMLElement & { value: string };
  }

  it('defaults to by function and remembers the last choice', async () => {
    const el = await mount(makeAgent('stopped', 1));
    expect(formOf(el)!.grouping).toBe('function');
    const g = grouping(el);
    g.value = 'tier';
    g.dispatchEvent(new Event('sl-change'));
    await flush(el);
    expect(formOf(el)!.grouping).toBe('tier');
    expect(localStorage.getItem(AGENT_EDIT_GROUPING_KEY)).toBe('tier');

    document.body.innerHTML = '';
    const again = await mount(makeAgent('stopped', 1));
    expect(formOf(again)!.grouping).toBe('tier');
    expect(grouping(again).value).toBe('tier');
  });
});

describe('agent edit page Clear of a stored value', () => {
  it('clearing a previously set Max turns sends null and reports it in the toast', async () => {
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    await editField(el, 'config.max_turns', 'clear');
    const f = formOf(el)!.shadowRoot!.querySelector('.field[data-key="config.max_turns"]')!;
    // Before the save the field says when the clear takes effect.
    expect(f.querySelector('[data-testid="clear-note"]')!.textContent).toContain(
      'next reincarnation'
    );
    // The hub's answer for this body (TestAgentEditGoldens "clear").
    nextDisposition = { applied: [], held: [], heldForReincarnate: ['config.max_turns'] };
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([
      { stateVersion: 5, config: { max_turns: null } },
    ]);
    expect(toasts).toEqual(['Saved max_turns; takes effect at the next reincarnation.']);
  });

  it('a field with no stored value of its own offers no Clear, and sends nothing', async () => {
    const agent = makeAgent('stopped', 5);
    agent.appliedConfig!.inlineConfig = {};
    const el = await mount(agent);
    const f = formOf(el)!.shadowRoot!.querySelector('.field[data-key="config.max_turns"]')!;
    expect(f.querySelector('sl-button.clear')!.hasAttribute('disabled')).toBe(true);
    expect(button(el, 'save')!.hasAttribute('disabled')).toBe(true);
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

  it('experiment off: the page works, by function only, with no toggle and no strip', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_edit': false };
    localStorage.setItem(AGENT_EDIT_GROUPING_KEY, 'tier');
    const el = await mount(makeAgent('stopped', 5), makeAgent('stopped', 6));
    expect(el.shadowRoot!.querySelector('scion-page-404')).toBeNull();
    expect(button(el, 'grouping')).toBeNull();
    const form = formOf(el)!;
    expect(form.grouping).toBe('function');
    expect(form.shadowRoot!.querySelector('[data-testid="tier-summary"]')).toBeNull();
    expect(form.shadowRoot!.querySelector('section[data-section="agent"]')).not.toBeNull();
    await editField(el, 'config.max_turns', { type: '40' });
    await click(el, 'save');
    expect(patches().map((c) => c.body)).toEqual([{ stateVersion: 5, config: { max_turns: 40 } }]);
  });

  it('default (no experiment value set): the toggle and strip are shown', async () => {
    window.__SCION_FEATURES__ = {};
    const el = await mount(makeAgent('stopped', 5));
    expect(button(el, 'grouping')).not.toBeNull();
    expect(formOf(el)!.shadowRoot!.querySelector('[data-testid="tier-summary"]')).not.toBeNull();
  });

  it('experiment on: the grouping toggle and the tier summary strip', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_edit': true };
    const el = await mount(makeAgent('stopped', 5));
    expect(button(el, 'grouping')).not.toBeNull();
    expect(formOf(el)!.shadowRoot!.querySelector('[data-testid="tier-summary"]')).not.toBeNull();
  });

  it('experiment off: /agents/{id}/configure lands on a working Edit page', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_edit': false };
    const { legacyAgentRedirect } = await import('../../client/agent-routes.js');
    const target = legacyAgentRedirect('/agents/a-1/configure')!;
    window.history.pushState({}, '', target);
    const el = await mount(makeAgent('stopped', 5));
    expect(el.shadowRoot!.querySelector('scion-page-404')).toBeNull();
    expect(formOf(el)).not.toBeNull();
    expect(calls.some((c) => c.url === '/api/v1/agents/a-1')).toBe(true);
  });
});

describe('agentEditTopValues: the explicit env record', () => {
  it('keys the explicit record on CreateInputs when the agent has them', () => {
    const agent = makeAgent('stopped', 1);
    agent.appliedConfig = {
      env: { SCION_AUTO_EXPOSE_PORTS: 'true' },
      inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
      createInputs: { inlineConfig: { env: {} } },
    };
    expect(agentEditTopValues(agent).explicitEnv).toEqual({});
  });

  it('falls back to the inline env without CreateInputs', () => {
    const agent = makeAgent('stopped', 1);
    agent.appliedConfig = { inlineConfig: { env: { SCION_AUTO_EXPOSE_PORTS: 'false' } } };
    expect(agentEditTopValues(agent).explicitEnv).toEqual({ SCION_AUTO_EXPOSE_PORTS: 'false' });
  });

  it('passes the hub default and env visibility', () => {
    const v = agentEditTopValues(makeAgent('stopped', 1), true);
    expect(v.autoExposeHubDefault).toBe(true);
    expect(v.envVisible).toBe(true);
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

  it('says a pending reincarnation-held key is held as a whole', () => {
    expect(
      dispositionSummary(
        {
          applied: [],
          held: ['config.model'],
          heldForReincarnate: ['config.max_turns', 'config.skills'],
        },
        ['model', 'skills']
      )
    ).toBe(
      'Saved model for the next start. Saved skills; held as a whole until a reincarnation, including parts a start could apply. Saved max_turns; takes effect at the next reincarnation.'
    );
  });

  it('names held keys', () => {
    expect(
      dispositionSummary({ applied: [], held: ['config.model'], heldForReincarnate: [] })
    ).toBe('Saved model for the next start.');
  });

  it('says nothing was saved when every list is empty or missing', () => {
    expect(dispositionSummary({ applied: [], held: [], heldForReincarnate: [] })).toBe(
      'Saved; nothing changed.'
    );
    expect(dispositionSummary(undefined)).toBe('Saved; nothing changed.');
  });
});
