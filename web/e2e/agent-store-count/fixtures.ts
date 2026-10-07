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
 * The full app over a mocked hub, for counting agent-list requests: 450
 * agents served page by page (`limit` and `cursor` honoured, optionally
 * delayed), so a store walk reads three pages and the chat sidebar's five;
 * a stubbed `EventSource` the test can push hub events through; and a log
 * of every agent-list request.
 */

import type { Page, Request } from '@playwright/test';

export const PROJECT_ID = 'project-one';
export const AGENT_COUNT = 450;
/**
 * The agent store's page size: every store walk of the hub reads this many
 * rows a page. Must equal `AGENT_STORE_PAGE_SIZE` in `src/client/agent-store.ts`
 * (not imported, so Playwright does not evaluate the store in Node).
 */
export const STORE_PAGE_SIZE = 200;
/** Pages in one store walk of the fixture hub. */
export const STORE_PAGES_PER_WALK = Math.ceil(AGENT_COUNT / STORE_PAGE_SIZE);

/** The `n`th fixture agent's id, 1-based. */
export function agentId(n: number): string {
  return `aaaaaaaa-aaaa-4aaa-8aaa-${n.toString(16).padStart(12, '0')}`;
}

/** The `n`th fixture agent's name, 1-based. */
export function agentName(n: number): string {
  return `agent-${n.toString().padStart(3, '0')}`;
}

interface FixtureAgent {
  id: string;
  name: string;
  slug: string;
  projectId: string;
  project: string;
  phase: string;
  ancestry: string[];
  created: string;
  updated: string;
  _capabilities: { actions: string[] };
  _messageability: { canMessage: boolean };
}

function fixtureAgents(): FixtureAgent[] {
  return Array.from({ length: AGENT_COUNT }, (_, i) => {
    const n = i + 1;
    const at = new Date(Date.UTC(2026, 0, 1, 0, 0, n)).toISOString();
    return {
      id: agentId(n),
      name: agentName(n),
      slug: agentName(n),
      projectId: PROJECT_ID,
      project: 'Project One',
      phase: 'running',
      ancestry: ['fixture-user'],
      created: at,
      updated: at,
      _capabilities: { actions: ['attach', 'lifecycle'] },
      _messageability: { canMessage: true },
    };
  });
}

/** The hub and project agent-list endpoints. */
const AGENT_LIST_PATH = /^\/api\/v1\/(?:agents|projects\/[^/]+\/agents)$/;
const SINGLE_AGENT_PATH = /^\/api\/v1\/agents\/([^/]+)$/;
const PROJECT_PATH = /^\/api\/v1\/projects\/([^/]+)$/;

/** One agent-list GET, classified. */
export interface ListRequest {
  path: string;
  /** The page's own query: `cursor` set means it continues a walk. */
  params: URLSearchParams;
  /** A request that starts a walk: no cursor, no `sort` (a probe sorts). */
  walk: boolean;
  /** A delta probe (`sort=updated`), counted apart from walks. */
  probe: boolean;
  /** Issued by the agent store: the store walks in the compact view. */
  store: boolean;
}

export interface Hub {
  /** Every agent-list GET so far, in order. */
  readonly requests: readonly ListRequest[];
  /** Walks the agent store started. */
  storeWalks(): number;
  /** Walks started by anything other than the store (the chat sidebar, a page's own fetch). */
  otherWalks(): number;
  /** Every agent-list request: walks, their later pages, and probes. */
  total(): number;
  /** `GET /api/v1/users` requests (the chat sidebar's users walk). */
  usersRequests(): number;
  /** The `cursor` of every store page request after a walk's first, in order. */
  storeCursors(): string[];
  /**
   * Holds the response to the next first page of a store walk until the
   * returned function is called, so a walk stays in flight.
   */
  holdNextStoreWalk(): () => void;
  /** Delivers a hub event through every open stub `EventSource`. */
  emit(subject: string, data: unknown): Promise<void>;
}

