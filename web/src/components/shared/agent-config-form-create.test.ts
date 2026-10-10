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
 * <scion-agent-config-form> in create mode (ptone/scion#3974): every former
 * Additional Options field round-trips to the create request, an untouched
 * form sends nothing, Clear means inherit, choices start blank so any pick
 * is a touch, and inherited values are source-labelled placeholders.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

import type { AgentConfigPlaceholder } from '../../shared/agent-config-inherited.js';
import { PROJECT_OVERRIDE } from '../../shared/agent-config-inherited.js';
import { GcpIdentityState } from '../../shared/gcp-identity-state.js';
import type { ScionAgentConfigForm } from './agent-config-form.js';

beforeAll(async () => {
  await import('./agent-config-form.js');
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function mount(props: Partial<ScionAgentConfigForm> = {}): Promise<ScionAgentConfigForm> {
  const el = document.createElement('scion-agent-config-form');
  el.mode = 'create';
  el.editability = null;
  el.branchAvailable = true;
  el.gcpIdentity = new GcpIdentityState();
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function field(el: ScionAgentConfigForm, key: string): HTMLElement {
  const found = el.shadowRoot!.querySelector<HTMLElement>(`.field[data-key="${key}"]`);
  if (!found) throw new Error(`field not rendered: ${key}`);
  return found;
}

type Valued = HTMLElement & { value: string; checked: boolean };

function control(el: ScionAgentConfigForm, key: string, sel: string): Valued {
  const c = field(el, key).querySelector(sel) as Valued | null;
  if (!c) throw new Error(`no ${sel} in ${key}`);
  return c;
}

async function type(el: ScionAgentConfigForm, key: string, value: string, sel = 'sl-input') {
  const input = control(el, key, sel);
  input.value = value;
  input.dispatchEvent(new Event('sl-input'));
  await el.updateComplete;
}

/** A user pick that changes the select's value: Shoelace fires sl-change. */
async function pick(el: ScionAgentConfigForm, key: string, value: string, sel = 'sl-select') {
  const select = control(el, key, sel);
  select.value = value;
  select.dispatchEvent(new Event('sl-change'));
  await el.updateComplete;
}

async function check(el: ScionAgentConfigForm, key: string, on: boolean, sel = 'sl-checkbox') {
  const box = control(el, key, sel);
  box.checked = on;
  box.dispatchEvent(new Event('sl-change'));
  await el.updateComplete;
}

async function clear(el: ScionAgentConfigForm, key: string) {
  (field(el, key).querySelector('sl-button.clear') as HTMLElement).click();
  await el.updateComplete;
}

const ALL_CREATE_KEYS = [
  'branch',
  'config.model',
  'config.thinking_level',
  'config.image',
  'config.user',
  'config.telemetry',
  'autoExpose',
  'agentRole',
  'messageMode',
  'config.auth_selectedType',
  'gcp_identity',
  'config.system_prompt',
  'config.agent_instructions',
  'config.max_turns',
  'config.max_model_calls',
  'config.max_duration',
  'config.resources',
  'config.env',
  'labels',
];

describe('create mode: fields and tabs', () => {
  it('renders every former Additional Options field in the five tabs', async () => {
    const el = await mount();
    const keys = Array.from(el.shadowRoot!.querySelectorAll('.field[data-key]'))
      .map((f) => (f as HTMLElement).dataset.key)
      .filter((k) => !k!.includes('.service_account'));
    expect(keys).toEqual(ALL_CREATE_KEYS);
    const tabs = Array.from(el.shadowRoot!.querySelectorAll('sl-tab')).map((t) =>
      t.textContent!.trim()
    );
    expect(tabs).toEqual([
      'General',
      'Auth & Security',
      'Prompts',
      'Limits & Resources',
      'Environment & Labels',
    ]);
    expect(el.shadowRoot!.querySelector('[data-testid="tier-summary"]')).toBeNull();
  });

  it('hides Branch when the project has no usable git remote', async () => {
    const el = await mount({ branchAvailable: false });
    expect(el.shadowRoot!.querySelector('.field[data-key="branch"]')).toBeNull();
  });
});

describe('create mode: untouched form sends nothing', () => {
  it('posts no config keys and no top-level keys the user did not set, even with placeholders', async () => {
    const placeholders: Record<string, AgentConfigPlaceholder> = {
      'config.model': { value: 'project-model', source: 'inherited from project settings' },
      'config.max_turns': { value: '40', source: 'inherited from project settings' },
      'config.telemetry': { value: 'false', source: PROJECT_OVERRIDE, override: true },
      autoExpose: { value: 'true', source: 'inherited from hub defaults' },
      agentRole: { value: 'readonly', source: 'inherited from project settings' },
    };
    const el = await mount({ placeholders });
    expect(el.collectConfigPatch()).toEqual({});
    expect(el.collectTopLevel()).toEqual({});
    expect(el.touchedKeys).toEqual([]);
    expect(el.validate()).toEqual([]);
  });
});

/**
 * One case per former Additional Options field: operate the control the
 * way a user does, then check what lands in the create request.
 */
const ROUND_TRIPS: Array<{
  /** The field the case covers. */
  key: string;
  name: string;
  act: (el: ScionAgentConfigForm) => Promise<void>;
  config?: Record<string, unknown>;
  top?: Record<string, unknown>;
}> = [
  {
    key: 'branch',
    name: 'branch',
    act: (el) => type(el, 'branch', ' feat/x '),
    top: { branch: 'feat/x' },
  },
  {
    key: 'config.model',
    name: 'model (typed)',
    act: (el) => type(el, 'config.model', 'claude-opus-4-8'),
    config: { model: 'claude-opus-4-8' },
  },
  {
    key: 'config.model',
    name: 'model (alias)',
    act: (el) => pick(el, 'config.model', 'large', 'sl-select.alias'),
    config: { model: 'large' },
  },
  {
    key: 'config.thinking_level',
    name: 'thinking level',
    act: async (el) => {
      await check(el, 'config.thinking_level', true, 'sl-checkbox.set');
      await type(el, 'config.thinking_level', '80', 'sl-range');
    },
    config: { thinking_level: 80 },
  },
  {
    key: 'config.image',
    name: 'container image',
    act: (el) => type(el, 'config.image', 'img:1'),
    config: { image: 'img:1' },
  },
  {
    key: 'config.user',
    name: 'container user',
    act: (el) => type(el, 'config.user', 'dev'),
    config: { user: 'dev' },
  },
  {
    key: 'config.telemetry',
    name: 'telemetry',
    act: (el) => pick(el, 'config.telemetry', 'false'),
    config: { telemetry: { enabled: false } },
  },
  {
    key: 'autoExpose',
    name: 'auto-expose ports',
    act: async (el) => {
      await pick(el, 'autoExpose', 'true');
      await pick(el, 'autoExpose', 'denylist', 'sl-select.ae-mode');
      await type(el, 'autoExpose', '22', 'sl-input.ae-list');
      await type(el, 'autoExpose', '5s', 'sl-input.ae-interval');
    },
    config: {
      env: {
        SCION_AUTO_EXPOSE_PORTS: 'true',
        SCION_AUTO_EXPOSE_MODE: 'denylist',
        SCION_AUTO_EXPOSE_PORTS_LIST: '22',
        SCION_AUTO_EXPOSE_INTERVAL: '5s',
      },
    },
  },
  {
    key: 'agentRole',
    name: 'agent role',
    act: (el) => pick(el, 'agentRole', 'readonly'),
    top: { agentRole: 'readonly' },
  },
  {
    key: 'messageMode',
    name: 'message mode',
    act: (el) => pick(el, 'messageMode', 'none'),
    top: { messageMode: 'none' },
  },
  {
    key: 'config.auth_selectedType',
    name: 'harness authentication',
    act: (el) => pick(el, 'config.auth_selectedType', 'vertex-ai'),
    config: { auth_selectedType: 'vertex-ai' },
  },
  {
    key: 'gcp_identity',
    name: 'GCP identity',
    act: (el) => pick(el, 'gcp_identity', 'passthrough'),
    top: { gcp_identity: { metadata_mode: 'passthrough' } },
  },
  {
    key: 'config.system_prompt',
    name: 'system prompt',
    act: (el) => type(el, 'config.system_prompt', 'Be terse.\n', 'sl-textarea'),
    config: { system_prompt: 'Be terse.\n' },
  },
  {
    key: 'config.agent_instructions',
    name: 'agent instructions',
    act: (el) => type(el, 'config.agent_instructions', 'file:///a.md', 'sl-textarea'),
    config: { agent_instructions: 'file:///a.md' },
  },
  {
    key: 'config.max_turns',
    name: 'max turns',
    act: (el) => type(el, 'config.max_turns', '25'),
    config: { max_turns: 25 },
  },
  {
    key: 'config.max_model_calls',
    name: 'max model calls',
    act: (el) => type(el, 'config.max_model_calls', '300'),
    config: { max_model_calls: 300 },
  },
  {
    key: 'config.max_duration',
    name: 'max duration',
    act: (el) => type(el, 'config.max_duration', '2h'),
    config: { max_duration: '2h' },
  },
  {
    key: 'config.resources',
    name: 'resources',
    act: async (el) => {
      const subs = field(el, 'config.resources').querySelectorAll('.sub sl-input');
      const values = ['2', '4Gi', '4', '8Gi', '20Gi'];
      for (let i = 0; i < values.length; i++) {
        const input = subs[i] as Valued;
        input.value = values[i];
        input.dispatchEvent(new Event('sl-input'));
      }
      await el.updateComplete;
    },
    config: {
      resources: {
        requests: { cpu: '2', memory: '4Gi' },
        limits: { cpu: '4', memory: '8Gi' },
        disk: '20Gi',
      },
    },
  },
  {
    key: 'config.env',
    name: 'environment variables',
    act: async (el) => {
      const editor = field(el, 'config.env').querySelector('scion-env-editor')!;
      editor.dispatchEvent(
        new CustomEvent('env-change', { detail: { entries: [{ key: 'FOO', value: 'bar' }] } })
      );
      await el.updateComplete;
    },
    config: { env: { FOO: 'bar' } },
  },
  {
    key: 'labels',
    name: 'labels',
    act: async (el) => {
      (field(el, 'labels').querySelector('sl-button.add-label') as HTMLElement).click();
      await el.updateComplete;
      await type(el, 'labels', ' team ', 'sl-input.label-key');
      await type(el, 'labels', ' web ', 'sl-input.label-value');
    },
    top: { labels: { team: 'web' } },
  },
];

describe('create mode: every former Additional Options field round-trips', () => {
  for (const rt of ROUND_TRIPS) {
    it(rt.name, async () => {
      const el = await mount();
      await rt.act(el);
      expect(el.collectConfigPatch()).toEqual(rt.config ?? {});
      expect(el.collectTopLevel()).toEqual(rt.top ?? {});
    });
  }

  it('has a round-trip case for exactly the fields the form renders', async () => {
    const el = await mount();
    const rendered = el.fields.map((f) => f.key);
    expect(new Set(ROUND_TRIPS.map((r) => r.key))).toEqual(new Set(rendered));
    expect(new Set(rendered)).toEqual(new Set(ALL_CREATE_KEYS));
  });

  it('merges custom env and auto-expose keys into one env object', async () => {
    const el = await mount();
    await pick(el, 'autoExpose', 'false');
    const editor = field(el, 'config.env').querySelector('scion-env-editor')!;
    editor.dispatchEvent(
      new CustomEvent('env-change', { detail: { entries: [{ key: 'A', value: '1' }] } })
    );
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({
      env: { A: '1', SCION_AUTO_EXPOSE_PORTS: 'false' },
    });
  });
});

describe('create mode: Clear = inherit, with the inherited placeholder shown', () => {
  const placeholders: Record<string, AgentConfigPlaceholder> = {
    'config.max_turns': { value: '40', source: 'inherited from project settings' },
    'config.model': { value: 'hub-model', source: 'inherited from hub defaults' },
    'config.auth_selectedType': { value: 'api-key', source: 'inherited from project settings' },
  };

  it('a typed limit, then Clear, sends nothing and shows the inherited value', async () => {
    const el = await mount({ placeholders });
    await type(el, 'config.max_turns', '7');
    expect(el.collectConfigPatch()).toEqual({ max_turns: 7 });
    await clear(el, 'config.max_turns');
    expect(el.collectConfigPatch()).toEqual({});
    const input = control(el, 'config.max_turns', 'sl-input');
    expect(input.value).toBe('');
    expect(input.getAttribute('placeholder')).toBe('40 (inherited from project settings)');
  });

  it('a picked choice, then Clear, sends nothing and shows the inherited value', async () => {
    const el = await mount({ placeholders });
    await pick(el, 'config.auth_selectedType', 'none');
    await clear(el, 'config.auth_selectedType');
    expect(el.collectConfigPatch()).toEqual({});
    const select = control(el, 'config.auth_selectedType', 'sl-select');
    expect(select.value).toBe('');
    expect(select.getAttribute('placeholder')).toBe(
      'Provider API Key (inherited from project settings)'
    );
  });

  it('shows just "Inherited" where the inherited value is not known', async () => {
    const el = await mount();
    for (const key of ['config.user', 'config.system_prompt', 'config.max_duration']) {
      const c = field(el, key).querySelector('sl-input, sl-textarea')!;
      expect(c.getAttribute('placeholder')).toBe('Inherited');
    }
    expect(field(el, 'config.thinking_level').textContent).toContain('Inherited');
  });

  it('Clear on an untouched field is disabled: there is nothing to clear', async () => {
    const el = await mount({ placeholders });
    expect(
      field(el, 'config.max_turns').querySelector('sl-button.clear')!.hasAttribute('disabled')
    ).toBe(true);
  });
});

describe('create mode: Unlimited is a separate limits control', () => {
  it('sends 0 for count limits and "0" for a duration, never on Clear', async () => {
    const el = await mount();
    await check(el, 'config.max_turns', true);
    await check(el, 'config.max_model_calls', true);
    await check(el, 'config.max_duration', true);
    expect(el.collectConfigPatch()).toEqual({
      max_turns: 0,
      max_model_calls: 0,
      max_duration: '0',
    });
    await clear(el, 'config.max_turns');
    expect(el.collectConfigPatch()).toEqual({ max_model_calls: 0, max_duration: '0' });
  });

  it('offers Unlimited only on the three limits', async () => {
    const el = await mount();
    const withUnlimited = Array.from(el.shadowRoot!.querySelectorAll('sl-checkbox.unlimited')).map(
      (c) => (c.closest('.field') as HTMLElement).dataset.key
    );
    expect(withUnlimited).toEqual([
      'config.max_turns',
      'config.max_model_calls',
      'config.max_duration',
    ]);
  });
});

describe('create mode: touched-tracking does not depend on same-value change events', () => {
  // Shoelace fires no sl-change when the user re-picks the value already
  // shown. Choices therefore start blank, so a pick of the inherited value
  // changes the select and is a touch. Touches come only from user events:
  // a value that reaches the control without an event is not counted.
  const choiceKeys: Array<[string, string]> = [
    ['config.telemetry', 'false'],
    ['autoExpose', 'true'],
    ['agentRole', 'readonly'],
    ['messageMode', 'project'],
    ['config.auth_selectedType', 'api-key'],
    ['gcp_identity', 'block'],
  ];

  for (const [key, inherited] of choiceKeys) {
    it(`${key} starts blank, and picking the inherited value counts`, async () => {
      const el = await mount({
        placeholders: { [key]: { value: inherited, source: 'inherited from project settings' } },
      });
      expect(control(el, key, 'sl-select').value).toBe('');
      await pick(el, key, inherited);
      expect(el.touchedKeys).toEqual([key]);
    });

    it(`${key} does not count a value set without a user event`, async () => {
      const el = await mount();
      control(el, key, 'sl-select').value = inherited;
      await el.updateComplete;
      expect(el.touchedKeys).toEqual([]);
      expect(el.collectConfigPatch()).toEqual({});
      expect(el.collectTopLevel()).toEqual({});
    });
  }

  it('the model alias picker starts blank and fills the model field', async () => {
    const el = await mount({
      placeholders: { 'config.model': { value: 'large', source: 'inherited from hub defaults' } },
    });
    expect(control(el, 'config.model', 'sl-select.alias').value).toBe('');
    expect(control(el, 'config.model', 'sl-input').value).toBe('');
    await pick(el, 'config.model', 'large', 'sl-select.alias');
    expect(el.collectConfigPatch()).toEqual({ model: 'large' });
  });

  it('thinking level is inherited until Set is checked, and unchecking it is untouched again', async () => {
    const el = await mount({
      placeholders: {
        'config.thinking_level': { value: '7', source: 'inherited from project settings' },
      },
    });
    expect(control(el, 'config.thinking_level', 'sl-range').hasAttribute('disabled')).toBe(true);
    expect(field(el, 'config.thinking_level').textContent).toContain(
      '7 (inherited from project settings)'
    );
    await check(el, 'config.thinking_level', true, 'sl-checkbox.set');
    // Set starts at the inherited value, so checking it pins what applies today.
    expect(el.collectConfigPatch()).toEqual({ thinking_level: 7 });
    await check(el, 'config.thinking_level', false, 'sl-checkbox.set');
    expect(el.collectConfigPatch()).toEqual({});
  });
});

describe('create mode: the project telemetry setting is labelled as an override', () => {
  it('names the project setting as an override, and notes it on a pick', async () => {
    const el = await mount({
      placeholders: {
        'config.telemetry': { value: 'false', source: PROJECT_OVERRIDE, override: true },
      },
    });
    const select = control(el, 'config.telemetry', 'sl-select');
    expect(select.getAttribute('placeholder')).toBe(
      'Disabled (project setting; overrides any value set here)'
    );
    expect(field(el, 'config.telemetry').querySelector('[data-testid="override-note"]')).toBeNull();
    await pick(el, 'config.telemetry', 'true');
    expect(
      field(el, 'config.telemetry').querySelector('[data-testid="override-note"]')!.textContent
    ).toContain('project setting applies instead of this choice');
    // The choice is still sent: it is recorded and applies if the setting goes.
    expect(el.collectConfigPatch()).toEqual({ telemetry: { enabled: true } });
  });
});

describe('create mode: auto-expose ports', () => {
  it('labels the inherited value with its source and sends nothing until operated', async () => {
    const el = await mount({
      placeholders: {
        autoExpose: {
          value: 'true',
          source: 'inherited from hub defaults, unless the harness config sets it',
        },
      },
    });
    expect(control(el, 'autoExpose', 'sl-select').getAttribute('placeholder')).toBe(
      'Enabled (inherited from hub defaults, unless the harness config sets it)'
    );
    expect(el.collectConfigPatch()).toEqual({});
  });

  it('sends Disabled as an explicit false with no sub-fields', async () => {
    const el = await mount();
    await pick(el, 'autoExpose', 'false');
    expect(el.collectConfigPatch()).toEqual({ env: { SCION_AUTO_EXPOSE_PORTS: 'false' } });
  });

  it('sends the default sub-field values with Enabled, and an empty list as an explicit value', async () => {
    const el = await mount();
    await pick(el, 'autoExpose', 'true');
    expect(el.collectConfigPatch()).toEqual({
      env: {
        SCION_AUTO_EXPOSE_PORTS: 'true',
        SCION_AUTO_EXPOSE_MODE: 'allowlist',
        SCION_AUTO_EXPOSE_PORTS_LIST: '',
        SCION_AUTO_EXPOSE_INTERVAL: '3s',
      },
    });
  });

  it('Clear returns to inherit', async () => {
    const el = await mount();
    await pick(el, 'autoExpose', 'true');
    await clear(el, 'autoExpose');
    expect(el.collectConfigPatch()).toEqual({});
  });
});

describe('create mode: other inline config summary', () => {
  it('is absent when none is set', async () => {
    const el = await mount();
    expect(el.shadowRoot!.querySelector('[data-testid="other-inline-config"]')).toBeNull();
  });

  it('lists volumes, skills, MCP servers, services and kubernetes when set', async () => {
    const el = await mount({
      otherInlineConfig: {
        source: 'from the template',
        config: {
          volumes: [{ target: '/data' }],
          skills: [{ uri: 'gs://s/a', as: 'a' }],
          mcp_servers: { search: { transport: 'stdio' } },
          services: [{ name: 'db', command: ['pg'] }],
          kubernetes: { namespace: 'x' },
        },
      },
    });
    const summary = el.shadowRoot!.querySelector('[data-testid="other-inline-config"]')!;
    expect(summary.textContent).toContain('from the template');
    const items = Array.from(summary.querySelectorAll('li')).map((li) => li.textContent!.trim());
    expect(items).toEqual([
      'Volumes: 1 mount',
      'Skills: a',
      'MCP servers: search',
      'Services: db',
      'Kubernetes settings',
    ]);
  });
});

describe('create mode: resources', () => {
  it('shows project defaults per sub-field as placeholders and sends only typed ones', async () => {
    const el = await mount({
      placeholders: {
        'config.resources.requests.cpu': { value: '2', source: 'inherited from project settings' },
      },
    });
    const subs = field(el, 'config.resources').querySelectorAll('.sub sl-input');
    expect((subs[0] as Valued).getAttribute('placeholder')).toBe(
      '2 (inherited from project settings)'
    );
    const disk = subs[4] as Valued;
    disk.value = '30Gi';
    disk.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({ resources: { disk: '30Gi' } });
  });
});

describe('create mode: thinking level never shows a made-up value', () => {
  it('hides the slider while inherited with no known value', async () => {
    const el = await mount();
    expect(field(el, 'config.thinking_level').querySelector('sl-range')).toBeNull();
    expect(field(el, 'config.thinking_level').textContent).toContain('Inherited');
    await check(el, 'config.thinking_level', true, 'sl-checkbox.set');
    expect(field(el, 'config.thinking_level').querySelector('sl-range')).not.toBeNull();
  });

  it('positions the disabled slider at a known inherited value', async () => {
    const el = await mount({
      placeholders: {
        'config.thinking_level': { value: '7', source: 'inherited from project settings' },
      },
    });
    const range = control(el, 'config.thinking_level', 'sl-range');
    expect(range.hasAttribute('disabled')).toBe(true);
    expect(Number(range.value)).toBe(7);
  });
});

describe('create mode: the auto-expose control wins over a custom env entry', () => {
  it('sends the control value when a custom entry names the same key', async () => {
    const el = await mount();
    const editor = field(el, 'config.env').querySelector('scion-env-editor')!;
    editor.dispatchEvent(
      new CustomEvent('env-change', {
        detail: {
          entries: [
            { key: 'SCION_AUTO_EXPOSE_PORTS', value: 'true' },
            { key: 'OTHER', value: 'x' },
          ],
        },
      })
    );
    await el.updateComplete;
    await pick(el, 'autoExpose', 'false');
    expect(el.collectConfigPatch()).toEqual({
      env: { SCION_AUTO_EXPOSE_PORTS: 'false', OTHER: 'x' },
    });
  });

  it('sends a custom entry as is while the control is untouched', async () => {
    const el = await mount();
    const editor = field(el, 'config.env').querySelector('scion-env-editor')!;
    editor.dispatchEvent(
      new CustomEvent('env-change', {
        detail: { entries: [{ key: 'SCION_AUTO_EXPOSE_PORTS', value: 'true' }] },
      })
    );
    await el.updateComplete;
    expect(el.collectConfigPatch()).toEqual({ env: { SCION_AUTO_EXPOSE_PORTS: 'true' } });
  });
});
