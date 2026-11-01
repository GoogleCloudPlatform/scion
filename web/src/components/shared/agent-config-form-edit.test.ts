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
 * <scion-agent-config-form> in edit mode with every field (ptone/scion#3975):
 * the two groupings partition the one field table, held and
 * reincarnate-only edits stay disabled behind SAVEABLE_DISPOSITIONS, and the
 * edit semantics of name, labels, env, auto-expose, timezone, task, the GCP
 * identity, the message mode and the fixed fields.
 */

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

import type { AgentEditability } from '../../shared/types.js';
import {
  EDIT_PHASES,
  FIXED_REASON,
  NOT_ON_RESUME_REASON,
  RUNNING_REASON,
  hubEditability,
} from '../../shared/__fixtures__/agent-editability.js';
import { GcpIdentityState, STORED_IDENTITY_HINT } from '../../shared/gcp-identity-state.js';
import type {
  AgentConfigEditValues,
  AgentConfigGrouping,
  ScionAgentConfigForm,
} from './agent-config-form.js';

let mod: typeof import('./agent-config-form.js');

beforeAll(async () => {
  mod = await import('./agent-config-form.js');
});

afterEach(() => {
  document.body.innerHTML = '';
});

const EDIT_VALUES: AgentConfigEditValues = {
  name: 'agent-1',
  labels: { team: 'a', tier: 'gold' },
  messageMode: 'project',
  agentRole: 'baseline',
  env: {
    FOO: 'bar',
    NEEDED: '',
    TZ: 'Europe/Paris',
    SCION_AUTO_EXPOSE_PORTS: 'true',
    SCION_AUTO_EXPOSE_MODE: 'denylist',
    SCION_AUTO_EXPOSE_PORTS_LIST: '22',
    SCION_AUTO_EXPOSE_INTERVAL: '5s',
  },
  detailHref: '/agents/a-1',
  fixed: {
    projectId: 'proj',
    template: 'tmpl',
    harnessConfig: 'claude',
    runtimeBrokerId: 'broker-1',
    profile: '',
    branch: 'scion/agent-1',
  },
};

async function mount(
  phase: string,
  props: Partial<ScionAgentConfigForm> = {},
  editability: AgentEditability = hubEditability(phase)
): Promise<ScionAgentConfigForm> {
  const el = document.createElement('scion-agent-config-form');
  el.mode = 'edit';
  el.editability = editability;
  el.editValues = EDIT_VALUES;
  el.gcpIdentity = new GcpIdentityState();
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function field(el: ScionAgentConfigForm, key: string): HTMLElement {
  const found = el.shadowRoot!.querySelector<HTMLElement>(`.field[data-key="${key}"]`);
  if (!found) throw new Error(`no field ${key}`);
  return found;
}

async function typeInto(el: ScionAgentConfigForm, key: string, value: string): Promise<void> {
  const input = field(el, key).querySelector('sl-input, sl-textarea') as HTMLInputElement;
  input.value = value;
  input.dispatchEvent(new Event('sl-input'));
  await el.updateComplete;
}

async function clickClear(el: ScionAgentConfigForm, key: string): Promise<void> {
  (field(el, key).querySelector('sl-button.clear') as HTMLElement).click();
  await el.updateComplete;
}

/** The keys of the top-level fields in each rendered section, by section id. */
function sectionKeys(el: ScionAgentConfigForm): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const section of el.shadowRoot!.querySelectorAll<HTMLElement>('section.group')) {
    const keys = Array.from(section.querySelectorAll<HTMLElement>('.field'))
      .map((f) => f.dataset.key!)
      // The service-account picker is part of the GCP identity field.
      .filter((k) => k !== 'gcp_identity.service_account');
    out.set(section.dataset.section!, keys);
  }
  return out;
}

