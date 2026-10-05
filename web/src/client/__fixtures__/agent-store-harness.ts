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
 * Test harness for the agent store: a fake EventSource the feed's real
 * SSE client connects through, an in-memory agent-list server honouring
 * `limit`/`cursor`, and a store wired to both with a real `StateManager`
 * feed per connection.
 */

import { vi } from 'vitest';
import type { Agent } from '../../shared/types.js';
import type { ApiFetchOptions } from '../api.js';
import { AgentStore } from '../agent-store.js';
import type { AgentStoreOptions } from '../agent-store.js';
import { StateManager } from '../state.js';

type Listener = (event: Event) => void;

/** Stands in for `EventSource`: opened, fed and dropped by the test. */
export class FakeEventSource {
  static instances: FakeEventSource[] = [];

  readonly url: string;
  readyState = 0;
  closed = false;
  onopen: Listener | null = null;
  onerror: Listener | null = null;
  onmessage: Listener | null = null;
  private readonly listeners = new Map<string, Set<Listener>>();

  constructor(url: string | URL) {
    this.url = String(url);
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: Listener): void {
    let set = this.listeners.get(type);
    if (!set) {
      set = new Set();
      this.listeners.set(type, set);
    }
    set.add(listener);
  }

  removeEventListener(type: string, listener: Listener): void {
    this.listeners.get(type)?.delete(listener);
  }

  close(): void {
    this.closed = true;
    this.readyState = 2;
  }

  /** The browser's open signal. */
  open(): void {
    this.readyState = 1;
    this.onopen?.(new Event('open'));
  }

  /** Deliver one hub update on this stream. */
  emit(subject: string, data: unknown): void {
    const event = new MessageEvent('update', { data: JSON.stringify({ subject, data }) });
    for (const listener of Array.from(this.listeners.get('update') ?? [])) listener(event);
  }

  /** A live connection failing. */
  drop(): void {
    this.onerror?.(new Event('error'));
  }

  /** Subjects this stream subscribed to. */
  get subjects(): string[] {
    return new URL(this.url, 'http://localhost').searchParams.getAll('sub');
  }

  static latestOpen(): FakeEventSource {
    const live = FakeEventSource.instances.filter((es) => !es.closed);
    const latest = live[live.length - 1];
    if (!latest) throw new Error('no open EventSource');
    return latest;
  }
}

export function agent(id: string, extra: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: 'p1',
    template: 'default',
    phase: 'running',
    activity: 'idle',
    ...extra,
  } as Agent;
}

interface Gate {
  promise: Promise<void>;
  release: () => void;
}

