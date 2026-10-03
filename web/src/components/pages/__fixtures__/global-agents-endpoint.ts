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
 * A fake global agents endpoint shared by the agents page and home tests,
 * plus the projects, invite-stats and lifecycle endpoints those pages call.
 */

import type { Agent, Capabilities } from '../../../shared/types.js';

/** happy-dom has no EventSource; it opens on the next tick, so a drain's wait for it resolves at once. */
export class FakeEventSource extends EventTarget {
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

export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

export function makeAgent(i: number, overrides: Partial<Agent> = {}): Agent {
  const s = String(i).padStart(5, '0');
  return {
    id: `g-${s}`,
    name: `agent-${s}`,
    projectId: `p-${i % 7}`,
    template: 't',
    phase: 'running',
    created: `2026-01-01T00:00:00.${s}Z`,
    updated: `2026-01-02T00:00:00.${s}Z`,
    messageMode: i % 2 === 0 ? 'project' : 'branch',
    labels: { env: 'prod' },
    _capabilities: { actions: ['read', 'update', 'delete', 'stop_all'] },
    ...overrides,
  } as Agent;
}

/** Ids in the fake `mine` and `shared` scopes. */
export const isMine = (a: Agent): boolean => Number(a.id.slice(2)) % 2 === 0;
export const isShared = (a: Agent): boolean => Number(a.id.slice(2)) % 3 === 0;

export const SCOPE_CAPS: Capabilities = { actions: ['create', 'stop_all'] };

export interface Fake {
  agents: Agent[];
  requests: string[];
  /** Label values answered with a 400. */
  badLabels?: Set<string>;
  /** Every agents GET fails with a 500. */
  failAll?: boolean;
  /** The projects list answered by `/api/v1/projects`. */
  projects?: Array<{ id: string; name: string }>;
  /** Every non-agents GET (projects, invite stats), in order. */
  otherRequests?: string[];
}

/**
 * A fake global agents endpoint: sorted fit requests (complete when the set
 * fits), sorted cursor pages, `stats` (IDs omitted above 2,000), scope,
 * k=v label and phase; legacy (unsorted) cursor pages of 500.
 */
export function fakeFetch(fake: Fake) {
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const rawUrl =
      typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const u = new URL(rawUrl, 'http://localhost');
    const method = (
      init?.method ?? (input instanceof Request ? input.method : 'GET')
    ).toUpperCase();

    if (u.pathname.startsWith('/api/v1/agents/') && method !== 'GET') {
      return Promise.resolve(jsonResponse({ stopped: 0, failed: 0 }));
    }
    if (u.pathname === '/api/v1/projects' && method === 'GET') {
      fake.otherRequests?.push(rawUrl);
      return Promise.resolve(
        jsonResponse({ projects: fake.projects ?? [], _capabilities: { actions: ['create'] } })
      );
    }
    if (u.pathname === '/api/v1/admin/invites/stats' && method === 'GET') {
      fake.otherRequests?.push(rawUrl);
      return Promise.resolve(
        jsonResponse({
          pendingInvites: 3,
          totalRedemptions: 7,
          allowListCount: 11,
          recentRedemptions: [],
        })
      );
    }
    if (u.pathname !== '/api/v1/agents' || method !== 'GET') {
      return Promise.resolve(jsonResponse({}, 404));
    }

    fake.requests.push(rawUrl);
    if (fake.failAll) {
      return Promise.resolve(jsonResponse({ error: { message: 'boom' } }, 500));
    }
    const label = u.searchParams.get('label') ?? '';
    if (fake.badLabels?.has(label)) {
      return Promise.resolve(jsonResponse({ error: { message: 'invalid label' } }, 400));
    }
    const scope = u.searchParams.get('scope');
    let list = fake.agents;
    if (scope === 'mine') list = list.filter(isMine);
    if (scope === 'shared') list = list.filter(isShared);
    if (label.includes('=')) {
      const [k, v] = [label.slice(0, label.indexOf('=')), label.slice(label.indexOf('=') + 1)];
      list = list.filter((a) => a.labels?.[k] === v);
    }

    const sort = u.searchParams.get('sort');
    if (!sort) {
      const start = Number(u.searchParams.get('cursor') ?? '0');
      const end = start + 500;
      return Promise.resolve(
        jsonResponse({
          agents: list.slice(start, end),
          nextCursor: end < list.length ? String(end) : undefined,
          _capabilities: SCOPE_CAPS,
        })
      );
    }

    const dir = u.searchParams.get('dir') === 'asc' ? 1 : -1;
    const field = sort === 'created' ? 'created' : 'updated';
    const sorted = [...list].sort((a, b) => dir * (a[field] ?? '').localeCompare(b[field] ?? ''));
    const statsOf = (): { total: number; running: number; agents?: Array<[string, string]> } => ({
      total: sorted.length,
      running: sorted.filter((a) => a.phase === 'running').length,
      ...(sorted.length <= 2000
        ? { agents: sorted.map((a) => [a.id, a.phase]) as Array<[string, string]> }
        : {}),
    });
    const phase = u.searchParams.get('phase') ?? '';
    const phased = phase ? sorted.filter((a) => a.phase === phase) : sorted;
    const limit = Number(u.searchParams.get('limit') ?? '25');
    const wantStats = u.searchParams.get('stats') !== null;
    const cursor = u.searchParams.get('cursor');
    const fit = u.searchParams.get('fit');

    if (cursor === null && fit !== null && sorted.length <= Number(fit)) {
      return Promise.resolve(
        jsonResponse({
          agents: sorted,
          totalCount: sorted.length,
          complete: true,
          stats: statsOf(),
          _capabilities: SCOPE_CAPS,
        })
      );
    }
    const start = Number(cursor ?? '0');
    const end = start + limit;
    return Promise.resolve(
      jsonResponse({
        agents: phased.slice(start, end),
        totalCount: phased.length,
        complete: false,
        nextCursor: end < phased.length ? String(end) : undefined,
        ...(wantStats ? { stats: statsOf() } : {}),
        _capabilities: SCOPE_CAPS,
      })
    );
  };
}

