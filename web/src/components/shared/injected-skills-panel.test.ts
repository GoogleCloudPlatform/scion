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
 * Four behaviours matter and are easy to regress:
 *
 *  1. The "Discover Skills from Directory" button is gated on the *normalized*
 *     URI, not the raw input, and only for github.com hosts. gh:// and skill://
 *     URIs are unambiguously single skills and must not offer discovery; a URL
 *     that stays https://github.com/ might be a directory, so discovery is
 *     offered alongside plain add.
 *  2. Hub scope must batch into exactly ONE PUT. Hub injected skills use a
 *     PUT-whole-list API, so a naive loop would issue N read-modify-writes.
 *  3. Project/user scope keeps using the per-item POST endpoint, once per URI —
 *     and that endpoint 409s on duplicates, so addEntries() must filter what is
 *     already present and must not abort the batch on the first failure.
 *  4. Discovery state must not survive a Cancel.
 *
 * Shoelace elements are not registered in this environment; they render as
 * unknown elements, which is enough to assert presence, attributes, and to
 * dispatch the sl-* events the component listens for.
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

/** Knobs for the fetch mock. All optional; defaults are the happy path. */
interface MockOptions {
  /** Response for POST /api/v1/skills/discover-directory. */
  discover?: { status: number; body: unknown };
  /** Deferred gate: when set, the discover response waits on this promise. */
  discoverGate?: Promise<void>;
  /** Per-skillUri status override for injected-skill POSTs (default 201). */
  postStatus?: Record<string, number>;
  /** Status for the hub PUT (default 200). */
  putStatus?: number;
  /** Entries returned by the list GET, in project/user wire shape. */
  entries?: Array<{ id: string; skillUri: string }>;
}

function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/**
 * Build a fetch mock that records every call and answers the endpoints the
 * panel touches: list GETs, project/user POSTs, hub PUTs, and the
 * discover-directory POST.
 */
function makeFetchMock(calls: Call[], opts: MockOptions = {}) {
  return async (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
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
      if (opts.discoverGate) await opts.discoverGate;
      const r = opts.discover ?? { status: 200, body: { skills: [], count: 0 } };
      return jsonResponse(r.body, r.status);
    }

    if (method === 'POST') {
      const status = opts.postStatus?.[parsed?.skillUri as string] ?? 201;
      if (status >= 400) {
        return jsonResponse({ error: { code: 'conflict', message: `rejected ${status}` } }, status);
      }
      return jsonResponse({ id: 'new-entry' }, status);
    }

    if (method === 'PUT') {
      const status = opts.putStatus ?? 200;
      if (status >= 400) {
        return jsonResponse({ error: { message: 'boom' } }, status);
      }
      return jsonResponse({}, status);
    }

    // Hub list GET and project/user list GET have different shapes; return both
    // keys so either read path finds the configured list.
    return jsonResponse(
      { entries: opts.entries ?? [], system: [], user_defined: [] },
      200
    );
  };
}

/** Wait until the panel's initial (or in-flight) load() has settled. */
async function settle(el: any): Promise<void> {
  await vi.waitFor(() => {
    if (el.loading) throw new Error('panel still loading');
  });
  await el.updateComplete;
}

/** Create the panel for a given scope and let its initial load() settle. */
async function createPanel(
  scope: 'project' | 'user' | 'hub',
  calls: Call[],
  opts: MockOptions = {}
) {
  vi.stubGlobal('fetch', vi.fn(makeFetchMock(calls, opts)));
  const el = document.createElement('scion-injected-skills-panel') as any;
  el.scope = scope;
  if (scope === 'project') el.scopeId = 'proj-1';
  document.body.appendChild(el);
  await settle(el);
  calls.length = 0; // Drop the initial load GET so assertions see only the action.
  return el;
}

/** Build a SkillRow the way load() would. */
function row(uri: string, readonly = false) {
  return {
    id: `id-${uri}`,
    uri,
    as: '',
    optional: false,
    sortOrder: 0,
    skillName: '',
    skillSlug: '',
    readonly,
  };
}

/** All <sl-button> labels currently in the panel's shadow DOM. */
function buttonLabels(el: any): string[] {
  return [...el.shadowRoot.querySelectorAll('sl-button')].map((b: any) =>
    (b.textContent ?? '').trim()
  );
}

const DISCOVER_LABEL = 'Discover Skills from Directory';

/** Open the add dialog in URI mode with the given input value. */
async function openUriDialog(el: any, uri: string) {
  el.dialogOpen = true;
  el.dialogMode = 'uri';
  el.dialogUri = uri;
  await el.updateComplete;
}