function classify(request: Request): ListRequest | null {
  if (request.method() !== 'GET') return null;
  const url = new URL(request.url());
  if (!AGENT_LIST_PATH.test(url.pathname)) return null;
  const params = url.searchParams;
  const probe = params.has('sort');
  return {
    path: `${url.pathname}${url.search}`,
    params,
    walk: !probe && !params.has('cursor'),
    probe,
    store: params.get('view') === 'compact',
  };
}

function json(body: unknown, status = 200): { status: number; json: unknown } {
  return { status, json: body };
}

/**
 * Mocks the hub for `page`. `latencyMs` delays every agent-list response,
 * which changes how the chat sidebar's walk and the palettes' loads overlap.
 */
export async function setupHub(page: Page, options: { latencyMs?: number } = {}): Promise<Hub> {
  const latencyMs = options.latencyMs ?? 0;
  const agents = fixtureAgents();
  const requests: ListRequest[] = [];
  let users = 0;
  let hold: Promise<void> | null = null;

  page.on('request', (request) => {
    const listRequest = classify(request);
    if (listRequest) requests.push(listRequest);
    if (request.method() === 'GET' && new URL(request.url()).pathname === '/api/v1/users') users++;
  });
  await installPaletteFinder(page);

  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': true, 'web.native_chat': true };
    const streams: Array<EventTarget & { onopen: (() => void) | null }> = [];
    (window as unknown as { __hubStreams: typeof streams }).__hubStreams = streams;
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      onerror: (() => void) | null = null;
      readyState = 0;
      constructor(public url: string) {
        super();
        streams.push(this);
        queueMicrotask(() => {
          this.readyState = 1;
          this.onopen?.();
        });
      }
      close(): void {
        this.readyState = 2;
        const i = streams.indexOf(this);
        if (i >= 0) streams.splice(i, 1);
      }
    } as unknown as typeof EventSource;
  });

  await page.route('**/auth/me', (route) =>
    route.fulfill(json({ id: 'fixture-user', email: 'fixture@example.test' }))
  );

  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;

    if (AGENT_LIST_PATH.test(path)) {
      const params = url.searchParams;
      if (
        hold &&
        params.get('view') === 'compact' &&
        !params.has('cursor') &&
        !params.has('sort')
      ) {
        const held = hold;
        hold = null;
        await held;
      }
      if (latencyMs > 0) await new Promise((resolve) => setTimeout(resolve, latencyMs));
      const projectMatch = /^\/api\/v1\/projects\/([^/]+)\/agents$/.exec(path);
      const rows = projectMatch ? agents.filter((a) => a.projectId === projectMatch[1]) : agents;
      const limit = Math.min(Number(url.searchParams.get('limit')) || 500, 500);
      const offset = Number(url.searchParams.get('cursor')) || 0;
      const page = rows.slice(offset, offset + limit);
      const next = offset + limit < rows.length ? String(offset + limit) : '';
      await route
        .fulfill(
          json({
            agents: page,
            ...(next ? { nextCursor: next } : {}),
            totalCount: rows.length,
            _capabilities: { actions: ['create'] },
          })
        )
        .catch(() => {});
      return;
    }

    if (path.endsWith('/pty')) return route.fulfill(json({}));
    const single = SINGLE_AGENT_PATH.exec(path);
    if (single) {
      const agent = agents.find((a) => a.id === single[1]);
      return route.fulfill(agent ? json(agent) : json({ error: 'not found' }, 404));
    }
    const project = PROJECT_PATH.exec(path);
    if (project) {
      return route.fulfill(
        json({
          id: project[1],
          name: 'Project One',
          slug: project[1],
          _capabilities: { actions: ['attach', 'create'] },
        })
      );
    }
    if (/^\/api\/v1\/projects\/[^/]+\/metrics/.test(path)) {
      return route.fulfill(json({ error: 'not found' }, 404));
    }

    switch (path) {
      case '/api/v1/settings/public':
        return route.fulfill(json({ nativeChatEnabled: true }));
      case '/api/v1/system/status':
        return route.fulfill(json({ complete: true }));
      case '/api/v1/auth/admin-status':
        return route.fulfill(json({ isAdmin: false, isSuperAdmin: false, permissions: [] }));
      case '/api/v1/projects':
        return route.fulfill(
          json({ projects: [{ id: PROJECT_ID, name: 'Project One', slug: PROJECT_ID }] })
        );
      case '/api/v1/users':
        return route.fulfill(json({ users: [] }));
      case '/api/v1/chat/dms':
        return route.fulfill(json({ dms: [] }));
      case '/api/v1/notifications':
        return route.fulfill(json({ userNotifications: [], subscriptions: [] }));
      default:
        return route.fulfill(json({}));
    }
  });

  await page.routeWebSocket('**/pty?*', (socket) => {
    // The client reaches a connected state on its first inbound data frame.
    socket.send(JSON.stringify({ type: 'data', data: '' }));
  });

  return {
    get requests(): readonly ListRequest[] {
      return requests;
    },
    storeWalks: () => requests.filter((r) => r.walk && r.store).length,
    otherWalks: () => requests.filter((r) => r.walk && !r.store).length,
    total: () => requests.length,
    usersRequests: () => users,
    storeCursors: () =>
      requests
        .filter((r) => r.store && !r.probe && !r.walk)
        .map((r) => r.params.get('cursor') ?? ''),
    holdNextStoreWalk: (): (() => void) => {
      let release = (): void => {};
      hold = new Promise<void>((resolve) => {
        release = resolve;
      });
      return release;
    },
    emit: async (subject, data): Promise<void> => {
      await page.evaluate(
        ({ subject: s, data: d }) => {
          const streams = (window as unknown as { __hubStreams: EventTarget[] }).__hubStreams;
          for (const stream of streams) {
            stream.dispatchEvent(
              new MessageEvent('update', { data: JSON.stringify({ subject: s, data: d }) })
            );
          }
        },
        { subject, data }
      );
    },
  };
}

