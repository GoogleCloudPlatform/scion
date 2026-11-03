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
 * The shared env-override badge views (ptone/scion#3837), the Server Config
 * badges they add (Harness Configs, the project default scratchpad), and
 * the Agent Defaults sub-tabs (ptone/scion#4109).
 */
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { render, nothing } from 'lit';
import type { TemplateResult } from 'lit';

import {
  ENV_LOCK_BADGE_TEXT,
  renderEnvBadge,
  renderEnvBadgeUnder,
  renderEnvLockBadge,
  renderEnvValueLine,
  renderKeysEnvBadge,
} from '../shared/env-badge-views.js';
import { agentDefaultsPanelName } from './admin-server-config.js';
import { isListedLayer1Path } from './__fixtures__/settings-registry.js';

/** The normalised text of a rendered template ('' for nothing). */
function textOf(t: TemplateResult | typeof nothing): string {
  const host = document.createElement('div');
  render(t, host);
  return (host.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('shared env badge views', () => {
  it('renderEnvBadge says override, superseded, or not in effect', () => {
    expect(textOf(renderEnvBadge())).toBe('Overridden by environment on this node');
    expect(textOf(renderEnvBadge(false))).toBe('Environment value superseded by the saved setting');
    expect(textOf(renderEnvBadge(false, 'startup'))).toBe(
      'Environment value not in effect on this node'
    );
  });

  it('renderEnvLockBadge is the one lock badge both pages use', () => {
    expect(ENV_LOCK_BADGE_TEXT).toBe('🔒 Set via environment variable');
    expect(textOf(renderEnvLockBadge())).toBe(ENV_LOCK_BADGE_TEXT);
  });

  it('renderEnvValueLine labels the saved value only when it differs', () => {
    expect(textOf(renderEnvValueLine({ value: '2h', in_effect: true }))).toBe(
      'In effect on this node: 2h (environment variable)'
    );
    expect(textOf(renderEnvValueLine({ value: '2h', in_effect: true, saved_differs: true }))).toBe(
      'In effect on this node: 2h (environment variable)Saved value below, not in effect on this node:'
    );
    expect(textOf(renderEnvValueLine({ value: ['RS256'], in_effect: false }))).toBe(
      'Environment value on this node, not in effect: RS256 (the saved value wins)'
    );
    expect(textOf(renderEnvValueLine({ value: 'x', in_effect: false }, 'startup'))).toBe(
      'Environment value on this node, not in effect: x (a start-up default or flag wins)'
    );
  });

  it('renderKeysEnvBadge renders nothing for a key the environment does not set', () => {
    expect(renderKeysEnvBadge(['other.key'], {}, ['a.b'])).toBe(nothing);
    expect(renderKeysEnvBadge([], { 'a.b': { value: 'v', in_effect: true } }, ['a.b'])).toBe(
      nothing
    );
  });

  it('renderKeysEnvBadge shows the plain badge for a masked or unreported entry', () => {
    expect(textOf(renderKeysEnvBadge(['a.b'], {}, ['a.b']))).toBe(
      'Overridden by environment on this node'
    );
    const masked = textOf(
      renderKeysEnvBadge(['a.b'], { 'a.b': { value: '********', masked: true } }, ['a.b'])
    );
    expect(masked).toBe('Overridden by environment on this node');
    expect(masked).not.toContain('********');
  });

  it('renderKeysEnvBadge shows the value line with the reported state', () => {
    expect(
      textOf(renderKeysEnvBadge(['a.b'], { 'a.b': { value: 'v', in_effect: false } }, ['a.b']))
    ).toBe(
      'Environment value superseded by the saved setting' +
        ' Environment value on this node, not in effect: v (the saved value wins)'
    );
  });

  it('renderEnvBadgeUnder matches the prefix and its leaves only', () => {
    expect(renderEnvBadgeUnder(['harness_configsx.a'], {}, 'harness_configs')).toBe(nothing);
    expect(
      textOf(renderEnvBadgeUnder(['harness_configs.claude.image'], {}, 'harness_configs'))
    ).toBe('Overridden by environment on this node');
    const listed = textOf(
      renderEnvBadgeUnder(
        ['harness_configs.claude.image'],
        { 'harness_configs.claude.image': { value: '********', masked: true } },
        'harness_configs'
      )
    );
    expect(listed).toContain('harness_configs.claude.image');
    expect(listed).toContain('masked: whether it is in effect is not reported');
  });

  it('the scratchpad and harness config badge keys are koanf paths the server can report', () => {
    // Both are Layer-1 registry paths.
    expect(isListedLayer1Path('project_defaults.default_scratchpad')).toBe(true);
    expect(isListedLayer1Path('harness_configs')).toBe(true);
  });
});

// ── The Server Config page ──

function makeConfig(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    schema_version: '1',
    default_template: 'gemini',
    image_registry: 'ghcr.io/test',
    server: { hub: { port: 8080 }, broker: { enabled: false } },
    harness_configs: { claude: { harness: 'claude', image: 'img:1' } },
    ...overrides,
  };
}

function jsonResponse(body: unknown): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function fetchHandler(config: Record<string, unknown>) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    if (path.includes('/api/v1/admin/server-config/schema')) return jsonResponse({ sections: {} });
    if (path.includes('/api/v1/admin/server-config')) return jsonResponse(config);
    if (path.includes('/api/v1/admin/project-defaults')) {
      return jsonResponse({ default_scratchpad: true });
    }
    if (path.includes('/api/v1/admin/messaging')) {
      return jsonResponse({ cross_project_messaging_enabled: false, revision: 1 });
    }
    if (path.includes('/api/v1/admin/maintenance/check-updates')) {
      return jsonResponse({ tier: 'source', update_available: false });
    }
    if (path.includes('/api/v1/github-app/installations')) return jsonResponse([]);
    if (path.includes('/api/v1/github-app')) return jsonResponse({ configured: false });
    return jsonResponse([]);
  };
}

type PageElement = HTMLElement & { updateComplete: Promise<boolean> };

let element: PageElement | null = null;

async function createPage(config: Record<string, unknown>): Promise<PageElement> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler(config)));
  const el = document.createElement('scion-page-admin-server-config') as PageElement;
  document.body.appendChild(el);
  element = el;
  await vi.waitFor(() => {
    expect((el as unknown as { loading: boolean }).loading).toBe(false);
  });
  await el.updateComplete;
  return el;
}