function cleanup() {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
}

describe('injected-skills-panel — discover button gating', () => {
  beforeAll(async () => {
    const mod = await import('./injected-skills-panel.js');
    ScionInjectedSkillsPanel = mod.ScionInjectedSkillsPanel;
    expect(ScionInjectedSkillsPanel).toBeDefined();
  });

  afterEach(cleanup);

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

  it('offers discovery when the normalized result stays a github.com https:// URL', async () => {
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

  it('does not offer discovery for a non-github https:// host', async () => {
    // The backend rejects anything but https://github.com/, so offering the
    // button here would only buy the user a round-trip to a 400.
    const el = await createPanel('project', []);
    el.dialogUri = 'https://gitlab.com/org/repo/tree/main/skills';
    el.dialogTransformed = null;
    expect(el.showDiscoverButton).toBe(false);
  });

  it('matches the github.com host case-insensitively', async () => {
    const el = await createPanel('project', []);
    el.dialogUri = 'HTTPS://GitHub.com/org/repo/tree/main/skills';
    el.dialogTransformed = null;
    expect(el.showDiscoverButton).toBe(true);
  });
});

describe('injected-skills-panel — discover button in the DOM', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(cleanup);

  it('renders the discover button for a GitHub directory URL in URI mode', async () => {
    const el = await createPanel('project', []);
    await openUriDialog(el, 'https://github.com/org/repo/tree/main/skills');
    expect(buttonLabels(el)).toContain(DISCOVER_LABEL);
  });

  it('omits the discover button for a gh:// URI', async () => {
    const el = await createPanel('project', []);
    await openUriDialog(el, 'gh://org/repo/my-skill@main');
    expect(buttonLabels(el)).not.toContain(DISCOVER_LABEL);
  });

  it('omits the discover button in skill-bank search mode', async () => {
    // Search mode picks a hub-bank skill by slug; there is no URL to probe.
    const el = await createPanel('project', []);
    el.dialogOpen = true;
    el.dialogMode = 'search';
    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.updateComplete;
    expect(buttonLabels(el)).not.toContain(DISCOVER_LABEL);
  });

  it('shows the button once typing produces a directory URL', async () => {
    // Drive the real sl-input handler so normalizeSkillURIClient runs, rather
    // than setting dialogTransformed by hand and bypassing the gate under test.
    const el = await createPanel('project', []);
    el.dialogOpen = true;
    el.dialogMode = 'uri';
    await el.updateComplete;

    const input: any = el.shadowRoot.querySelector('sl-input[label="Skill URI"]');
    expect(input).toBeTruthy();

    // A standard skills/<name> path normalizes to gh:// — single skill, no button.
    input.value = 'https://github.com/org/repo/tree/main/skills/my-skill';
    input.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(el.dialogTransformed).toBe('gh://org/repo/my-skill@main');
    expect(buttonLabels(el)).not.toContain(DISCOVER_LABEL);

    // The parent directory stays https:// — offer discovery.
    input.value = 'https://github.com/org/repo/tree/main/skills';
    input.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(buttonLabels(el)).toContain(DISCOVER_LABEL);
  });
});

describe('injected-skills-panel — handleDiscoverDirectory', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(cleanup);

  it('opens the selection dialog with everything pre-selected on success', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls, {
      discover: {
        status: 200,
        body: {
          skills: [
            { uri: 'gh://org/repo/a@main', name: 'a' },
            { uri: 'gh://org/repo/b@main', name: 'b' },
          ],
          count: 2,
        },
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

  it('sends projectId for project scope', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls, {
      discover: { status: 200, body: { skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }], count: 1 } },
    });
    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();

    const discover = calls.find((c) => c.url.includes('discover-directory'));
    expect(discover?.body.projectId).toBe('proj-1');
  });

  it('sends no projectId for hub scope', async () => {
    const calls: Call[] = [];
    const el = await createPanel('hub', calls, {
      discover: { status: 200, body: { skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }], count: 1 } },
    });
    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();

    const discover = calls.find((c) => c.url.includes('discover-directory'));
    expect(discover?.body.projectId).toBeUndefined();
    expect(discover?.body.sourceUrl).toBe('https://github.com/org/repo/tree/main/skills');
  });

  it('sends no projectId for user scope', async () => {
    // User scope has no project credentials to spend; the hub does an
    // unauthenticated fetch of a public repo.
    const calls: Call[] = [];
    const el = await createPanel('user', calls, {
      discover: { status: 200, body: { skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }], count: 1 } },
    });
    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();

    const discover = calls.find((c) => c.url.includes('discover-directory'));
    expect(discover?.body.projectId).toBeUndefined();
  });

  it('posts the raw directory URL, not the normalized form', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls, {
      discover: { status: 200, body: { skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }], count: 1 } },
    });
    el.dialogUri = 'https://github.com/org/repo/tree/main/custom/dir';
    el.dialogTransformed = 'https://github.com/org/repo/tree/main/custom/dir';
    await el.handleDiscoverDirectory();

    const discover = calls.find((c) => c.url.includes('discover-directory'));
    expect(discover?.body.sourceUrl).toBe('https://github.com/org/repo/tree/main/custom/dir');
  });

  it('surfaces a backend error inline and does not open the selection dialog', async () => {
    const el = await createPanel('project', [], {
      discover: {
        status: 400,
        body: { error: { code: 'discover_failed', message: 'no skills found at ...' } },
      },
    });

    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();

    expect(el.discoveryDialogOpen).toBe(false);
    expect(el.discoveryError).toBeTruthy();
  });

  it('treats an empty skills array as an error, not an empty dialog', async () => {
    const el = await createPanel('project', [], {
      discover: { status: 200, body: { skills: [], count: 0 } },
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

  it('captures the backend skipped[] list and explains it in the dialog', async () => {
    // The backend reports child directories it passed over (no SKILL.md, or an
    // unsafe name). Without surfacing them, a folder the user expected simply
    // vanishes from the list with no explanation.
    const el = await createPanel('project', [], {
      discover: {
        status: 200,
        body: {
          skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }],
          skipped: ['not-a-skill'],
          count: 1,
        },
      },
    });

    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();
    await el.updateComplete;

    expect(el.skippedSkillNames).toEqual(['not-a-skill']);
    const note = el.shadowRoot.querySelector('.discovery-skipped-note');
    expect(note).toBeTruthy();
    // Collapse the template's incidental whitespace before matching.
    expect(note.textContent.replace(/\s+/g, ' ').trim()).toBe(
      '1 folder not recognized as skills was skipped.'
    );
  });

  it('pluralizes the skipped note and omits it when nothing was skipped', async () => {
    const el = await createPanel('project', [], {
      discover: {
        status: 200,
        body: {
          skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }],
          skipped: ['not-a-skill', 'docs'],
          count: 1,
        },
      },
    });
    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el.handleDiscoverDirectory();
    await el.updateComplete;

    expect(
      el.shadowRoot.querySelector('.discovery-skipped-note').textContent.replace(/\s+/g, ' ').trim()
    ).toBe('2 folders not recognized as skills were skipped.');

    // A response with no skipped[] must not render an empty note.
    const clean = await createPanel('project', [], {
      discover: {
        status: 200,
        body: { skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }], count: 1 },
      },
    });
    clean.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await clean.handleDiscoverDirectory();
    await clean.updateComplete;

    expect(clean.skippedSkillNames).toEqual([]);
    expect(clean.shadowRoot.querySelector('.discovery-skipped-note')).toBeNull();
  });

  it('holds discoveryLoading for the duration of the probe, on success and on failure', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });

    const el = await createPanel('project', [], {
      discoverGate: gate,
      discover: { status: 200, body: { skills: [{ uri: 'gh://org/repo/a@main', name: 'a' }], count: 1 } },
    });
    el.dialogUri = 'https://github.com/org/repo/tree/main/skills';

    const inFlight = el.handleDiscoverDirectory();
    expect(el.discoveryLoading).toBe(true);
    release();
    await inFlight;
    expect(el.discoveryLoading).toBe(false);

    // Same guarantee on the error path.
    const failing: Call[] = [];
    const el2 = await createPanel('project', failing, {
      discover: { status: 500, body: { error: { message: 'boom' } } },
    });
    el2.dialogUri = 'https://github.com/org/repo/tree/main/skills';
    await el2.handleDiscoverDirectory();
    expect(el2.discoveryLoading).toBe(false);
    expect(el2.discoveryError).toBeTruthy();
  });
});