describe('the two groupings partition the field table', () => {
  const groupings: AgentConfigGrouping[] = ['function', 'tier'];
  for (const phase of EDIT_PHASES) {
    for (const grouping of groupings) {
      it(`${phase}, by ${grouping}: every field is in exactly one section`, async () => {
        const el = await mount(phase, { grouping });
        const rendered = el.fields.map((f) => f.key);
        // Every field of the table renders in edit mode (the identity picker
        // is given), so the partition covers the whole table.
        expect(rendered.sort()).toEqual(mod.AGENT_CONFIG_FIELDS.map((f) => f.key).sort());

        const all = Array.from(sectionKeys(el).values()).flat();
        expect(all.sort()).toEqual([...rendered].sort());
        expect(new Set(all).size).toBe(all.length);

        // The component's own partition agrees with the DOM.
        const fromSections = el.sections.flatMap((s) => s.fields.map((f) => f.key));
        expect(fromSections.sort()).toEqual([...rendered].sort());
      });
    }
  }

  it('by function: the Agent group first, then the create form tabs', async () => {
    const el = await mount('stopped', { grouping: 'function' });
    expect(Array.from(sectionKeys(el).keys())).toEqual([
      'agent',
      'general',
      'auth',
      'prompts',
      'limits',
      'environment',
    ]);
    expect(sectionKeys(el).get('agent')).toEqual([
      'name',
      'config.task',
      'projectId',
      'template',
      'harnessConfig',
      'runtimeBrokerId',
      'profile',
    ]);
    expect(sectionKeys(el).get('general')).toContain('explicitTimezone');
    expect(sectionKeys(el).get('general')).toContain('branch');
  });

  it.each([
    ['created', 'Applies at next start'],
    ['stopped', 'Applies at next start'],
    ['error', 'Applies at next start'],
    ['suspended', 'Applies at resume'],
    ['running', 'Held until the next start'],
  ])('by tier, %s: section titles say what the phase does', async (phase, container) => {
    const el = await mount(phase, { grouping: 'tier' });
    const titles = Array.from(el.shadowRoot!.querySelectorAll('section.group h3')).map((h) =>
      h.textContent!.trim()
    );
    expect(titles).toEqual([container, 'Needs reincarnation', 'Takes effect immediately', 'Fixed']);
  });

  it('by tier: the identity fields are sub-headed in Needs reincarnation', async () => {
    const el = await mount('stopped', { grouping: 'tier' });
    const section = el.shadowRoot!.querySelector('section[data-section="reincarnate"]')!;
    const subhead = section.querySelector('[data-testid="identity-subhead"]')!;
    expect(subhead.textContent).toContain('Changes identity');
    const after: string[] = [];
    for (let n = subhead.nextElementSibling; n; n = n.nextElementSibling) {
      if (n.matches('.field')) after.push((n as HTMLElement).dataset.key!);
    }
    expect(after).toEqual(['agentRole', 'gcp_identity']);
    expect(sectionKeys(el).get('reincarnate')).toEqual([
      'config.system_prompt',
      'config.agent_instructions',
      'agentRole',
      'gcp_identity',
    ]);
  });

  it("by tier: a created agent's GCP identity applies at the next start", async () => {
    const el = await mount('created', { grouping: 'tier' });
    expect(sectionKeys(el).get('container')).toContain('gcp_identity');
    expect(sectionKeys(el).get('reincarnate')).not.toContain('gcp_identity');
  });

  it('the grouping changes neither what is emitted nor the touched state', async () => {
    const el = await mount('stopped', { grouping: 'function' });
    await typeInto(el, 'config.max_turns', '7');
    el.grouping = 'tier';
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({ max_turns: 7 });
    expect(field(el, 'config.max_turns').querySelector('sl-input')!).toHaveProperty('value', '7');
  });
});

describe('every field shows a mutability indicator in every phase', () => {
  for (const phase of EDIT_PHASES) {
    it(`${phase}: each rendered field has a non-empty status`, async () => {
      const el = await mount(phase);
      const fields = Array.from(
        el.shadowRoot!.querySelectorAll<HTMLElement>('.field[data-key]')
      ).filter(
        // The service-account picker is part of the GCP identity field.
        (f) => f.dataset.key !== 'gcp_identity.service_account'
      );
      expect(fields.map((f) => f.dataset.key).sort()).toEqual(el.fields.map((f) => f.key).sort());
      const missing = fields
        .filter((f) => !(f.querySelector('.status')?.textContent ?? '').trim())
        .map((f) => f.dataset.key);
      expect(missing).toEqual([]);
    });
  }

  it("a created agent's GCP identity says when it applies", async () => {
    const el = await mount('created');
    expect(field(el, 'gcp_identity').querySelector('.status')!.textContent).toContain(
      'Applies at first start'
    );
  });
});

