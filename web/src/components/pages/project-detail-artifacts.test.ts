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
 * Project page Files area with artifacts on (experiment hub.artifacts):
 * one area with an Artifacts | Shared dirs | Workspace switcher, Artifacts
 * first and the default; with artifacts off the Files area is unchanged.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import type { PageData } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

const PROJECT_ID = 'p-1';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

type TestElement = HTMLElement & {
  updateComplete: Promise<boolean>;
  pageData: PageData | null;
  projectId: string;
};

let element: TestElement | null = null;
let urls: string[] = [];

async function settle(el: TestElement): Promise<void> {
  for (let i = 0; i < 10; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(flag: boolean, sharedDirs = [{ name: 'datasets' }]): Promise<TestElement> {
  window.__SCION_FEATURES__ = { 'hub.artifacts': flag };
  urls = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      urls.push(url);
      if (url.startsWith('/api/v1/artifacts?')) {
        return Promise.resolve(json({ artifacts: [] }));
      }
      if (url.includes('/agents')) {
        return Promise.resolve(json({ agents: [], _capabilities: { actions: [] } }));
      }
      if (url.includes('/files')) {
        return Promise.resolve(json({ files: [], totalSize: 0, totalCount: 0 }));
      }
      if (url.endsWith(`/api/v1/projects/${PROJECT_ID}`)) {
        return Promise.resolve(
          json({
            id: PROJECT_ID,
            name: 'Project One',
            slug: 'project-one',
            sharedDirs,
            _capabilities: { actions: ['read', 'update'] },
          })
        );
      }
      return Promise.resolve(json({}, 404));
    })
  );
  const el = document.createElement('scion-page-project-detail') as TestElement;
  el.projectId = PROJECT_ID;
  el.pageData = {
    path: `/projects/${PROJECT_ID}`,
    title: 'Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  document.body.appendChild(el);
  element = el;
  await settle(el);
  return el;
}

async function pickSegment(el: TestElement, value: string): Promise<void> {
  const group = el.shadowRoot!.querySelector('sl-radio-group.files-segments') as HTMLElement & {
    value: string;
  };
  group.value = value;
  group.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
  await settle(el);
}

describe('project Files area with artifacts', () => {
  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    vi.stubGlobal('IntersectionObserver', undefined);
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    delete window.__SCION_FEATURES__;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('opens on the Artifacts segment and lists the project artifacts', async () => {
    const el = await mount(true);
    const segments = Array.from(
      el.shadowRoot!.querySelectorAll('.files-segments sl-radio-button')
    ).map((b) => b.textContent!.trim());
    expect(segments).toEqual(['Artifacts', 'Shared dirs', 'Workspace']);
    const list = el.shadowRoot!.querySelector('scion-artifact-list') as HTMLElement & {
      projectId: string;
    };
    expect(list).not.toBeNull();
    expect(list.projectId).toBe(PROJECT_ID);
    expect(el.shadowRoot!.querySelector('scion-file-browser')).toBeNull();
    await settle(el);
    expect(urls).toContain(`/api/v1/artifacts?mine=1&scope=${PROJECT_ID}`);
  });

  it('switches to the shared dirs and the workspace with the existing file browser', async () => {
    const el = await mount(true);
    await pickSegment(el, 'shared');
    expect(el.shadowRoot!.querySelector('scion-artifact-list')).toBeNull();
    expect(
      Array.from(el.shadowRoot!.querySelectorAll('sl-tab')).map((t) => t.textContent!.trim())
    ).toEqual(['datasets']);
    expect(el.shadowRoot!.querySelector('scion-file-browser[data-tab="datasets"]')).not.toBeNull();

    await pickSegment(el, 'workspace');
    expect(
      Array.from(el.shadowRoot!.querySelectorAll('sl-tab')).map((t) => t.textContent!.trim())
    ).toEqual(['workspace']);
    expect(el.shadowRoot!.querySelector('scion-file-browser[data-tab="workspace"]')).not.toBeNull();
  });

  it('leaves out the Shared dirs segment when the project has none', async () => {
    const el = await mount(true, []);
    const segments = Array.from(
      el.shadowRoot!.querySelectorAll('.files-segments sl-radio-button')
    ).map((b) => b.textContent!.trim());
    expect(segments).toEqual(['Artifacts', 'Workspace']);
  });

  it('keeps the Files area as it was when artifacts are off', async () => {
    const el = await mount(false);
    expect(el.shadowRoot!.querySelector('.files-segments')).toBeNull();
    expect(el.shadowRoot!.querySelector('scion-artifact-list')).toBeNull();
    expect(
      Array.from(el.shadowRoot!.querySelectorAll('sl-tab')).map((t) => t.textContent!.trim())
    ).toEqual(['workspace', 'datasets']);
    expect(urls.some((u) => u.startsWith('/api/v1/artifacts'))).toBe(false);
  });
});