function gate(): Gate {
  let release = (): void => {};
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

function abortable(signal: AbortSignal | null | undefined): Promise<never> {
  return new Promise<never>((_resolve, reject) => {
    const fail = (): void => reject(new DOMException('aborted', 'AbortError'));
    if (signal?.aborted) fail();
    signal?.addEventListener('abort', fail);
  });
}

export interface AgentServer {
  /** Rows the server holds, in list order. Mutable. */
  agents: Agent[];
  /** Every request path, in order. */
  requests: string[];
  /** Status for the next responses; 200 serves pages. */
  status: number;
  /** Scope capabilities sent with each page. */
  scopeCapabilities?: Agent['_capabilities'];
  fetch: ReturnType<typeof vi.fn<(path: string, options: ApiFetchOptions) => Promise<Response>>>;
  /** How a project list page renders a row (the hub's omits `_messageability`). */
  projectRow?: (agent: Agent) => Agent;
  /** Hold every request until the returned release is called. */
  pause(): () => void;
  /**
   * Hold single-agent reads until the returned release is called. With
   * `ignoreAbort`, a held read still answers after its signal aborts, as a
   * response already on the wire would.
   */
  holdAgentReads(options?: { ignoreAbort?: boolean }): () => void;
  /** Walks started: list requests without a cursor, optionally for one path prefix. */
  walks(pathPrefix?: string): number;
  /** Single-agent requests (`/api/v1/agents/{id}`), optionally for one id. */
  agentFetches(id?: string): number;
}

const SINGLE_AGENT = /^\/api\/v1\/agents\/([^/?]+)$/;

export function createAgentServer(initial: Agent[] = []): AgentServer {
  let held: Gate | null = null;
  let heldReads: { gate: Gate; ignoreAbort: boolean } | null = null;
  const server: AgentServer = {
    agents: [...initial],
    requests: [],
    status: 200,
    fetch: vi.fn(async (path: string, options: ApiFetchOptions): Promise<Response> => {
      server.requests.push(path);
      const signal = options.signal;
      if (held) await Promise.race([held.promise, abortable(signal)]);
      if (signal?.aborted) throw new DOMException('aborted', 'AbortError');
      const status = server.status;
      const url = new URL(path, 'http://localhost');
      const single = SINGLE_AGENT.exec(url.pathname)?.[1];
      if (single !== undefined && heldReads) {
        const reads = heldReads;
        await (reads.ignoreAbort
          ? reads.gate.promise
          : Promise.race([reads.gate.promise, abortable(signal)]));
        if (!reads.ignoreAbort && signal?.aborted) {
          throw new DOMException('aborted', 'AbortError');
        }
      }
      if (single !== undefined) {
        const found = server.agents.find((a) => a.id === decodeURIComponent(single));
        const ok = status >= 200 && status < 300 && found !== undefined;
        return {
          ok,
          status: ok ? 200 : found ? status : 404,
          json: () => Promise.resolve(found ? { ...found } : { error: 'not found' }),
        } as unknown as Response;
      }
      const limit = Number(url.searchParams.get('limit') ?? '50');
      const offset = Number(url.searchParams.get('cursor') ?? '0');
      const project = /^\/api\/v1\/projects\/([^/]+)\/agents$/.exec(url.pathname)?.[1];
      const rows = project
        ? server.agents.filter((a) => a.projectId === decodeURIComponent(project))
        : server.agents;
      const page = rows.slice(offset, offset + limit);
      const end = offset + page.length;
      const body = {
        agents: page.map((a) => (project && server.projectRow ? server.projectRow(a) : { ...a })),
        ...(end < rows.length ? { nextCursor: String(end) } : {}),
        ...(server.scopeCapabilities ? { _capabilities: server.scopeCapabilities } : {}),
      };
      return {
        ok: status >= 200 && status < 300,
        status,
        json: () => Promise.resolve(body),
      } as unknown as Response;
    }),
    holdAgentReads: (options = {}) => {
      const g = gate();
      heldReads = { gate: g, ignoreAbort: options.ignoreAbort ?? false };
      return () => {
        if (heldReads?.gate === g) heldReads = null;
        g.release();
      };
    },
    pause: () => {
      const g = gate();
      held = g;
      return () => {
        if (held === g) held = null;
        g.release();
      };
    },
    walks: (pathPrefix = '') =>
      server.requests.filter(
        (p) =>
          p.startsWith(pathPrefix) &&
          !p.includes('cursor=') &&
          !SINGLE_AGENT.test(new URL(p, 'http://localhost').pathname)
      ).length,
    agentFetches: (id) =>
      server.requests.filter((p) => {
        const match = SINGLE_AGENT.exec(new URL(p, 'http://localhost').pathname)?.[1];
        return match !== undefined && (id === undefined || decodeURIComponent(match) === id);
      }).length,
  };
  return server;
}

export interface Harness {
  store: AgentStore;
  server: AgentServer;
  /** Every feed the store created, oldest first. */
  feeds: StateManager[];
  /** Where window-level triggers are dispatched. */
  events: EventTarget;
  /** The feed's current stream. */
  stream: () => FakeEventSource;
  /** Open the feed's current stream and let waiting walks proceed. */
  connect: () => Promise<void>;
  /** Deliver `project.<projectId>.agent.<type>` and flush the feed. */
  emitAgent: (type: string, data: Record<string, unknown>, projectId?: string) => Promise<void>;
}

/** Let promise chains and zero-delay timers run under fake timers. */
export async function settle(): Promise<void> {
  for (let i = 0; i < 20; i++) await vi.advanceTimersByTimeAsync(0);
}

/**
 * A store over an in-memory server with a real `StateManager` feed. Call
 * after `vi.useFakeTimers()`; stubs `EventSource` globally.
 */
export function createHarness(
  initial: Agent[] = [],
  options: Partial<AgentStoreOptions> = {}
): Harness {
  FakeEventSource.instances = [];
  vi.stubGlobal('EventSource', FakeEventSource);
  const server = createAgentServer(initial);
  const feeds: StateManager[] = [];
  const events = new EventTarget();
  const store = new AgentStore({
    fetch: server.fetch,
    feedFactory: (): StateManager => {
      const feed = new StateManager();
      feeds.push(feed);
      return feed;
    },
    events,
    ...options,
  });
  const stream = (): FakeEventSource => FakeEventSource.latestOpen();
  return {
    store,
    server,
    feeds,
    events,
    stream,
    connect: async (): Promise<void> => {
      stream().open();
      await settle();
    },
    emitAgent: async (type, data, projectId = 'p1'): Promise<void> => {
      stream().emit(`project.${projectId}.agent.${type}`, data);
      await vi.advanceTimersByTimeAsync(100);
      await settle();
    },
  };
}