describe('held and reincarnate-only edits stay disabled (one switch)', () => {
  it('only now and immediate can be saved', () => {
    expect([...mod.SAVEABLE_DISPOSITIONS].sort()).toEqual(['immediate', 'now']);
  });

  it.each(['stopped', 'suspended', 'error'])(
    '%s: prompts and identity render their tier badge, disabled, and are never sent',
    async (phase) => {
      const el = await mount(phase);
      for (const key of ['config.system_prompt', 'config.agent_instructions', 'agentRole']) {
        const f = field(el, key);
        expect(f.dataset.disposition).toBe('reincarnate');
        expect(f.querySelector('[data-testid="reincarnate-badge"]')!.textContent).toContain(
          'Needs reincarnation'
        );
        expect(f.querySelector('[data-testid="not-saveable"]')).not.toBeNull();
        expect(f.querySelector('sl-textarea, sl-select')!.hasAttribute('disabled')).toBe(true);
      }
      expect(
        field(el, 'agentRole').querySelector('[data-testid="reincarnate-badge"]')!.textContent
      ).toContain('changes identity');
      const gcp = field(el, 'gcp_identity');
      expect(gcp.dataset.disposition).toBe('reincarnate');
      expect(gcp.querySelector('sl-select')!.hasAttribute('disabled')).toBe(true);

      // Even a forced input event is not emitted.
      await typeInto(el, 'config.system_prompt', 'be brief');
      el.gcpIdentity!.pickMode('passthrough');
      expect(el.collectConfigPatch()).toEqual({});
      expect(el.collectTopLevel()).toEqual({});
    }
  );

  it('a held disposition renders its badge, disabled, and is never sent', async () => {
    const el = await mount(
      'running',
      {},
      hubEditability('running', {
        'config.max_turns': { tier: 'T1', disposition: 'held' },
      })
    );
    const f = field(el, 'config.max_turns');
    expect(f.textContent).toContain('Held — applies at next start');
    expect(f.querySelector('[data-testid="not-saveable"]')).not.toBeNull();
    await typeInto(el, 'config.max_turns', '9');
    expect(el.collectConfigPatch()).toEqual({});
  });

  it('running: container fields are locked with the reason; name stays editable', async () => {
    const el = await mount('running');
    for (const key of ['config.model', 'config.env', 'config.task', 'autoExpose']) {
      expect(field(el, key).textContent).toContain(RUNNING_REASON);
    }
    expect(field(el, 'config.env').querySelector('[data-testid="env-readonly"]')).not.toBeNull();
    expect(field(el, 'name').querySelector('sl-input')!.hasAttribute('disabled')).toBe(false);
  });
});

describe('name and labels apply immediately in every phase', () => {
  for (const phase of EDIT_PHASES) {
    it(`${phase}: a renamed agent and edited labels are sent as top-level keys`, async () => {
      const el = await mount(phase);
      expect(field(el, 'name').dataset.disposition).toBe('immediate');
      expect(field(el, 'labels').dataset.disposition).toBe('immediate');
      await typeInto(el, 'name', '  renamed ');
      const value = field(el, 'labels').querySelectorAll(
        'sl-input.label-value'
      )[0] as HTMLInputElement;
      value.value = 'b';
      value.dispatchEvent(new Event('sl-input'));
      await el.updateComplete;
      expect(el.collectTopLevel()).toEqual({
        name: 'renamed',
        labels: { team: 'b', tier: 'gold' },
      });
      expect(el.collectConfigPatch()).toEqual({});
    });
  }

  it('says name and labels take effect immediately, not at the next start', async () => {
    const el = await mount('stopped');
    for (const key of ['name', 'labels']) {
      const text = field(el, key).querySelector('.status')!.textContent!;
      expect(text).toContain('Takes effect immediately');
      expect(text).not.toContain('next start');
    }
  });

  it('shows the stored name and labels, and an untouched form sends neither', async () => {
    const el = await mount('stopped');
    expect(field(el, 'name').querySelector('sl-input')!).toHaveProperty('value', 'agent-1');
    expect(field(el, 'labels').querySelectorAll('.label-row')).toHaveLength(2);
    expect(el.collectTopLevel()).toEqual({});
  });

  it('removing every label sends an empty map', async () => {
    const el = await mount('stopped');
    for (let i = 0; i < 2; i++) {
      (field(el, 'labels').querySelector('sl-icon-button') as HTMLElement).click();
      await el.updateComplete;
    }
    expect(el.collectTopLevel()).toEqual({ labels: {} });
  });

  it('an emptied name is not an edit: no Clear, nothing sent, no error', async () => {
    const el = await mount('stopped');
    expect(field(el, 'name').querySelector('sl-button.clear')).toBeNull();
    await typeInto(el, 'name', '  ');
    expect(el.touchedKeys).not.toContain('name');
    expect(el.collectTopLevel()).toEqual({});
    expect(el.validate()).toEqual([]);
  });
});