type FetchLike = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

function abortError(): Error {
  const err = new Error('aborted');
  err.name = 'AbortError';
  return err;
}

/** One request seen by {@link holdable}: its URL and the signal it was sent with. */
export interface SentRequest {
  url: string;
  signal: AbortSignal | undefined;
}

/**
 * Wraps `inner` so a test can hold the next matching requests until it
 * releases them. A held request rejects with an `AbortError` when its
 * signal aborts, as `fetch` does; a request whose signal is already
 * aborted when released never reaches `inner`. Every matching request is
 * recorded in `sent`, in order, held or not.
 */
export function holdable(inner: FetchLike, matches: (url: URL) => boolean) {
  const sent: SentRequest[] = [];
  const held: Array<() => void> = [];
  let toHold = 0;
  const fn: FetchLike = async (input, init) => {
    const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const signal = init?.signal ?? undefined;
    if (matches(new URL(raw, 'http://localhost'))) {
      sent.push({ url: raw, signal });
      if (toHold > 0) {
        toHold--;
        await new Promise<void>((resolve, reject) => {
          held.push(resolve);
          signal?.addEventListener('abort', () => reject(abortError()), { once: true });
        });
      }
    }
    if (signal?.aborted) throw abortError();
    return inner(input, init);
  };
  return {
    fn,
    sent,
    /** Hold the next `n` matching requests. */
    hold(n = 1): void {
      toHold += n;
    },
    /** How many requests are held right now. */
    get heldCount(): number {
      return held.length;
    },
    /** Let every held request continue. */
    release(): void {
      for (const resolve of held.splice(0)) resolve();
    },
  };
}

/** Matches the global agents list endpoint. */
export const isGlobalAgentsList = (u: URL): boolean => u.pathname === '/api/v1/agents';
