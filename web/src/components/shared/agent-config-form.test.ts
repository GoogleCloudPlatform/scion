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
 * <scion-agent-config-form> (ptone/scion#3972): touched-only emission,
 * clear = inherit (null), Unlimited = 0, mutability display from the hub's
 * editability, and source-labelled placeholders.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

import type { AgentEditability, AgentFieldEditState } from '../../shared/types.js';
import type { ScionAgentConfigForm } from './agent-config-form.js';

beforeAll(async () => {
  await import('./agent-config-form.js');
});

afterEach(() => {
  document.body.innerHTML = '';
});

const NOW: AgentFieldEditState = { tier: 'T1', disposition: 'now', clearNeedsReincarnate: true };

function editability(
  phase: string,
  fields: Record<string, AgentFieldEditState> = {}
): AgentEditability {
  return {
    phase,
    fields: {
      'config.model': { ...NOW, sessionSensitive: true },
      'config.max_turns': NOW,
      'config.max_duration': NOW,
      ...fields,
    },
  };
}

async function mount(props: Partial<ScionAgentConfigForm> = {}): Promise<ScionAgentConfigForm> {
  const el = document.createElement('scion-agent-config-form');
  el.editability = editability('stopped');
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function q<T extends Element = HTMLElement>(el: ScionAgentConfigForm, sel: string): T {
  const found = el.shadowRoot!.querySelector<T>(sel);
  if (!found) throw new Error(`not found: ${sel}`);
  return found;
}

function field(el: ScionAgentConfigForm, key: string): HTMLElement {
  return q(el, `.field[data-key="${key}"]`);
}

async function type(el: ScionAgentConfigForm, key: string, value: string): Promise<void> {
  const input = field(el, key).querySelector('sl-input') as HTMLInputElement;
  input.value = value;
  input.dispatchEvent(new Event('sl-input'));
  await el.updateComplete;
}

async function clear(el: ScionAgentConfigForm, key: string): Promise<void> {
  (field(el, key).querySelector('sl-button.clear') as HTMLElement).click();
  await el.updateComplete;
}

async function setUnlimited(el: ScionAgentConfigForm, key: string, on: boolean): Promise<void> {
  const box = field(el, key).querySelector('sl-checkbox') as HTMLInputElement;
  box.checked = on;
  box.dispatchEvent(new Event('sl-change'));
  await el.updateComplete;
}

describe('scion-agent-config-form emission', () => {
  it('emits nothing for an untouched form, even with stored values', async () => {
    const el = await mount({ values: { model: 'opus', max_turns: 20, max_duration: '1h' } });
    expect(el.collectConfigPatch()).toEqual({});
    expect(el.touchedKeys).toEqual([]);
  });

  it('emits only the touched field', async () => {
    const el = await mount({ values: { model: 'opus', max_turns: 20 } });
    await type(el, 'config.max_turns', '50');
    expect(el.collectConfigPatch()).toEqual({ max_turns: 50 });
    await type(el, 'config.model', ' sonnet ');
    expect(el.collectConfigPatch()).toEqual({ max_turns: 50, model: 'sonnet' });
  });

  it('counts re-entering the shown value as a touch', async () => {
    const el = await mount({ values: { max_turns: 20 } });
    await type(el, 'config.max_turns', '20');
    expect(el.collectConfigPatch()).toEqual({ max_turns: 20 });
  });

  it('sends null for Clear (inherit), never 0', async () => {
    const el = await mount({ values: { model: 'opus', max_turns: 20, max_duration: '1h' } });
    await clear(el, 'config.max_turns');
    await clear(el, 'config.model');
    await clear(el, 'config.max_duration');
    expect(el.collectConfigPatch()).toEqual({ max_turns: null, model: null, max_duration: null });
  });

  it('sends null when the user empties a field', async () => {
    const el = await mount({ values: { max_duration: '1h' } });
    await type(el, 'config.max_duration', '');
    expect(el.collectConfigPatch()).toEqual({ max_duration: null });
  });

  it('sends 0 for Unlimited, and unchecking it without typing is untouched again', async () => {
    const el = await mount({ values: { max_turns: 20 } });
    await setUnlimited(el, 'config.max_turns', true);
    expect(el.collectConfigPatch()).toEqual({ max_turns: 0 });
    expect(field(el, 'config.max_turns').querySelector('sl-input')!.hasAttribute('disabled')).toBe(
      true
    );
    await setUnlimited(el, 'config.max_turns', false);
    expect(el.collectConfigPatch()).toEqual({});
  });

  it('typing after Clear sends the typed value', async () => {
    const el = await mount();
    await clear(el, 'config.max_turns');
    await type(el, 'config.max_turns', '7');
    expect(el.collectConfigPatch()).toEqual({ max_turns: 7 });
  });

  it('reset forgets every edit and fires agent-config-change', async () => {
    const el = await mount();
    const seen: string[][] = [];
    el.addEventListener('agent-config-change', (e) =>
      seen.push((e as CustomEvent<{ touched: string[] }>).detail.touched)
    );
    await type(el, 'config.max_turns', '7');
    el.reset();
    expect(el.collectConfigPatch()).toEqual({});
    expect(seen).toEqual([['config.max_turns'], []]);
  });

  it('validates limits', async () => {
    const el = await mount();
    await type(el, 'config.max_turns', '-3');
    await type(el, 'config.max_duration', 'soon');
    expect(el.validate()).toHaveLength(2);
    await type(el, 'config.max_turns', '3');
    await type(el, 'config.max_duration', '1h30m');
    expect(el.validate()).toEqual([]);
  });
});

describe('scion-agent-config-form mutability display', () => {
  it('locks a field the hub reports locked, shows the reason, and never emits it', async () => {
    const el = await mount({
      editability: editability('running', {
        'config.max_turns': {
          ...NOW,
          disposition: 'locked',
          reason: 'Editing a running agent is not available yet.',
        },
      }),
    });
    const f = field(el, 'config.max_turns');
    expect(f.dataset.disposition).toBe('locked');
    expect(f.querySelector('sl-input')!.hasAttribute('disabled')).toBe(true);
    expect(f.querySelector('sl-button.clear')!.hasAttribute('disabled')).toBe(true);
    expect(f.querySelector('[data-testid="locked-reason"]')!.textContent).toContain(
      'Editing a running agent is not available yet.'
    );
    expect(f.querySelector('sl-icon[name="lock"]')).not.toBeNull();

    // Even a draft that reached a locked field is not sent.
    await type(el, 'config.max_turns', '9');
    expect(el.collectConfigPatch()).toEqual({});
  });

  it('locks a field with no editability entry in edit mode', async () => {
    const el = await mount({ editability: { phase: 'stopped', fields: {} } });
    expect(field(el, 'config.model').dataset.disposition).toBe('locked');
  });

  it('treats every field as editable now in create mode', async () => {
    const el = await mount({ mode: 'create', editability: null });
    for (const key of ['config.model', 'config.max_turns', 'config.max_duration']) {
      expect(field(el, key).dataset.disposition).toBe('now');
    }
    expect(el.shadowRoot!.querySelector('[data-testid="tier-summary"]')).toBeNull();
  });

  it('shows a held badge and a needs-reincarnation badge', async () => {
    const el = await mount({
      editability: editability('running', {
        'config.model': { ...NOW, disposition: 'held' },
        'config.max_turns': { ...NOW, disposition: 'reincarnate' },
      }),
    });
    expect(field(el, 'config.model').textContent).toContain('Held — applies at next start');
    expect(field(el, 'config.max_turns').textContent).toContain('Needs reincarnation');
    expect(
      field(el, 'config.model').querySelector('sl-icon[name="hourglass-split"]')
    ).not.toBeNull();
  });

  it('locks a field the harness does not support', async () => {
    const el = await mount({
      harnessCapabilities: {
        harness: 'generic',
        limits: {
          max_turns: { support: 'no', reason: 'generic has no turn limit' },
          max_model_calls: { support: 'no' },
          max_duration: { support: 'yes' },
        },
      } as never,
    });
    expect(field(el, 'config.max_turns').dataset.disposition).toBe('locked');
    expect(field(el, 'config.max_turns').textContent).toContain('generic has no turn limit');
    expect(field(el, 'config.max_duration').dataset.disposition).toBe('now');
  });

  it('says when an edit applies, by phase', async () => {
    const suspended = await mount({ editability: editability('suspended') });
    expect(field(suspended, 'config.max_turns').textContent).toContain('Applies at resume');
    document.body.innerHTML = '';

    const session = await mount({
      editability: editability('suspended', {
        'config.model': { ...NOW, sessionSensitive: true, note: 'session' },
      }),
    });
    expect(field(session, 'config.model').textContent).toContain('the conversation continues');
    document.body.innerHTML = '';

    const stopped = await mount();
    expect(field(stopped, 'config.max_turns').textContent).toContain('Applies at next start');
  });

  it('warns that a clear or Unlimited applies at the next reincarnation', async () => {
    const el = await mount();
    expect(el.shadowRoot!.querySelector('[data-testid="clear-note"]')).toBeNull();
    await clear(el, 'config.max_turns');
    expect(
      field(el, 'config.max_turns').querySelector('[data-testid="clear-note"]')
    ).not.toBeNull();
    await setUnlimited(el, 'config.max_duration', true);
    expect(
      field(el, 'config.max_duration').querySelector('[data-testid="clear-note"]')!.textContent
    ).toContain('Unlimited takes effect at the next reincarnation');
  });

  it('summarizes the fields by disposition', async () => {
    const el = await mount({
      editability: editability('running', {
        'config.model': { ...NOW, disposition: 'locked', reason: 'r' },
        'config.max_turns': { ...NOW, disposition: 'locked', reason: 'r' },
      }),
    });
    const chips = Array.from(el.shadowRoot!.querySelectorAll('[data-testid="tier-summary"] .chip'));
    expect(chips.map((c) => c.textContent!.trim().replace(/\s+/g, ' '))).toEqual([
      '1 apply at next start',
      '2 locked',
    ]);
  });

  it('groups fields by function', async () => {
    const el = await mount();
    const groups = Array.from(el.shadowRoot!.querySelectorAll('section.group')).map((g) => ({
      tab: (g as HTMLElement).dataset.tab,
      keys: Array.from(g.querySelectorAll('.field')).map((f) => (f as HTMLElement).dataset.key),
    }));
    expect(groups).toEqual([
      { tab: 'general', keys: ['config.model'] },
      { tab: 'limits', keys: ['config.max_turns', 'config.max_duration'] },
    ]);
  });
});

describe('scion-agent-config-form placeholders', () => {
  it('shows an unset field blank with a source-labelled placeholder', async () => {
    const el = await mount({
      values: {},
      placeholders: {
        'config.model': { value: 'claude-opus', source: 'resolved when the agent was created' },
        'config.max_turns': { source: 'the template or hub defaults' },
      },
    });
    const model = field(el, 'config.model').querySelector('sl-input') as HTMLInputElement;
    expect(model.value).toBe('');
    expect(model.getAttribute('placeholder')).toBe(
      'claude-opus (resolved when the agent was created)'
    );
    expect(
      field(el, 'config.max_turns').querySelector('sl-input')!.getAttribute('placeholder')
    ).toBe('Inherited from the template or hub defaults');
  });

  it('shows a stored value as the value', async () => {
    const el = await mount({ values: { max_turns: 12 } });
    expect(
      (field(el, 'config.max_turns').querySelector('sl-input') as HTMLInputElement).value
    ).toBe('12');
  });
});