describe('injected-skills-panel — selection dialog interaction', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(cleanup);

  /** Mount a panel with the selection dialog open over three discovered skills. */
  async function withSelection(calls: Call[], scope: 'project' | 'hub' = 'project') {
    const el = await createPanel(scope, calls);
    el.discoveredSkills = [
      { uri: 'gh://org/repo/a@main', name: 'a' },
      { uri: 'gh://org/repo/b@main', name: 'b' },
      { uri: 'gh://org/repo/c@main', name: 'c' },
    ];
    el.selectedSkillURIs = new Set(el.discoveredSkills.map((s: any) => s.uri));
    el.discoveryDialogOpen = true;
    await el.updateComplete;
    return el;
  }

  function selectAllCheckbox(el: any) {
    return el.shadowRoot.querySelector('.selection-header sl-checkbox');
  }

  function skillCheckboxes(el: any) {
    return [...el.shadowRoot.querySelectorAll('.selection-item sl-checkbox')];
  }

  it('Select All clears and restores the whole selection', async () => {
    const el = await withSelection([]);
    const header: any = selectAllCheckbox(el);
    expect(header).toBeTruthy();

    header.checked = false;
    header.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(el.selectedSkillURIs.size).toBe(0);

    header.checked = true;
    header.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(el.selectedSkillURIs.size).toBe(3);
  });

  it('marks Select All indeterminate when only some skills are selected', async () => {
    const el = await withSelection([]);
    expect(selectAllCheckbox(el).hasAttribute('indeterminate')).toBe(false);

    const [first]: any[] = skillCheckboxes(el);
    first.checked = false;
    first.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;

    expect(el.selectedSkillURIs.size).toBe(2);
    expect(selectAllCheckbox(el).hasAttribute('indeterminate')).toBe(true);
  });

  it('toggling one skill assigns a new Set instance so Lit re-renders', async () => {
    const el = await withSelection([]);
    const before = el.selectedSkillURIs;

    const [first]: any[] = skillCheckboxes(el);
    first.checked = false;
    first.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;

    expect(el.selectedSkillURIs).not.toBe(before);
    expect(el.selectedSkillURIs.has('gh://org/repo/a@main')).toBe(false);

    first.checked = true;
    first.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(el.selectedSkillURIs.has('gh://org/repo/a@main')).toBe(true);
  });

  it('closing the selection dialog makes no network calls', async () => {
    const calls: Call[] = [];
    const el = await withSelection(calls);

    const dialog = el.shadowRoot.querySelector('sl-dialog[label="Select Skills to Add"]');
    expect(dialog).toBeTruthy();
    dialog.dispatchEvent(new Event('sl-request-close'));
    await el.updateComplete;

    expect(el.discoveryDialogOpen).toBe(false);
    expect(calls).toHaveLength(0);
  });
});

