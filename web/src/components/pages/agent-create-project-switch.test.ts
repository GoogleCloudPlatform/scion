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
 * Project switches on the Create Agent page start the template, limits and
 * harness-config loaders without awaiting them. A slow response for a
 * project the user already left must not overwrite the current project's
 * values (ptone/scion#2940).
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, afterEach } from 'vitest';

interface AgentCreateInternals extends HTMLElement {
  updateComplete: Promise<unknown>;
  projectId: string;
  templateId: string;
  maxTurns: number;
  harnessConfigs: Array<{ name: string }>;
}

function jsonResponse(body: unknown): Response {
  return { ok: true, status: 200, json: async () => body } as Response;
}

/** Resolvers for the project B requests, held until the test releases them. */
let releaseB: Array<() => void> = [];

function stubFetch(): void {
  releaseB = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      const respond = (body: unknown): Promise<Response> => Promise.resolve(jsonResponse(body));
      const held = (body: unknown): Promise<Response> =>
        new Promise((resolve) => releaseB.push(() => resolve(jsonResponse(body))));

      if (url.includes('/api/v1/projects/proj-a/settings')) {
        return respond({ defaultTemplate: 'tmpl-a', defaultMaxTurns: 11 });
      }
      if (url.includes('/api/v1/projects/proj-b/settings')) {
        return held({ defaultTemplate: 'tmpl-b', defaultMaxTurns: 22 });
      }
      if (url.includes('/api/v1/projects?')) {
        return respond({
          projects: [
            { id: 'proj-a', name: 'A' },
            { id: 'proj-b', name: 'B' },
          ],
        });
      }
      if (url.includes('/api/v1/templates')) {
        return respond({
          templates: [
            { id: 't-a', name: 'tmpl-a', scope: 'global' },
            { id: 't-b', name: 'tmpl-b', scope: 'global' },
          ],
        });
      }
      if (url.includes('/api/v1/harness-configs') && url.includes('projectId=proj-b')) {
        return held({ harnessConfigs: [{ name: 'harness-b' }] });
      }
      if (url.includes('/api/v1/harness-configs') && url.includes('projectId=proj-a')) {
        return respond({ harnessConfigs: [{ name: 'harness-a' }] });
      }
      return respond({ brokers: [], harnessConfigs: [], items: [] });
    })
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
}

async function mount(): Promise<AgentCreateInternals> {
  await import('./agent-create.js');
  const el = document.createElement('scion-page-agent-create') as AgentCreateInternals;
  document.body.appendChild(el);
  await flush();
  await el.updateComplete;
  return el;
}

function selectProject(el: AgentCreateInternals, id: string): void {
  const select = el.shadowRoot?.querySelector('#project') as
    | (HTMLElement & { value: string })
    | null;
  if (!select) throw new Error('project select not rendered');
  select.value = id;
  select.dispatchEvent(new Event('sl-change'));
}

describe('Create Agent project switch', () => {
  it('ignores a previous project response that arrives after a switch', async () => {
    stubFetch();
    const el = await mount();
    expect(el.projectId).toBe('proj-a');
    expect(el.templateId).toBe('t-a');
    expect(el.maxTurns).toBe(11);

    // Switch to B, whose responses are held, then back to A before they land.
    selectProject(el, 'proj-b');
    await flush();
    selectProject(el, 'proj-a');
    await flush();
    expect(el.templateId).toBe('t-a');
    expect(el.harnessConfigs.map((h) => h.name)).toEqual(['harness-a']);

    // B's late responses must not overwrite A's values.
    expect(releaseB.length).toBeGreaterThan(0);
    releaseB.forEach((release) => release());
    await flush();

    expect(el.projectId).toBe('proj-a');
    expect(el.templateId).toBe('t-a');
    expect(el.maxTurns).toBe(11);
    expect(el.harnessConfigs.map((h) => h.name)).toEqual(['harness-a']);
  });
});