/** What a quick palette (by its dialog label) currently shows in its Agents group. */
export interface PaletteState {
  open: boolean;
  status: string | undefined;
  agentIds: string[];
}

/**
 * Finds the quick palette labelled `label`, through open shadow roots (the
 * chat page renders its palette in its own). Runs in the page.
 */
function findPalette(label: string): Element | undefined {
  const visit = (root: Document | ShadowRoot): Element | undefined => {
    for (const el of Array.from(root.querySelectorAll('*'))) {
      if (
        el.tagName === 'SCION-QUICK-PALETTE' &&
        (el as unknown as { label: string }).label === label
      ) {
        return el;
      }
      if (el.shadowRoot) {
        const found = visit(el.shadowRoot);
        if (found) return found;
      }
    }
    return undefined;
  };
  return visit(document);
}

/** Installs {@link findPalette} in the page as `window.__findPalette`. */
export async function installPaletteFinder(page: Page): Promise<void> {
  await page.addInitScript(`window.__findPalette = ${findPalette.toString()};`);
}

export function paletteState(page: Page, label: string): Promise<PaletteState | null> {
  return page.evaluate((l) => {
    const find = (window as unknown as { __findPalette: (label: string) => Element | undefined })
      .__findPalette;
    const el = find(l) as unknown as
      | {
          open: boolean;
          groups: {
            agents?: { status: string; candidates: Array<{ target: { agentId?: string } }> };
          };
        }
      | undefined;
    if (!el) return null;
    const agents = el.groups.agents;
    return {
      open: el.open,
      status: agents?.status,
      agentIds: (agents?.candidates ?? []).map((c) => c.target.agentId ?? ''),
    };
  }, label);
}
