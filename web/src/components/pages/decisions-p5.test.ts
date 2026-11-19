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
 * Unit tests for Decisions dismiss P5 (ptone/scion#8217):
 * - Made tab dismissal entries, Provenance = Dismissed filter, and plain-text reason sink
 * - Multi-select on Waiting on you (project and profile pages)
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import type {
  ProjectDecisionLogEntryView,
  ProjectDecisionProjectRef,
  ProjectMadeDecisionsResponse,
  ProjectPendingDecisionsResponse,
} from '../../client/project-decisions.js';
import {
  resetProfileDecisionProbeCacheForTests,
  type ProfileMadeByProjectDecisionsResponse,
  type ProfileMadeRecentDecisionsResponse,
  type ProfilePendingDecisionsResponse,
} from '../../client/profile-decisions.js';
import {
  DECISIONS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import { setPreferredTimeZone } from '../../utils/time.js';

const showToast = vi.hoisted(() => vi.fn());
vi.mock('../../utils/toast.js', () => ({ showToast }));

class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
    setTimeout(() => {
      if (this.readyState === FakeEventSource.CLOSED) return;
      this.readyState = FakeEventSource.OPEN;
      this.onopen?.(new Event('open'));
    }, 0);
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function jsonResponse(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

import '@shoelace-style/shoelace/dist/components/radio-group/radio-group.js';
import '@shoelace-style/shoelace/dist/components/radio-button/radio-button.js';
await import('./project-decisions.js');
await import('./profile-decisions.js');
type ScionPageProjectDecisions = import('./project-decisions.js').ScionPageProjectDecisions;
type ScionPageProfileDecisions = import('./profile-decisions.js').ScionPageProfileDecisions;
type ScionDecisionsFilterPanel =
  import('../shared/decisions-filter-panel.js').ScionDecisionsFilterPanel;

const PROJ_SCION: ProjectDecisionProjectRef = { id: 'p-1', name: 'scion', slug: 'scion' };
const AGENT_REL = '11111111-2222-3333-4444-555555555555';

function makeDismissLogEntry(
  over: Partial<ProjectDecisionLogEntryView> = {}
): ProjectDecisionLogEntryView {
  return {
    project: PROJ_SCION,
    answerId: 'ans-dsm-1',
    decision: {
      rowId: 'r-dsm-1',
      promptMessageId: 'm-dsm-1',
      promptAvailable: true,
      key: 'D234',
      ref: 'D234',
      version: 1,
      question: 'Ship v0.9 today?',
      askerAgentId: AGENT_REL,
      askerAgentName: 'release-bot',
    },
    choiceId: '$dismissed',
    label: 'Dismissed',
    matchedDefault: false,
    answeredByUserId: 'u-me',
    answeredByName: 'Alice',
    provenance: 'direct',
    outcome: 'dismiss',
    dismissReason: 'Stale: the release already shipped.',
    surface: 'decisions_page',
    at: '2026-11-16T09:12:00Z',
    chatKey: 'topic-release',
    conversationLabel: '#release',
    conversationAvailable: true,
    sourceMessageAvailable: false,
    ...over,
  } as ProjectDecisionLogEntryView;
}

async function settle(
  el: HTMLElement & { updateComplete: Promise<boolean> },
  ms = 15
): Promise<void> {
  await new Promise((r) => setTimeout(r, ms));
  await el.updateComplete;
  const panel = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
    'scion-decisions-filter-panel'
  );
  await panel?.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

describe('Made tab dismissals and Provenance = Dismissed (ptone/scion#8217)', () => {
  let requests: { method: string; url: string; body?: unknown }[];
  let madeResponse: ProjectMadeDecisionsResponse;
  let profileMadeByProject: Omit<ProfileMadeByProjectDecisionsResponse, 'sort'>;
  let profileMadeRecent: Omit<ProfileMadeRecentDecisionsResponse, 'sort'>;

  beforeAll(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    window.localStorage.removeItem('scion:decisions:filters-open');
    resetServerFlagStateForTests();
    resetProfileDecisionProbeCacheForTests();
    setPreferredTimeZone('UTC');
    setFeatureFlag(DECISIONS_FLAG, true);
    showToast.mockReset();
    requests = [];

    const emptyPending: ProjectPendingDecisionsResponse = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      prompts: [],
      agents: [{ id: AGENT_REL, name: 'release-bot' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
    };
    const emptyProfilePending: ProfilePendingDecisionsResponse = {
      ...emptyPending,
      projects: [PROJ_SCION],
      truncatedProjects: false,
    };

    madeResponse = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      entries: [makeDismissLogEntry()],
      agents: [{ id: AGENT_REL, name: 'release-bot' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
    };
    profileMadeByProject = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      groups: [{ project: PROJ_SCION, entries: [makeDismissLogEntry()], more: false }],
      agents: [{ id: AGENT_REL, name: 'release-bot', projectId: 'p-1' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      projects: [PROJ_SCION],
      truncatedProjects: false,
    };
    profileMadeRecent = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      entries: [makeDismissLogEntry()],
      agents: [{ id: AGENT_REL, name: 'release-bot', projectId: 'p-1' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      projects: [PROJ_SCION],
      truncatedProjects: false,
    };

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        const method = (init?.method ?? 'GET').toUpperCase();
        requests.push({ method, url: url.pathname + url.search });

        if (url.pathname === '/api/v1/projects/p-1') {
          return jsonResponse(PROJ_SCION);
        }
        if (url.pathname === '/auth/me' || url.pathname === '/api/v1/auth/me') {
          return jsonResponse({ id: 'u-me', name: 'Alice' });
        }
        if (url.pathname === '/api/v1/projects/p-1/decisions') {
          const tab = url.searchParams.get('tab') ?? 'pending';
          return tab === 'made' ? jsonResponse(madeResponse) : jsonResponse(emptyPending);
        }
        if (url.pathname === '/api/v1/users/me/decisions') {
          const tab = url.searchParams.get('tab') ?? 'pending';
          const sort = url.searchParams.get('sort') ?? 'project';
          if (tab !== 'made') return jsonResponse(emptyProfilePending);
          return sort === 'project'
            ? jsonResponse({ sort: 'project', ...profileMadeByProject })
            : jsonResponse({ sort: 'recent', ...profileMadeRecent });
        }
        return jsonResponse({}, 404);
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('renders a dismiss entry on project Made (desktop table and mobile list) with Dismissed, x-circle badge, plain-text reason, and no $dismissed pill (X6)', async () => {
    madeResponse.entries = [
      makeDismissLogEntry({
        answerId: 'ans-dsm-1',
        label: '', // even if label is omitted, never falls back to $dismissed
        dismissReason: 'Stale: the release already shipped.',
      }),
      makeDismissLogEntry({
        answerId: 'ans-dsm-2',
        decision: {
          rowId: 'r-dsm-2',
          promptMessageId: 'm-dsm-2',
          promptAvailable: true,
          key: 'migrate',
          ref: 'migrate',
          version: 1,
          question: 'Run the backfill on staging first?',
          askerAgentId: 'a-ncx',
          askerAgentName: 'nc-x',
        },
        dismissReason: undefined,
      }),
    ];
    window.history.replaceState({}, '', '/projects/p-1/decisions?tab=made&provenance=dismissed');
    const el = document.createElement('scion-page-project-decisions') as ScionPageProjectDecisions;
    el.pageData = {
      path: '/projects/p-1/decisions?tab=made&provenance=dismissed',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(el);
    await settle(el, 25);

    expect(
      requests.some((r) => r.url === '/api/v1/projects/p-1/decisions?tab=made&provenance=dismissed')
    ).toBe(true);

    const root = el.shadowRoot!;
    const row1 = root.querySelector<HTMLElement>('tr[data-answer-row="ans-dsm-1"]')!;
    expect(row1).not.toBeNull();
    expect(row1.textContent).toContain('D234');
    expect(row1.textContent).toContain('Ship v0.9 today?');
    expect(row1.textContent).toContain('Dismissed');
    expect(row1.textContent).toContain('Stale: the release already shipped.');
    expect(row1.textContent).toContain('release-bot');
    expect(row1.textContent).toContain('#release');
    expect(row1.textContent).not.toContain('$dismissed');

    const badge = row1.querySelector<HTMLElement>('[data-provenance] .badge.dec')!;
    expect(badge).not.toBeNull();
    expect(badge.textContent?.trim()).toBe('dismissed');
    expect(badge.querySelector('sl-icon')?.getAttribute('name')).toBe('x-circle');

    // Second row has no dismissReason -> no secondary reason element rendered
    const row2 = root.querySelector<HTMLElement>('tr[data-answer-row="ans-dsm-2"]')!;
    expect(row2.querySelector('[data-dismiss-reason]')).toBeNull();

    // Mobile stacked list item also renders Dismissed, reason, and x-circle badge without $dismissed
    const mobile1 = root.querySelector<HTMLElement>('[data-mobile-answer="ans-dsm-1"]')!;
    expect(mobile1.textContent).toContain('D234');
    expect(mobile1.textContent).toContain('Dismissed');
    expect(mobile1.textContent).toContain('Ship v0.9 today?');
    expect(mobile1.textContent).toContain('Stale: the release already shipped.');
    expect(mobile1.textContent).not.toContain('$dismissed');
    expect(mobile1.querySelector('.badge.dec sl-icon')?.getAttribute('name')).toBe('x-circle');
  });

  it('escapes HTML/Markdown payloads in dismissReason as inert plain text on project and profile Made (sink test)', async () => {
    const payload = '<img src=x onerror="window.__xss=1"> **not bold** <script>alert(1)</script>';
    madeResponse.entries = [makeDismissLogEntry({ dismissReason: payload })];
    profileMadeByProject.groups = [
      {
        project: PROJ_SCION,
        entries: [makeDismissLogEntry({ dismissReason: payload })],
        more: false,
      },
    ];

    window.history.replaceState({}, '', '/projects/p-1/decisions?tab=made');
    const projEl = document.createElement(
      'scion-page-project-decisions'
    ) as ScionPageProjectDecisions;
    projEl.pageData = {
      path: '/projects/p-1/decisions?tab=made',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(projEl);
    await settle(projEl, 25);

    const tableReason = projEl.shadowRoot!.querySelector<HTMLElement>(
      'tr[data-answer-row="ans-dsm-1"] [data-dismiss-reason]'
    )!;
    expect(tableReason).not.toBeNull();
    expect(tableReason.textContent).toBe(payload);
    expect(tableReason.querySelector('img, script, strong, b')).toBeNull();

    // Profile Made (sort=project grouped view)
    projEl.remove();
    window.history.replaceState(
      {},
      '',
      '/profile/decisions?tab=made&sort=project&provenance=dismissed'
    );
    const profEl = document.createElement(
      'scion-page-profile-decisions'
    ) as ScionPageProfileDecisions;
    profEl.pageData = {
      path: '/profile/decisions?tab=made&sort=project&provenance=dismissed',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(profEl);
    await settle(profEl, 25);

    expect(
      requests.some(
        (r) => r.url === '/api/v1/users/me/decisions?tab=made&sort=project&provenance=dismissed'
      )
    ).toBe(true);
    const profReason = profEl.shadowRoot!.querySelector<HTMLElement>(
      '[data-mobile-answer="ans-dsm-1"] [data-dismiss-reason]'
    )!;
    expect(profReason).not.toBeNull();
    expect(profReason.textContent).toBe(payload);
    expect(profReason.querySelector('img, script, strong, b')).toBeNull();
  });
});
