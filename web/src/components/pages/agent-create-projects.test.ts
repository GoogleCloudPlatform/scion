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
 * Which projects the Create Agent picker asks for (#106).
 *
 * The picker used `mine=true`, which the Hub resolves to projects where the
 * caller holds the project-owner role specifically. Creating an agent does not
 * require ownership — project-member carries `agent.create`, and the create
 * endpoint authorizes against the target project — so every project a member
 * belonged to was missing from the picker and they could not create an agent
 * anywhere, with no error to explain it.
 *
 * This pins the request, because the defect is entirely in what is asked for:
 * the response handling was always correct.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

/** Every URL the component fetched, in order. */
let requested: string[] = [];

/** Minimal stand-ins so loadFormData can run to completion. */
function stubFetch(): void {
  requested = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      requested.push(url);
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ projects: [], brokers: [], templates: [], harnessConfigs: [] }),
      } as Response);
    })
  );
}

beforeEach(() => {
  stubFetch();
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

async function mountAgentCreate(): Promise<HTMLElement> {
  await import('./agent-create.js');
  const el = document.createElement('scion-page-agent-create');
  document.body.appendChild(el);
  await new Promise((r) => setTimeout(r, 0));
  await (el as HTMLElement & { updateComplete: Promise<unknown> }).updateComplete;
  return el;
}

/** The projects request, whatever else was fetched alongside it. */
function projectsRequest(): string | undefined {
  return requested.find((u) => u.includes('/api/v1/projects'));
}

describe('Create Agent project picker', () => {
  it('does not restrict the project list to projects the caller owns', async () => {
    await mountAgentCreate();

    const url = projectsRequest();
    expect(url, 'the page should request a project list').toBeDefined();
    // `mine=true` means owner-only on the server, which is a narrower question
    // than the create endpoint actually enforces.
    expect(url).not.toContain('mine=true');
  });

  it('requests the project list the caller may read', async () => {
    await mountAgentCreate();

    const url = projectsRequest();
    expect(url).toContain('/api/v1/projects');
    // The default list is already scoped to what the caller can read, which is
    // the correct population for a picker whose action is authorized per
    // project at submit time.
    expect(url).toContain('limit=');
  });
});
