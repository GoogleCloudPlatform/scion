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
 * `/projects` live updates (webperf C10): `projects-updated` merges in place
 * (only projects whose store object changed are re-merged; the array keeps
 * its identity when nothing changed) and rows are keyed by project ID.
 * Exercises the real `stateManager` singleton; only `fetch`, `EventSource`
 * and `localStorage` are faked.
 */

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import './projects.js';
import { stateManager } from '../../client/state.js';
import type { Project } from '../../shared/types.js';

/** happy-dom has no EventSource; setScope opens one. */
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

type TestEl = HTMLElement & { projects: Project[]; updateComplete: Promise<boolean> };

function project(id: string, overrides: Partial<Project> = {}): Project {
  return {
    id,
    name: id,
    slug: id,
    agentCount: 0,
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    ...overrides,
  } as Project;
}

function handleUpdate(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({ subject, data });
}

beforeAll(() => {
  const store = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
});

describe('scion-page-projects live updates (keyed, in place)', () => {
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    // Force a real scope change so the dashboard maps start empty.
    stateManager.setScope({ type: 'brokers-list' });
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-page-projects').forEach((n) => n.remove());
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  async function mount(view: 'grid' | 'list'): Promise<TestEl> {
    localStorage.setItem('scion-view-projects', view);
    const initial = [project('p1'), project('p2'), project('p3')];
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ projects: initial, _capabilities: { actions: [] } }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
    const el = document.createElement('scion-page-projects') as TestEl;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    return el;
  }

  it('an update to one project keeps every other project object and the array when nothing changed', async () => {
    const el = await mount('grid');
    const before = el.projects;
    expect(before.map((p) => p.id)).toEqual(['p1', 'p2', 'p3']);

    // A notification with no store change: nothing is re-merged.
    stateManager.dispatchEvent(new CustomEvent('projects-updated'));
    await el.updateComplete;
    expect(el.projects).toBe(before);

    handleUpdate('project.p2.updated', { projectId: 'p2', name: 'renamed' });
    await el.updateComplete;
    expect(el.projects).not.toBe(before);
    expect(el.projects[0]).toBe(before[0]);
    expect(el.projects[2]).toBe(before[2]);
    expect(el.projects[1].name).toBe('renamed');

    // The merged p2 is kept on the next unrelated update.
    const p2 = el.projects[1];
    handleUpdate('project.p3.updated', { projectId: 'p3', agentCount: 4 });
    await el.updateComplete;
    expect(el.projects[1]).toBe(p2);
    expect(el.projects[2].agentCount).toBe(4);
  });

  for (const [view, rowOf] of [
    [
      'grid',
      (el: TestEl, id: string): Element | null | undefined =>
        el.shadowRoot?.querySelector(`a[href="/projects/${id}"]`),
    ],
    [
      'list',
      (el: TestEl, id: string): Element | null | undefined =>
        [...(el.shadowRoot?.querySelectorAll('tbody tr') ?? [])].find((tr) =>
          tr.querySelector('.name-cell')?.textContent?.includes(id)
        ),
    ],
  ] as const) {
    it(`projects list keeps row DOM nodes when another project is deleted (${view})`, async () => {
      const el = await mount(view);
      const p2Row = rowOf(el, 'p2');
      const p3Row = rowOf(el, 'p3');
      expect(p2Row && p3Row).toBeTruthy();

      handleUpdate('project.p1.deleted', {});
      await el.updateComplete;

      expect(el.projects.map((p) => p.id)).toEqual(['p2', 'p3']);
      expect(rowOf(el, 'p1')).toBeFalsy();
      expect(rowOf(el, 'p2')).toBe(p2Row);
      expect(rowOf(el, 'p3')).toBe(p3Row);
    });
  }
});