function shadow(el: PageElement): ShadowRoot {
  return el.shadowRoot!;
}

function norm(s: string | null | undefined): string {
  return (s ?? '').replace(/\s+/g, ' ').trim();
}

/** The Harness Configs section (the .section whose header names it). */
function harnessSection(el: PageElement): Element | undefined {
  return Array.from(shadow(el).querySelectorAll('.section')).find((s) =>
    norm(s.querySelector('.section-title, h3')?.textContent).startsWith('Harness Configs')
  );
}

/** The Project Default Settings card. */
function projectDefaultsSection(el: PageElement): Element | undefined {
  return Array.from(shadow(el).querySelectorAll('.section')).find(
    (s) => norm(s.querySelector('h3')?.textContent) === 'Project Default Settings'
  );
}

describe('Server Config env badges and Agent Defaults tabs', () => {
  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(fetchHandler(makeConfig())));
    await import('./admin-server-config.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('Harness Configs shows an env badge for a key under harness_configs', async () => {
    const el = await createPage(
      makeConfig({
        env_overrides: ['harness_configs.claude.image'],
        env_values: { 'harness_configs.claude.image': { value: '********', masked: true } },
      })
    );
    const section = harnessSection(el);
    expect(section, 'Harness Configs section').toBeDefined();
    expect(norm(section!.querySelector('.env-badge')?.textContent)).toBe(
      'Overridden by environment on this node'
    );
    expect(norm(section!.querySelector('.env-values-under')?.textContent)).toContain(
      'harness_configs.claude.image'
    );
  });

  it('Harness Configs shows no env badge without a harness_configs key (control)', async () => {
    const el = await createPage(makeConfig({ env_overrides: ['runtimes.docker.type'] }));
    expect(harnessSection(el)!.querySelector('.env-badge')).toBeNull();
  });

  it('the scratchpad toggle shows the env value the saved setting supersedes', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'db',
        env_overrides: ['project_defaults.default_scratchpad'],
        env_values: {
          'project_defaults.default_scratchpad': { value: false, in_effect: false },
        },
      })
    );
    const card = projectDefaultsSection(el);
    expect(card, 'Project Default Settings card').toBeDefined();
    expect(norm(card!.querySelector('.env-badge')?.textContent)).toBe(
      'Environment value superseded by the saved setting'
    );
    expect(norm(card!.querySelector('.env-value-not-in-effect')?.textContent)).toBe(
      'Environment value on this node, not in effect: false (the saved value wins)'
    );
    // The toggle stays editable: the saved value is the one in effect.
    expect(card!.querySelector('sl-switch')).not.toBeNull();
  });

  it('the scratchpad toggle has no env badge when the key is not env-set (control)', async () => {
    const el = await createPage(makeConfig({ settings_tier: 'db' }));
    expect(projectDefaultsSection(el)!.querySelector('.env-badge')).toBeNull();
  });

  it('no Agent Defaults panel shares a name with a top-level panel', async () => {
    const el = await createPage(makeConfig());
    const outer = shadow(el).querySelector('sl-tab-group')!;
    const outerNames = Array.from(outer.querySelectorAll(':scope > sl-tab-panel')).map((p) =>
      p.getAttribute('name')
    );
    const inner = shadow(el).querySelector('sl-tab-group.agent-defaults-tab-group')!;
    const innerNames = Array.from(inner.querySelectorAll(':scope > sl-tab-panel')).map((p) =>
      p.getAttribute('name')
    );
    expect(innerNames).toEqual([
      agentDefaultsPanelName('general'),
      agentDefaultsPanelName('limits'),
      agentDefaultsPanelName('resources'),
    ]);
    expect(innerNames.filter((n) => outerNames.includes(n))).toEqual([]);
    // Each inner tab points at an inner panel.
    const tabPanels = Array.from(inner.querySelectorAll(':scope > sl-tab')).map((t) =>
      t.getAttribute('panel')
    );
    expect(tabPanels).toEqual(innerNames);
  });

  it('the General sub-tab panel is active on load, before any click', async () => {
    const el = await createPage(makeConfig());
    const inner = shadow(el).querySelector('sl-tab-group.agent-defaults-tab-group')!;
    const active = Array.from(inner.querySelectorAll(':scope > sl-tab-panel[active]')).map((p) =>
      p.getAttribute('name')
    );
    expect(active).toEqual([agentDefaultsPanelName('general')]);
    const activeTabs = Array.from(inner.querySelectorAll(':scope > sl-tab[active]')).map((t) =>
      t.getAttribute('panel')
    );
    expect(activeTabs).toEqual([agentDefaultsPanelName('general')]);
  });

  it('a sub-tab switch selects the sub-tab and leaves the top-level tab alone', async () => {
    const el = await createPage(makeConfig());
    const inner = shadow(el).querySelector('sl-tab-group.agent-defaults-tab-group')!;
    // Shoelace emits sl-tab-show with bubbles and composed set.
    inner.dispatchEvent(
      new CustomEvent('sl-tab-show', {
        detail: { name: agentDefaultsPanelName('limits') },
        bubbles: true,
        composed: true,
      })
    );
    await el.updateComplete;
    const page = el as unknown as { activeTab: string; agentDefaultsTab: string };
    expect(page.activeTab).toBe('general');
    expect(page.agentDefaultsTab).toBe('limits');
    const outer = shadow(el).querySelector('sl-tab-group')!;
    expect(
      Array.from(outer.querySelectorAll(':scope > sl-tab[active]')).map((t) =>
        t.getAttribute('panel')
      )
    ).toEqual(['general']);
    expect(
      Array.from(inner.querySelectorAll(':scope > sl-tab-panel[active]')).map((p) =>
        p.getAttribute('name')
      )
    ).toEqual([agentDefaultsPanelName('limits')]);
  });

  it('a top-level tab switch still selects the top-level tab', async () => {
    const el = await createPage(makeConfig());
    const outer = shadow(el).querySelector('sl-tab-group')!;
    outer.dispatchEvent(
      new CustomEvent('sl-tab-show', { detail: { name: 'hub-server' }, bubbles: true })
    );
    await el.updateComplete;
    expect((el as unknown as { activeTab: string }).activeTab).toBe('hub-server');
  });
});