describe('injected-skills-panel — addEntries batching', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(cleanup);

  it('hub scope writes all selected skills in exactly one PUT', async () => {
    const calls: Call[] = [];
    const el = await createPanel('hub', calls);

    // Pretend the hub already holds one system entry and one user entry: the
    // PUT must preserve the user entry and drop the readonly system one.
    el.rows = [row('skill://sys/one', true), row('gh://org/repo/existing@main')];

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

  it('hub scope does not duplicate a URI that is already present', async () => {
    // setHubInjectedSkills stores whatever list it is handed, so a re-discovery
    // of the same directory would otherwise append the same URI twice.
    const calls: Call[] = [];
    const el = await createPanel('hub', calls);
    el.rows = [row('gh://org/repo/a@main')];

    await el.addEntries(['gh://org/repo/a@main', 'gh://org/repo/b@main']);

    const puts = calls.filter((c) => c.method === 'PUT');
    expect(puts).toHaveLength(1);
    expect(puts[0].body.user_defined.map((r: any) => r.uri)).toEqual([
      'gh://org/repo/a@main',
      'gh://org/repo/b@main',
    ]);
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

  it('project scope skips URIs already present rather than POSTing a guaranteed 409', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls);
    el.rows = [row('gh://org/repo/a@main')];

    await el.addEntries(['gh://org/repo/a@main', 'gh://org/repo/b@main']);

    const posts = calls.filter((c) => c.method === 'POST');
    expect(posts).toHaveLength(1);
    expect(posts[0].body.skillUri).toBe('gh://org/repo/b@main');
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

  it('keeps going past a 409 and reports the failure at the end', async () => {
    // The injected-skill POST endpoint returns 409 on a duplicate skillUri. A
    // loop that aborts on the first failure would leave a partial add that can
    // never be retried, because the skills that did land 409 on the next try.
    const calls: Call[] = [];
    const el = await createPanel('project', calls, {
      postStatus: { 'gh://org/repo/a@main': 409 },
    });

    await expect(
      el.addEntries(['gh://org/repo/a@main', 'gh://org/repo/b@main', 'gh://org/repo/c@main'])
    ).rejects.toThrow(/1 of 3/);

    const posts = calls.filter((c) => c.method === 'POST');
    expect(posts.map((p) => p.body.skillUri)).toEqual([
      'gh://org/repo/a@main',
      'gh://org/repo/b@main',
      'gh://org/repo/c@main',
    ]);
  });

  it('attempts every URI even when one POST fails with a 500', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls, {
      postStatus: { 'gh://org/repo/b@main': 500 },
    });

    await expect(
      el.addEntries(['gh://org/repo/a@main', 'gh://org/repo/b@main', 'gh://org/repo/c@main'])
    ).rejects.toThrow(/gh:\/\/org\/repo\/b@main/);

    const posts = calls.filter((c) => c.method === 'POST');
    expect(posts).toHaveLength(3);
  });
});

describe('injected-skills-panel — handleDiscoveryConfirm', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(cleanup);

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
    const el = await createPanel('hub', calls, { putStatus: 500 });

    el.dialogOpen = true;
    el.discoveryDialogOpen = true;
    el.discoveredSkills = [{ uri: 'gh://org/repo/a@main', name: 'a' }];
    el.selectedSkillURIs = new Set(['gh://org/repo/a@main']);

    await el.handleDiscoveryConfirm();

    expect(el.discoveryDialogOpen).toBe(true);
    expect(el.discoveryError).toBeTruthy();
  });

  it('closes both dialogs silently when every selected URI is already present', async () => {
    // addEntries() returns early when nothing is fresh, so no request is made.
    // That is intentional — an all-duplicates batch has nothing to write — but
    // the silence is easy to break by accident, so pin it.
    const calls: Call[] = [];
    const el = await createPanel('hub', calls);

    el.rows = [row('gh://org/repo/a@main'), row('gh://org/repo/b@main')];
    el.dialogOpen = true;
    el.discoveryDialogOpen = true;
    el.discoveredSkills = [
      { uri: 'gh://org/repo/a@main', name: 'a' },
      { uri: 'gh://org/repo/b@main', name: 'b' },
    ];
    el.selectedSkillURIs = new Set(el.discoveredSkills.map((s: any) => s.uri));

    await el.handleDiscoveryConfirm();

    expect(calls).toHaveLength(0);
    expect(el.discoveryDialogOpen).toBe(false);
    expect(el.dialogOpen).toBe(false);
    expect(el.discoveryError).toBeNull();
    expect(el.discoveryLoading).toBe(false);
  });

  it('reports a partial project-scope batch without losing the successful adds', async () => {
    const calls: Call[] = [];
    const el = await createPanel('project', calls, {
      postStatus: { 'gh://org/repo/b@main': 409 },
    });

    el.dialogOpen = true;
    el.discoveryDialogOpen = true;
    el.discoveredSkills = [
      { uri: 'gh://org/repo/a@main', name: 'a' },
      { uri: 'gh://org/repo/b@main', name: 'b' },
      { uri: 'gh://org/repo/c@main', name: 'c' },
    ];
    el.selectedSkillURIs = new Set(el.discoveredSkills.map((s: any) => s.uri));

    await el.handleDiscoveryConfirm();

    // a and c landed; the dialog stays open naming b.
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(3);
    expect(el.discoveryDialogOpen).toBe(true);
    expect(el.discoveryError).toContain('gh://org/repo/b@main');
    expect(el.discoveryLoading).toBe(false);
  });
});

