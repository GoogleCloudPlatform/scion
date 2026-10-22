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
 * Create Agent harness config (ptone/scion#3914): the form sends
 * harnessConfig only when the user picked one. Untouched, it is omitted so
 * the hub applies its own order (project default, then the template's
 * declared default_harness_config, then the hub default), and the picker
 * shows that inherited value as its placeholder. A template's harness type
 * is never used as a harness config name.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { createInternals } from './__fixtures__/agent-create-internals.js';
import { invalidatePublicSettings } from '../../client/server-feature-flags.js';

interface TemplateFixture {
  id: string;
  name: string;
  slug: string;
  harness: string;
  defaultHarnessConfig?: string;
  status: string;
  scope: string;
}

interface CreatePrivate extends HTMLElement {
  loading: boolean;
  name: string;
  projectId: string;
  templateId: string;
  harness: string;
  error: string | null;
  updateComplete: Promise<unknown>;
  handleSubmit(e: Event, provisionOnly?: boolean): Promise<void>;
}

let projectSettings: Record<string, unknown> = {};
let hubSettings: Record<string, unknown> = {};
let templates: TemplateFixture[] = [];
let bodies: Array<Record<string, unknown>> = [];

function stubFetch(): void {
  bodies = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if (url.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          bodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        // A 400 makes handleSubmit stop before navigating away.
        return Promise.resolve({
          ok: false,
          status: 400,
          json: () => Promise.resolve({ error: { message: 'stub: not created' } }),
        } as Response);
      }
      let body: unknown = { projects: [], brokers: [], templates: [], harnessConfigs: [] };
      if (url.includes('/settings/public')) {
        body = { ...hubSettings };
      } else if (url.includes('/api/v1/projects?')) {
        body = {
          projects: [
            { id: 'p1', name: 'P1' },
            { id: 'p2', name: 'P2' },
          ],
        };
      } else if (url.includes('/api/v1/projects/p1/settings')) {
        body = { ...projectSettings };
      } else if (url.includes('/api/v1/projects/p2/settings')) {
        body = {};
      } else if (url.includes('/api/v1/templates')) {
        body = { templates };
      } else if (url.includes('/api/v1/harness-configs')) {
        body = {
          harnessConfigs: url.includes('projectId=p1')
            ? [{ name: 'claude' }, { name: 'gemini' }, { name: 'p1-only' }]
            : [{ name: 'claude' }, { name: 'gemini' }],
        };
      } else if (url.includes('/gcp-service-accounts')) {
        body = { items: [] };
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve(body),
      } as Response);
    })
  );
}

beforeAll(async () => {
  await import('./agent-create.js');
});

afterEach(() => {
  vi.unstubAllGlobals();
  invalidatePublicSettings();
  document.body.innerHTML = '';
  projectSettings = {};
  hubSettings = {};
  templates = [];
});

async function settle(c: CreatePrivate): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
  const deadline = Date.now() + 2000;
  while (c.loading && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    await c.updateComplete;
  }
  await new Promise((r) => setTimeout(r, 20));
  await c.updateComplete;
}

/** Mounts the page with project p1 and template t1 (when the fixture has one) chosen. */
async function mount(): Promise<CreatePrivate> {
  stubFetch();
  const el = document.createElement('scion-page-agent-create');
  document.body.appendChild(el);
  const c = createInternals<CreatePrivate>(el);
  await settle(c);
  c.name = 'test-agent';
  c.projectId = 'p1';
  await settle(c);
  if (templates.length > 0) c.templateId = templates[0].id;
  await c.updateComplete;
  return c;
}

function select(c: CreatePrivate, id: string): HTMLElement & { value: string } {
  const el = c.shadowRoot?.querySelector(`#${id}`);
  expect(el).toBeTruthy();
  return el as HTMLElement & { value: string };
}

function harnessSelect(c: CreatePrivate): HTMLElement & { value: string } {
  return select(c, 'harness');
}

/** Operates a page select the way a user does: set the value, fire sl-change. */
async function choose(c: CreatePrivate, id: string, value: string): Promise<void> {
  const el = select(c, id);
  el.value = value;
  el.dispatchEvent(new Event('sl-change'));
  await c.updateComplete;
}

/** Types into a page input the way a user does: set the value, fire sl-input. */
async function type(c: CreatePrivate, id: string, value: string): Promise<void> {
  const el = select(c, id);
  el.value = value;
  el.dispatchEvent(new Event('sl-input'));
  await c.updateComplete;
}

async function submit(c: CreatePrivate): Promise<Record<string, unknown>> {
  const before = bodies.length;
  await c.handleSubmit(new Event('submit'));
  expect(bodies).toHaveLength(before + 1);
  return bodies[bodies.length - 1];
}

const template = (extra: Partial<TemplateFixture>): TemplateFixture => ({
  id: 't1',
  name: 'default',
  slug: 'default',
  harness: 'claude',
  status: 'active',
  scope: 'global',
  ...extra,
});

