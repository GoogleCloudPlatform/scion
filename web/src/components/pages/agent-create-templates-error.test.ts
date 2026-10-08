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
 * A failed template list load on the Create Agent page (ptone/scion#3465)
 * shows an inline error on the Template field; projects, brokers and the
 * rest of the form still load.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

let templatesStatus = 500;

beforeEach(() => {
  templatesStatus = 500;
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      if (url.includes('/api/v1/templates')) {
        return Promise.resolve(
          new Response(JSON.stringify({ templates: [{ id: 't1', name: 't1' }] }), {
            status: templatesStatus,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(
        new Response(
          JSON.stringify({
            projects: url.includes('/api/v1/projects?') ? [{ id: 'p1', name: 'P1' }] : [],
            brokers: [{ id: 'b1', name: 'B1' }],
            harnessConfigs: [],
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    })
  );
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  document.body.innerHTML = '';
});

type CreateEl = HTMLElement & {
  updateComplete: Promise<unknown>;
  loading: boolean;
  error: string | null;
  projects: Array<{ id: string }>;
  brokers: Array<{ id: string }>;
  templates: Array<{ id: string }>;
};

async function mount(): Promise<CreateEl> {
  await import('./agent-create.js');
  const el = document.createElement('scion-page-agent-create') as CreateEl;
  document.body.appendChild(el);
  await vi.waitFor(() => expect(el.loading).toBe(false));
  await el.updateComplete;
  return el;
}

function text(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

describe('Create Agent template load failure', () => {
  it('loads the rest of the form and shows an inline template error', async () => {
    const el = await mount();

    expect(el.error).toBeNull();
    expect(el.projects.map((p) => p.id)).toEqual(['p1']);
    expect(el.brokers.map((b) => b.id)).toEqual(['b1']);
    expect(el.templates).toEqual([]);
    expect(text(el)).toContain('Could not load templates.');
  });

  it('shows no template error when the list loads', async () => {
    templatesStatus = 200;
    const el = await mount();

    expect(el.templates.map((t) => t.id)).toEqual(['t1']);
    expect(text(el)).not.toContain('Could not load templates.');
  });
});