describe('environment in edit mode', () => {
  it('shows the stored rows without the auto-expose keys and TZ', async () => {
    const el = await mount('stopped');
    const editor = field(el, 'config.env').querySelector('scion-env-editor') as HTMLElement & {
      entries: Array<{ key: string; value: string }>;
      requiredKeys: string[];
    };
    expect(editor.entries).toEqual([
      { key: 'FOO', value: 'bar' },
      { key: 'NEEDED', value: '' },
    ]);
    expect(editor.requiredKeys).toEqual(['NEEDED']);
    expect(el.missingRequiredEnv()).toEqual(['NEEDED']);
  });

  it('says that saving replaces the stored environment', async () => {
    const el = await mount('stopped');
    expect(
      field(el, 'config.env').querySelector('[data-testid="env-replace-note"]')!.textContent
    ).toContain('replaces the agent');
  });

  it('an untouched env sends nothing', async () => {
    const el = await mount('stopped');
    expect(el.collectConfigPatch()).toEqual({});
  });

  it('a row edit sends the whole env, without TZ or untouched auto-expose keys', async () => {
    const el = await mount('stopped');
    const editor = field(el, 'config.env').querySelector('scion-env-editor')!;
    editor.dispatchEvent(
      new CustomEvent('env-change', {
        detail: {
          entries: [
            { key: 'FOO', value: 'bar' },
            { key: 'NEEDED', value: 'x' },
            { key: 'TZ', value: 'Asia/Tokyo' },
            { key: 'NEW', value: '1' },
          ],
        },
      })
    );
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({ env: { FOO: 'bar', NEEDED: 'x', NEW: '1' } });
    expect(el.missingRequiredEnv()).toEqual([]);
  });

  it('the auto-expose control shows an explicit value; using it sends the keys with the stored rows', async () => {
    const el = await mount('stopped', {
      editValues: { ...EDIT_VALUES, explicitEnv: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
    });
    const select = field(el, 'autoExpose').querySelector('sl-select') as HTMLElement & {
      value: string;
    };
    expect(select.value).toBe('true');
    const list = field(el, 'autoExpose').querySelector('sl-input.ae-list') as HTMLInputElement;
    expect(list.value).toBe('22');
    list.value = '22,8080';
    list.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({
      env: {
        FOO: 'bar',
        NEEDED: '',
        SCION_AUTO_EXPOSE_PORTS: 'true',
        SCION_AUTO_EXPOSE_MODE: 'denylist',
        SCION_AUTO_EXPOSE_PORTS_LIST: '22,8080',
        SCION_AUTO_EXPOSE_INTERVAL: '5s',
      },
    });
  });
});

describe('environment hidden from the caller', () => {
  it('locks env and auto-expose with the reason, and sends neither', async () => {
    const el = await mount('stopped', {
      editValues: { ...EDIT_VALUES, env: undefined, envVisible: false },
    });
    for (const key of ['config.env', 'autoExpose']) {
      expect(field(el, key).dataset.disposition).toBe('locked');
      expect(field(el, key).textContent).toContain(mod.ENV_HIDDEN_REASON);
    }
    expect(field(el, 'config.env').querySelector('scion-env-editor')).toBeNull();
    expect(
      field(el, 'config.env').querySelector('[data-testid="env-hidden"]')!.textContent
    ).toContain('hidden from you');
    expect(field(el, 'config.env').textContent).not.toContain('No environment variables');
    expect(el.collectConfigPatch()).toEqual({});
  });
});

describe('effectiveAutoExposePorts (ported from the Configure page, ptone/scion#2562 AC7)', () => {
  it('labels a value in the explicit record as explicit', () => {
    expect(
      mod.effectiveAutoExposePorts(
        { SCION_AUTO_EXPOSE_PORTS: 'false' },
        { SCION_AUTO_EXPOSE_PORTS: 'false' },
        true
      )
    ).toEqual({ enabled: false, source: 'explicit' });
  });

  it('labels a value only in the live env as project/template', () => {
    expect(mod.effectiveAutoExposePorts({ SCION_AUTO_EXPOSE_PORTS: 'true' }, {}, false)).toEqual({
      enabled: true,
      source: 'project/template',
    });
  });

  it('reports the hub default as inherited when the live env lacks the key, ignoring the explicit record', () => {
    expect(mod.effectiveAutoExposePorts({}, { SCION_AUTO_EXPOSE_PORTS: 'false' }, true)).toEqual({
      enabled: true,
      source: 'inherited',
    });
    expect(mod.effectiveAutoExposePorts(undefined, undefined, false)).toEqual({
      enabled: false,
      source: 'inherited',
    });
  });
});

describe('auto-expose source and inheritance in edit mode', () => {
  const select = (el: ScionAgentConfigForm) =>
    field(el, 'autoExpose').querySelector('sl-select') as HTMLElement & { value: string };
  const source = (el: ScionAgentConfigForm) =>
    field(el, 'autoExpose').querySelector('[data-testid="auto-expose-source"]')!.textContent!;

  it('a project/template value starts blank with a labelled placeholder; an untouched save sends no env', async () => {
    const el = await mount('stopped'); // env has the key, the explicit record does not
    expect(select(el).value).toBe('');
    expect(select(el).getAttribute('placeholder')).toBe('Enabled (project/template)');
    expect(source(el)).toContain('project/template');
    expect(el.collectConfigPatch()).toEqual({});
  });

  it('an inherited value shows the hub default, labelled inherited', async () => {
    const el = await mount('stopped', {
      editValues: { ...EDIT_VALUES, env: { FOO: 'bar' }, autoExposeHubDefault: true },
    });
    expect(select(el).value).toBe('');
    expect(select(el).getAttribute('placeholder')).toBe(
      'Enabled (inherited (hub default shown; template may override))'
    );
    expect(source(el)).toContain('inherited (hub default shown; template may override)');
  });

  it('an explicit value is the control value, labelled explicit', async () => {
    const el = await mount('stopped', {
      editValues: { ...EDIT_VALUES, explicitEnv: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
    });
    expect(select(el).value).toBe('true');
    expect(source(el)).toContain('explicit');
  });

  it('the source label switches to explicit (unsaved) once the control changes', async () => {
    const el = await mount('stopped');
    const sel = select(el);
    sel.value = 'false';
    sel.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(source(el)).toContain('explicit (unsaved)');
  });

  it('picking the inherited value shown as the placeholder is a change (no same-value event needed)', async () => {
    // Real Shoelace fires no sl-change when the shown value is re-picked;
    // the control starts blank, so picking "Enabled" changes it from "".
    const el = await mount('stopped');
    expect(select(el).value).toBe('');
    const sel = select(el);
    sel.value = 'true';
    sel.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({
      env: {
        FOO: 'bar',
        NEEDED: '',
        SCION_AUTO_EXPOSE_PORTS: 'true',
        SCION_AUTO_EXPOSE_MODE: 'denylist',
        SCION_AUTO_EXPOSE_PORTS_LIST: '22',
        SCION_AUTO_EXPOSE_INTERVAL: '5s',
      },
    });
  });

  it("clearing the list sends LIST='' so it replaces the previous one", async () => {
    const el = await mount('stopped', {
      editValues: { ...EDIT_VALUES, explicitEnv: { SCION_AUTO_EXPOSE_PORTS: 'true' } },
    });
    const list = field(el, 'autoExpose').querySelector('sl-input.ae-list') as HTMLInputElement;
    list.value = '';
    list.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(el.collectConfigPatch().env?.SCION_AUTO_EXPOSE_PORTS_LIST).toBe('');
  });

  it('Undo (not Clear) discards the edit in edit mode', async () => {
    const el = await mount('stopped');
    const f = field(el, 'autoExpose');
    expect(f.querySelector('sl-button.clear')).toBeNull();
    const sel = select(el);
    sel.value = 'false';
    sel.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    const undo = field(el, 'autoExpose').querySelector('sl-button.undo') as HTMLElement;
    expect(undo.textContent).toContain('Undo');
    undo.click();
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({});
  });
});

/**
 * Edit-mode Clear, swept over the whole field table against what the hub's
 * PATCH does with an empty value (ptone/scion#3975):
 * - clear-null / clear-set / clear-empty: the hub records the clear (it
 *   reverts to the inherited value, or reports it as taking effect at the
 *   next reincarnation), so Clear is offered and sends null ("" for the
 *   timezone pin, which unpins);
 * - replace-only: the hub ignores an empty value (name; the task a start
 *   sends), so no Clear, and an emptied control is not an edit;
 * - undo: the control's button only discards the user's edit;
 * - editor: rows are removed in the editor and the whole map is sent;
 * - not-saveable: fixed, read-only, or disabled for this phase.
 */
const EDIT_CLEAR_SWEEP: Record<
  string,
  'clear-null' | 'clear-set' | 'clear-empty' | 'replace-only' | 'undo' | 'editor' | 'not-saveable'
> = {
  name: 'replace-only',
  'config.task': 'replace-only',
  projectId: 'not-saveable',
  template: 'not-saveable',
  harnessConfig: 'not-saveable',
  runtimeBrokerId: 'not-saveable',
  profile: 'not-saveable',
  branch: 'not-saveable',
  'config.model': 'clear-null',
  'config.thinking_level': 'clear-set',
  'config.image': 'clear-null',
  'config.user': 'clear-null',
  'config.telemetry': 'clear-null',
  autoExpose: 'undo',
  explicitTimezone: 'clear-empty',
  agentRole: 'not-saveable',
  messageMode: 'not-saveable',
  'config.auth_selectedType': 'clear-null',
  gcp_identity: 'not-saveable',
  'config.system_prompt': 'not-saveable',
  'config.agent_instructions': 'not-saveable',
  'config.max_turns': 'clear-null',
  'config.max_model_calls': 'clear-null',
  'config.max_duration': 'clear-null',
  'config.resources': 'undo',
  'config.env': 'editor',
  labels: 'editor',
};

describe('edit-mode Clear matches what the hub does with an empty value (field table sweep)', () => {
  const STORED = {
    task: 'old task',
    model: 'm',
    image: 'img',
    user: 'u',
    auth_selectedType: 'api-key',
    max_turns: 5,
    max_model_calls: 6,
    max_duration: '1h',
    telemetry: { enabled: true },
    thinking_level: 40,
    resources: { requests: { cpu: '1' } },
  };
  const mountStored = () =>
    mount('stopped', {
      values: STORED,
      editValues: {
        ...EDIT_VALUES,
        explicitTimezone: 'Europe/Berlin',
        explicitEnv: { SCION_AUTO_EXPOSE_PORTS: 'true' },
      },
    });

  it('classifies every field of the table', () => {
    expect(Object.keys(EDIT_CLEAR_SWEEP).sort()).toEqual(
      mod.AGENT_CONFIG_FIELDS.map((f) => f.key).sort()
    );
  });

  for (const [key, kind] of Object.entries(EDIT_CLEAR_SWEEP)) {
    it(`${key}: ${kind}`, async () => {
      const el = await mountStored();
      const f = () => field(el, key);
      const clearBtn = () => f().querySelector('sl-button.clear') as HTMLElement | null;
      const def = mod.AGENT_CONFIG_FIELDS.find((d) => d.key === key)!;
      switch (kind) {
        case 'clear-null':
        case 'clear-empty': {
          expect(el.fieldSaveable(def)).toBe(true);
          expect(clearBtn()).not.toBeNull();
          expect(clearBtn()!.hasAttribute('disabled')).toBe(false);
          clearBtn()!.click();
          await el.updateComplete;
          if (kind === 'clear-empty') {
            expect(el.collectTopLevel()).toEqual({ explicitTimezone: '' });
          } else {
            const configKey = key.replace(/^config\./, '');
            expect(el.collectConfigPatch()).toEqual({ [configKey]: null });
          }
          break;
        }
        case 'clear-set': {
          const box = f().querySelector('sl-checkbox.set') as HTMLInputElement;
          expect(box.hasAttribute('checked')).toBe(true);
          box.checked = false;
          box.dispatchEvent(new Event('sl-change'));
          await el.updateComplete;
          expect(el.collectConfigPatch()).toEqual({ thinking_level: null });
          break;
        }
        case 'replace-only': {
          expect(el.fieldSaveable(def)).toBe(true);
          expect(clearBtn()).toBeNull();
          await typeInto(el, key, '');
          expect(el.touchedKeys).not.toContain(key);
          expect(el.collectConfigPatch()).toEqual({});
          expect(el.collectTopLevel()).toEqual({});
          break;
        }
        case 'undo': {
          expect(clearBtn()).toBeNull();
          if (key === 'autoExpose') {
            const sel = f().querySelector('sl-select') as HTMLElement & { value: string };
            sel.value = 'false';
            sel.dispatchEvent(new Event('sl-change'));
          } else {
            const input = f().querySelector('.sub sl-input') as HTMLInputElement;
            input.value = '2';
            input.dispatchEvent(new Event('sl-input'));
          }
          await el.updateComplete;
          expect(clearBtn()).toBeNull();
          const undo = f().querySelector('sl-button.undo') as HTMLElement;
          expect(undo.textContent).toContain('Undo');
          undo.click();
          await el.updateComplete;
          expect(el.touchedKeys).not.toContain(key);
          break;
        }
        case 'editor':
          expect(el.fieldSaveable(def)).toBe(true);
          expect(clearBtn()).toBeNull();
          break;
        case 'not-saveable':
          expect(el.fieldSaveable(def)).toBe(false);
          expect(clearBtn() === null || clearBtn()!.hasAttribute('disabled')).toBe(true);
          break;
      }
    });
  }
});

describe('Resources: removing a stored value', () => {
  const sub = (el: ScionAgentConfigForm, name: string) =>
    field(el, 'config.resources').querySelector(
      `.sub[data-sub="${name}"] sl-input`
    ) as HTMLInputElement;
  async function setSub(el: ScionAgentConfigForm, name: string, value: string) {
    const input = sub(el, name);
    input.value = value;
    input.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
  }
  const note = (el: ScionAgentConfigForm) =>
    field(el, 'config.resources').querySelector('[data-testid="clear-note"]');
  const stored = { resources: { requests: { cpu: '1', memory: '2Gi' }, limits: { cpu: '4' } } };

  it('emptying one stored value says the removal waits for a reincarnation', async () => {
    const el = await mount('stopped', { values: stored });
    expect(note(el)).toBeNull();
    await setSub(el, 'limits.cpu', '');
    expect(note(el)!.textContent).toContain('next reincarnation');
    expect(el.collectConfigPatch()).toEqual({
      resources: { requests: { cpu: '1', memory: '2Gi' } },
    });
  });

  it('emptying every stored value sends null and says the same', async () => {
    const el = await mount('stopped', { values: stored });
    for (const name of ['requests.cpu', 'requests.memory', 'limits.cpu'])
      await setSub(el, name, '');
    expect(el.collectConfigPatch()).toEqual({ resources: null });
    expect(note(el)!.textContent).toContain('next reincarnation');
  });

  it('a held removal says the whole resources edit waits for a reincarnation', async () => {
    const el = await mount('stopped', { values: stored });
    await setSub(el, 'limits.cpu', '');
    // The agent starts running: the drafted edit is now a held edit.
    el.editability = hubEditability('running', {
      'config.resources': { tier: 'T1', disposition: 'held' },
    });
    await el.updateComplete;
    expect(field(el, 'config.resources').dataset.disposition).toBe('held');
    expect(note(el)!.textContent).toContain('holds the whole resources edit until a reincarnation');
    expect(note(el)!.textContent).not.toContain('next start');
  });

  it('changing or adding a value shows no note', async () => {
    const el = await mount('stopped', { values: stored });
    await setSub(el, 'limits.cpu', '8');
    await setSub(el, 'disk', '20Gi');
    expect(note(el)).toBeNull();
  });
});

describe('Clear on a field that only inherits', () => {
  it('is disabled until the user types, then drops the edit instead of sending null', async () => {
    const el = await mount('stopped', { values: {} });
    const clearBtn = () =>
      field(el, 'config.max_turns').querySelector('sl-button.clear') as HTMLElement;
    expect(clearBtn().hasAttribute('disabled')).toBe(true);
    await typeInto(el, 'config.max_turns', '5');
    expect(clearBtn().hasAttribute('disabled')).toBe(false);
    clearBtn().click();
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({});
    expect(el.touchedKeys).not.toContain('config.max_turns');
  });

  it('a stored value is cleared with null (inherit)', async () => {
    const el = await mount('stopped', { values: { max_turns: 4 } });
    await clickClear(el, 'config.max_turns');
    expect(el.collectConfigPatch()).toEqual({ max_turns: null });
  });
});

describe('fields that are never sent from edit mode', () => {
  type WithDrafts = { drafts: Record<string, unknown> };
  const draft = { typed: true, text: 'x', cleared: false, unlimited: false };

  it('never sends branch, even when the form state holds a value (ptone/scion#3984)', async () => {
    const el = await mount('created');
    (el as unknown as WithDrafts).drafts = { branch: { ...draft, text: 'other-branch' } };
    await el.updateComplete;
    expect(el.touchedKeys).toContain('branch');
    expect(el.collectTopLevel()).toEqual({});
  });

  it('never sends messageMode, even when the form state holds a value', async () => {
    const el = await mount('stopped');
    (el as unknown as WithDrafts).drafts = { messageMode: { ...draft, text: 'hub' } };
    await el.updateComplete;
    expect(el.touchedKeys).toContain('messageMode');
    expect(el.collectTopLevel()).toEqual({});
  });

  it('sends telemetry only after the user toggles it', async () => {
    const el = await mount('stopped', { values: { telemetry: { enabled: true } } });
    expect(el.collectConfigPatch()).toEqual({});
    const sel = field(el, 'config.telemetry').querySelector('sl-select') as HTMLElement & {
      value: string;
    };
    expect(sel.value).toBe('true');
    sel.value = 'false';
    sel.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({ telemetry: { enabled: false } });
  });
});

describe('timezone in edit mode', () => {
  function picker(el: ScionAgentConfigForm): HTMLElement & { value: string } {
    return field(el, 'explicitTimezone').querySelector('scion-timezone-picker') as HTMLElement & {
      value: string;
    };
  }

  async function pick(el: ScionAgentConfigForm, zone: string): Promise<void> {
    picker(el).dispatchEvent(new CustomEvent('timezone-change', { detail: { timezone: zone } }));
    await el.updateComplete;
  }

  it.each(EDIT_PHASES)(
    '%s: a picked zone is saved with Save as explicitTimezone',
    async (phase) => {
      const el = await mount(phase);
      expect(field(el, 'explicitTimezone').dataset.disposition).toBe('now');
      await pick(el, 'Asia/Tokyo');
      expect(el.collectTopLevel()).toEqual({ explicitTimezone: 'Asia/Tokyo' });
    }
  );

  it('shows the pin; Clear unpins with ""', async () => {
    const el = await mount('stopped', {
      editValues: { ...EDIT_VALUES, explicitTimezone: 'Europe/Berlin' },
    });
    expect(picker(el).value).toBe('Europe/Berlin');
    await clickClear(el, 'explicitTimezone');
    expect(el.collectTopLevel()).toEqual({ explicitTimezone: '' });
  });

  it('an unpinned agent shows how the zone is resolved, and nothing to clear', async () => {
    const el = await mount('stopped');
    expect(picker(el).value).toBe('');
    expect(picker(el).getAttribute('placeholder')).toContain('Not pinned');
    expect(
      field(el, 'explicitTimezone').querySelector('sl-button.clear')!.hasAttribute('disabled')
    ).toBe(true);
  });

  it('labels a pin adopted from an earlier TZ setting', async () => {
    const el = await mount('running', {
      editValues: {
        ...EDIT_VALUES,
        explicitTimezone: 'Europe/Berlin',
        explicitTimezoneLegacy: true,
      },
    });
    expect(
      field(el, 'explicitTimezone').querySelector('[data-testid="timezone-legacy"]')
    ).not.toBeNull();
  });

  it('an invalid zone is a validation error', async () => {
    const el = await mount('stopped');
    await pick(el, 'Mars/Olympus');
    expect(el.validate()).toEqual(['"Mars/Olympus" is not a valid IANA timezone name.']);
  });
});

describe('next task', () => {
  it('is labelled Next task and sent as config.task', async () => {
    const el = await mount('stopped');
    expect(field(el, 'config.task').querySelector('label')!.textContent).toContain('Next task');
    expect(field(el, 'config.task').textContent).toContain('a resume continues the conversation');
    await typeInto(el, 'config.task', 'fix the bug');
    expect(el.collectConfigPatch()).toEqual({ task: 'fix the bug' });
  });

  it('can be replaced but not cleared: no Clear, and an emptied task sends nothing', async () => {
    const el = await mount('stopped', { values: { task: 'old task' } });
    const f = field(el, 'config.task');
    expect(f.querySelector('sl-button.clear')).toBeNull();
    expect(f.textContent).toContain('It can be replaced, not emptied.');
    await typeInto(el, 'config.task', '');
    expect(el.collectConfigPatch()).toEqual({});
    await typeInto(el, 'config.task', 'new task');
    expect(el.collectConfigPatch()).toEqual({ task: 'new task' });
  });

  it('is marked not applied on resume for a suspended agent', async () => {
    const el = await mount('suspended');
    expect(field(el, 'config.task').dataset.disposition).toBe('locked');
    expect(field(el, 'config.task').textContent).toContain(NOT_ON_RESUME_REASON);
  });
});

describe('fixed fields and the message mode', () => {
  it('shows the fixed values read-only with the reason, and never sends them', async () => {
    const el = await mount('stopped');
    for (const [key, value] of [
      ['projectId', 'proj'],
      ['template', 'tmpl'],
      ['harnessConfig', 'claude'],
      ['runtimeBrokerId', 'broker-1'],
      ['branch', 'scion/agent-1'],
    ]) {
      const input = field(el, key).querySelector('sl-input')!;
      expect(input.hasAttribute('readonly')).toBe(true);
      expect(input).toHaveProperty('value', value);
      expect(field(el, key).textContent).toContain(FIXED_REASON);
    }
    expect(el.collectTopLevel()).toEqual({});
  });

  it('shows the message mode read-only with a link to the Messaging card', async () => {
    const el = await mount('stopped');
    const f = field(el, 'messageMode');
    expect(f.querySelector('sl-select')).toBeNull();
    expect(f.querySelector('scion-message-mode-badge')!.getAttribute('mode')).toBe('project');
    expect(f.querySelector('[data-testid="message-mode-detail-link"]')!.getAttribute('href')).toBe(
      '/agents/a-1'
    );
    expect(f.textContent).toContain('Messaging card');
    expect(el.collectTopLevel()).toEqual({});
  });
});

describe('GCP identity in edit mode', () => {
  it('shows the stored identity, sends nothing until a pick, then the pick (created)', async () => {
    const gcp = new GcpIdentityState();
    gcp.applyStored('block', undefined);
    const el = await mount('created', { gcpIdentity: gcp });
    const select = field(el, 'gcp_identity').querySelector('sl-select') as HTMLElement & {
      value: string;
    };
    expect(select.value).toBe('block');
    expect(select.hasAttribute('disabled')).toBe(false);
    expect(field(el, 'gcp_identity').textContent).toContain(STORED_IDENTITY_HINT);
    expect(el.collectTopLevel()).toEqual({});

    gcp.pickMode('block');
    await el.updateComplete;
    expect(el.collectTopLevel()).toEqual({ gcp_identity: { metadata_mode: 'block' } });
  });

  it('with nothing stored the picker is blank and nothing is sent', async () => {
    const el = await mount('created');
    const select = field(el, 'gcp_identity').querySelector('sl-select') as HTMLElement & {
      value: string;
    };
    expect(select.value).toBe('');
    expect(el.collectTopLevel()).toEqual({});
  });
});