describe('injected-skills-panel — discovery state lifecycle', () => {
  beforeAll(async () => {
    await import('./injected-skills-panel.js');
  });

  afterEach(cleanup);

  it('openDialog() clears stale discovery state', async () => {
    const el = await createPanel('project', []);
    el.discoveryError = 'stale error';
    el.discoveredSkills = [{ uri: 'gh://org/repo/a@main', name: 'a' }];
    el.skippedSkillNames = ['stale-folder'];
    el.selectedSkillURIs = new Set(['gh://org/repo/a@main']);
    el.discoveryDialogOpen = true;

    el.openDialog();

    expect(el.discoveryError).toBeNull();
    expect(el.discoveredSkills).toEqual([]);
    expect(el.skippedSkillNames).toEqual([]);
    expect(el.selectedSkillURIs.size).toBe(0);
    expect(el.discoveryDialogOpen).toBe(false);
    expect(el.dialogOpen).toBe(true);
  });

  it('closeDialog() clears discovery state so a Cancel does not leave a stale error', async () => {
    const el = await createPanel('project', []);
    el.dialogOpen = true;
    el.discoveryError = 'no skills found at ...';
    el.discoveredSkills = [{ uri: 'gh://org/repo/a@main', name: 'a' }];
    el.skippedSkillNames = ['stale-folder'];

    el.closeDialog();

    expect(el.dialogOpen).toBe(false);
    expect(el.discoveryError).toBeNull();
    expect(el.discoveredSkills).toEqual([]);
    expect(el.skippedSkillNames).toEqual([]);
  });
});
