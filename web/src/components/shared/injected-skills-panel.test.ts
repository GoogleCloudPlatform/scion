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
 * Tests for the directory batch-add flow in injected-skills-panel.ts.
 *
 * Three behaviours matter and are easy to regress:
 *
 *  1. The "Discover Skills from Directory" button is gated on the *normalized*
 *     URI, not the raw input. gh:// and skill:// URIs are unambiguously single
 *     skills and must not offer discovery; a URL that stays https:// might be
 *     a directory, so discovery is offered alongside plain add.
 *  2. Hub scope must batch into exactly ONE PUT. Hub injected skills use a
 *     PUT-whole-list API, so a naive loop would issue N read-modify-writes.
 *  3. Project/user scope keeps using the per-item POST endpoint, once per URI.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

let ScionInjectedSkillsPanel: any;

/** One recorded fetch call. */
interface Call {
  url: string;
  method: string;
  body: any;
}

/**
 * Build a fetch mock that records every call and answers the endpoints the
 * panel touches: list GETs, project/user POSTs, hub PUTs, and the new
 * discover-directory POST.
 */
function makeFetchMock(calls: Call[], discoverResponse?: { status: number; body: unknown }) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const href = String(url);
    const method = init?.method ?? 'GET';
    let parsed: any = undefined;
    if (init?.body) {
      try {
        parsed = JSON.parse(init.body as string);
      } catch {
        /* non-JSON body — ignore */
      }
    }
    calls.push({ url: href, method, body: parsed });

    if (href.includes('/skills/discover-directory')) {
      const r = discoverResponse ?? { status: 200, body: { skills: [], count: 0 } };
      return Promise.resolve(
        new Response(JSON.stringify(r.body), {
          status: r.status,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // Hub list GET and project/user list GET have different shapes; return both
    // keys so either read path finds an empty list.
    return Promise.resolve(
      new Response(JSON.stringify({ entries: [], system: [], user_defined: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

/** Create the panel for a given scope and let its initial load() settle. */
async function createPanel(
  scope: 'project' | 'user' | 'hub',
  calls: Call[],
  discoverResponse?: { status: number; body: unknown }
) {
  vi.stubGlobal('fetch', vi.fn(makeFetchMock(calls, discoverResponse)));
  const el = document.createElement('scion-injected-skills-panel') as any;
  el.scope = scope;
  if (scope === 'project') el.scopeId = 'proj-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 20));
  await el.updateComplete;
  calls.length = 0; // Drop the initial load GET so assertions see only the action.
  return el;
}

describe('injected-skills-panel — discover button gating', () => {
  beforeAll(async () => {
    const mod = await import('./injected-skills-panel.js');
    ScionInjectedSkillsPanel = mod.ScionInjectedSkillsPanel;
    expect(ScionInjectedSkillsPanel).toBeDefined();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('does not offer discovery for a gh:// URI', async () => {
    const el = await createPanel('project', []);
    el.dialogUri = 'gh://org/repo/my-skill@main';
    el.dialogTransformed = null;
    expect(el.showDiscoverButton).toBe(false);
  });

  it('does not offer discovery for a skill:// URI', async () => {
    const el = await createPanel('project', []);
    el.dialogUri = 'skill://scion/core/my-skill';
    el.dialogTransformed = null;
    expect(el.showDiscoverButton).toBe(false);
  });

  it('does not offer discovery when a GitHub URL normalizes to gh:// shorthand', async () => {
    const el = await createPanel('project', []);
    // A standard skills/ path collapses to gh://, so it is a single skill.
    el.dialogUri = 'https://github.com/org/repo/tree/main/skills/my-skill';
    el.dialogTransformed = 'gh://org/repo/my-skill@main';
    expect(el.showDiscoverButton).toBe(false);
  });

  it('offers discovery when the normalized result stays https://', async () => {
    const el = await createPanel('project', []);
    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    el.dialogTransformed = null;
    expect(el.showDiscoverButton).toBe(true);
  });

  it('offers discovery for a custom-path URL that normalizes to a full https:// URL', async () => {
    const el = await createPanel('project', []);
    el.dialogUri = 'https://github.com/org/repo/tree/main/custom';
    el.dialogTransformed = 'https://github.com/org/repo/tree/main/custom';
    expect(el.showDiscoverButton).toBe(true);
  });
});

describe('injected-skills-panel — handleDiscoverDirectory', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('opens the selection dialog with everything pre-selected on success', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls, {
      status: 200,
      body: {
        skills: [
          { uri: 'gh://org/repo/a@main', name: 'a' },
          { uri: 'gh://org/repo/b@main', name: 'b' },
        ],
        count: 2,
      },
    });

    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();

    expect(el.discoveryDialogOpen).toBe(true);
    expect(el.discoveredSkills).toHaveLength(2);
    expect([...el.selectedSkillURIs].sort()).toEqual([
      'gh://org/repo/a@main',
      'gh://org/repo/b@main',
    ]);
    expect(el.discoveryError).toBeNull();
  });

  it('sends projectId for project scope but not for hub scope', async () => {
    const projectCalls: Call[] = [];
    const projectPanel = await createPanel('project', projectCalls, {
      status: 200,
      body: { skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }], count: 1 },
    });
    projectPanel.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await projectPanel.handleDiscoverDirectory();
    const projectDiscover = projectCalls.find((c) => c.url.includes('discover-directory'));
    expect(projectDiscover?.body.projectId).toBe('proj-1');

    document.body.innerHTML = '';

    const hubCalls: Call[] = [];
    const hubPanel = await createPanel('hub', hubCalls, {
      status: 200,
      body: { skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }], count: 1 },
    });
    hubPanel.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await hubPanel.handleDiscoverDirectory();
    const hubDiscover = hubCalls.find((c) => c.url.includes('discover-directory'));
    expect(hubDiscover?.body.projectId).toBeUndefined();
    expect(hubDiscover?.body.sourceUrl).toBe('https://github.com/org/repo/tree/main/skills');
  });

  it('surfaces a backend error inline and does not open the selection dialog', async () => {
    const el = await createPanel('project', [], {
      status: 400,
      body: { error: { code: 'discover_failed', message: 'no skills found at ...' } },
    });

    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();

    expect(el.discoveryDialogOpen).toBe(false);
    expect(el.discoveryError).toBeTruthy();
  });

  it('treats an empty skills array as an error, not an empty dialog', async () => {
    const el = await createPanel('project', [], {
      status: 200,
      body: { skills: [], count: 0 },
    });

    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();

    expect(el.discoveryDialogOpen).toBe(false);
    expect(el.discoveryError).toBe('No skills found at this URL.');
  });

  it('does not call the backend for an empty URI', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls);
    el.dialogUri = '   ';
    await el.handleDiscoverDirectory();

    expect(calls.filter((c) => c.url.includes('discover-directory'))).toHaveLength(0);
    expect(el.discoveryError).toBeTruthy();
  });
});

describe('injected-skills-panel — addEntries batching', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('hub scope writes all selected skills in exactly one PUT', async () => {
    const calls: Call[] = [];
    const el = await createPanel('hub', calls);

    // Pretend the hub already holds one system entry and one user entry: the
    // PUT must preserve the user entry and drop the readonly system one.
    el.rows = [
      { id: '', uri: 'skill://sys/one', as: '', optional: false, sortOrder: 0, skillName: '', skillSlug: '', readonly: true },
      { id: '', uri: 'gh://org/repo/existing@main', as: '', optional: false, sortOrder: 1, skillName: '', skillSlug: '', readonly: false },
    ];

    await el.addEntries(['gh://org/repo/a@main', 'gh://org/repo/b@main', 'gh://org/repo/c@main']);

    const puts = calls.filter((c) => c.method === 'PUT');
    expect(puts).toHaveLength(1);
    expect(puts[0].url).toContain('/api/v1/hub/settings/injected-skills');
    expect(puts[0].body.user_defined.map((r: any) => r.uri)).toEqual([
      'gh://org/repo/existing@main',
      'gh://org/repo/a@main',
      'gh://org/repo/b@main',
      'gh://org/repo/c@main',
    ]);
    // No per-item POSTs for hub scope.
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0);
  });

  it('project scope issues one POST per selected skill', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls);

    await el.addEntries(['gh://org/repo/a@main', 'gh://org/repo/b@main']);

    const posts = calls.filter((c) => c.method === 'POST');
    expect(posts).toHaveLength(2);
    expect(posts.map((p) => p.body.skillUri)).toEqual([
      'gh://org/repo/a@main',
      'gh://org/repo/b@main',
    ]);
    expect(posts[0].url).toContain('/api/v1/projects/proj-1/injected-skills');
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
  });

  it('user scope issues one POST per selected skill', async () => {
    const calls: Call[] = [];
    const el = await createPanel('user', calls);

    await el.addEntries(['gh://org/repo/a@main']);

    const posts = calls.filter((c) => c.method === 'POST');
    expect(posts).toHaveLength(1);
    expect(posts[0].url).toContain('/api/v1/users/me/injected-skills');
  });

  it('makes no network calls for an empty URI list', async () => {
    const calls: Call[] = [];
    const el = await createPanel('hub', calls);

    await el.addEntries([]);

    expect(calls).toHaveLength(0);
  });
});