describe('Create Agent: harness config is sent only when picked', () => {
  it('omits harnessConfig when untouched and shows the project default as the placeholder', async () => {
    projectSettings = { defaultHarnessConfig: 'project-hc' };
    hubSettings = { defaultHarnessConfig: 'hub-hc' };
    templates = [template({ defaultHarnessConfig: 'template-hc' })];
    const c = await mount();
    expect(c.harness).toBe('');
    expect(harnessSelect(c).getAttribute('placeholder')).toBe(
      'project-hc (inherited from project settings)'
    );
    const body = await submit(c);
    expect(body).not.toHaveProperty('harnessConfig');
    expect(body.template).toBe('t1');
  });

  it("shows the template's declared default harness config over the hub default", async () => {
    hubSettings = { defaultHarnessConfig: 'hub-hc' };
    templates = [template({ defaultHarnessConfig: 'template-hc' })];
    const c = await mount();
    expect(harnessSelect(c).getAttribute('placeholder')).toBe(
      'template-hc (inherited from the template)'
    );
    expect(await submit(c)).not.toHaveProperty('harnessConfig');
  });

  it("never uses the template's harness type: the hub default applies", async () => {
    hubSettings = { defaultHarnessConfig: 'hub-hc' };
    templates = [template({ harness: 'gemini' })];
    const c = await mount();
    expect(harnessSelect(c).getAttribute('placeholder')).toBe(
      'hub-hc (inherited from hub defaults)'
    );
    expect(await submit(c)).not.toHaveProperty('harnessConfig');
  });

  it('shows Inherited when no source the page reads sets one', async () => {
    templates = [template({})];
    const c = await mount();
    expect(harnessSelect(c).getAttribute('placeholder')).toBe('Inherited');
    expect(await submit(c)).not.toHaveProperty('harnessConfig');
  });

  it('sends the harness config the user picked', async () => {
    projectSettings = { defaultHarnessConfig: 'project-hc' };
    templates = [template({ defaultHarnessConfig: 'template-hc' })];
    const c = await mount();
    await choose(c, 'harness', 'gemini');
    expect((await submit(c)).harnessConfig).toBe('gemini');
  });

  it('keeps a pick when the user changes the template', async () => {
    templates = [
      template({ defaultHarnessConfig: 'template-hc' }),
      template({ id: 't2', name: 'other', slug: 'other', defaultHarnessConfig: 'other-hc' }),
    ];
    const c = await mount();
    await choose(c, 'harness', 'claude');
    await choose(c, 'template', 't2');
    expect(c.templateId).toBe('t2');
    expect(c.harness).toBe('claude');
    expect((await submit(c)).harnessConfig).toBe('claude');
  });

  it("changing the template with no pick shows the new template's default", async () => {
    templates = [
      template({ defaultHarnessConfig: 'template-hc' }),
      template({ id: 't2', name: 'other', slug: 'other', defaultHarnessConfig: 'other-hc' }),
    ];
    const c = await mount();
    await choose(c, 'template', 't2');
    expect(c.harness).toBe('');
    expect(harnessSelect(c).getAttribute('placeholder')).toBe(
      'other-hc (inherited from the template)'
    );
    expect(await submit(c)).not.toHaveProperty('harnessConfig');
  });

  it('clearing the picker goes back to inheriting', async () => {
    projectSettings = { defaultHarnessConfig: 'project-hc' };
    templates = [template({})];
    const c = await mount();
    await choose(c, 'harness', 'gemini');
    await choose(c, 'harness', '');
    expect(c.harness).toBe('');
    expect(await submit(c)).not.toHaveProperty('harnessConfig');
  });

  it("drops a pick the new project's harness-config list does not offer", async () => {
    templates = [template({})];
    const c = await mount();
    await choose(c, 'harness', 'p1-only');
    expect(c.harness).toBe('p1-only');
    await choose(c, 'project', 'p2');
    await settle(c);
    expect(c.projectId).toBe('p2');
    expect(c.harness).toBe('');
    expect(await submit(c)).not.toHaveProperty('harnessConfig');
  });

  it('keeps a pick the new project also offers', async () => {
    templates = [template({})];
    const c = await mount();
    await choose(c, 'harness', 'gemini');
    await choose(c, 'project', 'p2');
    await settle(c);
    expect((await submit(c)).harnessConfig).toBe('gemini');
  });
});

describe('Create Agent: "Other..." harness config needs a name', () => {
  it('blocks submit with an error when the custom name is blank', async () => {
    projectSettings = { defaultHarnessConfig: 'project-hc' };
    templates = [template({})];
    const c = await mount();
    await choose(c, 'harness', '__other__');
    await type(c, 'custom-harness', '   ');
    await c.handleSubmit(new Event('submit'));
    await c.updateComplete;
    expect(bodies).toHaveLength(0);
    expect(c.error).toMatch(/custom harness config name/i);
    expect(c.shadowRoot?.querySelector('.error-banner')?.textContent).toMatch(
      /custom harness config name/i
    );
  });

  it('sends the custom name when one is entered', async () => {
    templates = [template({})];
    const c = await mount();
    await choose(c, 'harness', '__other__');
    await type(c, 'custom-harness', ' my-custom-harness ');
    const body = await submit(c);
    expect(body.harnessConfig).toBe('my-custom-harness');
  });

  it('still omits harnessConfig when the picker is untouched', async () => {
    templates = [template({})];
    const c = await mount();
    const body = await submit(c);
    expect(body).not.toHaveProperty('harnessConfig');
    expect(c.error).not.toMatch(/custom harness config name/i);
  });
});