describe('injected-skills-panel — handleDiscoveryConfirm', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('adds the selected skills and closes both dialogs', async () => {
    const calls: Call[] = [];
    const el = await createPanel('hub', calls);

    el.dialogOpen = true;
    el.discoveryDialogOpen = true;
    el.discoveredSkills = [
      { uri: 'gh://org/repo/a@main', name: 'a' },
      { uri: 'gh://org/repo/b@main', name: 'b' },
    ];
    el.selectedSkillURIs = new Set(['gh://org/repo/a@main']);

    await el.handleDiscoveryConfirm();

    const puts = calls.filter((c) => c.method === 'PUT');
    expect(puts).toHaveLength(1);
    expect(puts[0].body.user_defined.map((r: any) => r.uri)).toEqual(['gh://org/repo/a@main']);
    expect(el.discoveryDialogOpen).toBe(false);
    expect(el.dialogOpen).toBe(false);
  });

  it('keeps the selection dialog open and shows the error when the add fails', async () => {
    const calls: Call[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request, init?: RequestInit): Promise<Response> => {
        const href = String(url);
        const method = init?.method ?? 'GET';
        calls.push({ url: href, method, body: undefined });
        if (method === 'PUT') {
          return Promise.resolve(
            new Response(JSON.stringify({ error: { message: 'boom' } }), {
              status: 500,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ entries: [], system: [], user_defined: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    const el = document.createElement('scion-injected-skills-panel') as any;
    el.scope = 'hub';
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 20));

    el.dialogOpen = true;
    el.discoveryDialogOpen = true;
    el.discoveredSkills = [{ uri: 'gh://org/repo/a@main', name: 'a' }];
    el.selectedSkillURIs = new Set(['gh://org/repo/a@main']);

    await el.handleDiscoveryConfirm();

    expect(el.discoveryDialogOpen).toBe(true);
    expect(el.discoveryError).toBeTruthy();
  });
});
