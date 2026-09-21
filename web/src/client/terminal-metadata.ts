/** Workspace metadata has document/session lifetime, independent of route state. */
import type { Agent, ExposedPort } from '../shared/types.js';
import { SSEClient, type SSEUpdateEvent } from './sse-client.js';

export interface TerminalAgentMetadata {
  readonly agent: Agent | null;
  readonly availability: 'loading' | 'ready' | 'deleted' | 'unavailable';
  readonly error: string | null;
}
type Listener = (metadata: TerminalAgentMetadata) => void;
interface Entry {
  id: string;
  value: TerminalAgentMetadata;
  listeners: Set<Listener>;
  patch: Partial<Agent>;
  deleted: boolean;
  ready: boolean;
  excluded: boolean;
  diagnostic: boolean;
  diagnosticOutcome: 'readable' | 'excluded' | 'transient';
  epoch: number;
  controller: AbortController | null;
  queued: boolean;
  waiters: Array<() => void>;
}
interface Batch {
  client: SSEClient;
  entries: Entry[];
  diagnosed: boolean;
  blocked: boolean;
}

// The Hub limits each subject to 256 characters, but has no subject-count cap.
// Keep encoded query strings under 1800 characters for proxy/request-line headroom.
// This is a transport batch size, never a retained-session limit.
const QUERY_BUDGET = 1800;
const SNAPSHOT_CONCURRENCY = 4;

export class TerminalMetadata {
  private readonly entries = new Map<string, Entry>();
  private batches: Batch[] = [];
  private scheduled = false;
  private forceUnion = false;
  private disposed = false;
  private activeRequests = 0;

  constructor(private readonly hubUrl: string) {}

  get(agentId: string): TerminalAgentMetadata | undefined {
    return this.entries.get(agentId)?.value;
  }

  /** Subscribe only to retained entries. Session removal also removes listeners. */
  subscribe(agentId: string, listener: Listener): () => void {
    const entry = this.entries.get(agentId);
    if (!entry) throw new Error('Terminal metadata requires a retained session.');
    entry.listeners.add(listener);
    listener(entry.value);
    return () => {
      entry.listeners.delete(listener);
    };
  }

  /** Registry lifetime hook; repeated opens do not acquire additional ownership. */
  retain(id: string): void {
    if (this.disposed) throw new Error('Terminal metadata is disposed.');
    if (this.entries.has(id)) return;
    this.entries.set(id, {
      id,
      value: { agent: null, availability: 'loading', error: null },
      listeners: new Set(),
      patch: {},
      deleted: false,
      ready: false,
      excluded: false,
      diagnostic: false,
      diagnosticOutcome: 'transient',
      epoch: 0,
      controller: null,
      queued: false,
      waiters: [],
    });
    this.scheduleUnion();
  }

  release(id: string): void {
    const entry = this.entries.get(id);
    if (!entry) return;
    this.entries.delete(id);
    this.cancel(entry);
    entry.listeners.clear();
    this.settle(entry);
    // Last-session cleanup is synchronous, including account teardown.
    if (!this.entries.size) this.disconnect();
    else this.scheduleUnion();
  }

  /** Transport metadata is provisional only; never replaces a central snapshot. */
  seed(id: string, agent: Agent): void {
    const entry = this.entries.get(id);
    if (!entry || entry.value.agent || entry.deleted || entry.value.availability !== 'loading')
      return;
    this.publish(entry, { ...entry.value, agent: { ...agent, ...entry.patch } });
  }

  /** Capture-auth uses the same ordered refresh path as reconnect reconciliation. */
  refresh(id: string): Promise<void> {
    const entry = this.entries.get(id);
    if (!entry || this.disposed) return Promise.resolve();
    const result = new Promise<void>((resolve) => entry.waiters.push(resolve));
    // Supersede a previous snapshot; events already reflected in the store become
    // the starting point, and subsequent deltas overlay this new snapshot.
    this.cancel(entry);
    entry.patch = {};
    entry.queued = true;
    if (!entry.ready) {
      entry.excluded = false;
      entry.deleted = false;
      this.scheduleUnion(true);
    } else this.pump();
    return result;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const id of this.entries.keys()) this.release(id);
    this.disconnect();
  }

  private publish(entry: Entry, value: TerminalAgentMetadata): void {
    entry.value = value;
    for (const listener of entry.listeners) listener(value);
  }

  private cancel(entry: Entry): void {
    entry.epoch++;
    entry.controller?.abort();
    entry.controller = null;
    entry.queued = false;
    entry.diagnostic = false;
  }

  private settle(entry: Entry): void {
    for (const resolve of entry.waiters.splice(0)) resolve();
  }

  private disconnect(): void {
    for (const batch of this.batches) batch.client.disconnect();
    this.batches = [];
    for (const entry of this.entries.values()) {
      this.cancel(entry);
      entry.ready = false;
    }
  }

  private scheduleUnion(force = false): void {
    this.forceUnion ||= force;
    if (this.scheduled) return;
    this.scheduled = true;
    queueMicrotask(() => {
      this.scheduled = false;
      if (this.disposed) return;
      const retained = [...this.entries.values()].filter((entry) => !entry.excluded);
      const subscribed = this.batches.flatMap((batch) => batch.entries);
      // The same UUID can now belong to a replacement Entry. Existing batches
      // capture the old objects, so subject equality alone cannot justify reuse.
      if (
        !this.forceUnion &&
        retained.length === subscribed.length &&
        retained.every((entry, index) => entry === subscribed[index])
      )
        return;
      this.forceUnion = false;
      this.disconnect();
      let entries: Entry[] = [];
      let length = 0;
      for (const entry of this.entries.values()) {
        if (entry.excluded) continue;
        const size = `sub=${encodeURIComponent(`agent.${entry.id}.>`)}&`.length;
        if (length + size > QUERY_BUDGET && entries.length) {
          this.connectBatch(entries);
          entries = [];
          length = 0;
        }
        entries.push(entry);
        length += size;
      }
      if (entries.length) this.connectBatch(entries);
    });
  }

  private connectBatch(entries: Entry[]): void {
    const client = new SSEClient(new URL('events', this.hubUrl).href);
    const batch: Batch = { client, entries, diagnosed: false, blocked: false };
    this.batches.push(batch);
    const current = (): boolean => this.batches.includes(batch);
    client.addEventListener('connected', () => {
      if (!current()) return;
      batch.diagnosed = false;
      // Hub must install Subscribe before flushing headers (browser onopen).
      // Buffer from readiness, including while waiting for a concurrency slot.
      for (const entry of entries) {
        if (this.entries.get(entry.id) !== entry) continue;
        this.cancel(entry);
        entry.ready = true;
        entry.patch = {};
        entry.queued = true;
      }
      this.pump();
    });
    client.addEventListener('disconnected', () => {
      if (!current()) return;
      for (const entry of entries) {
        this.cancel(entry);
        entry.ready = false;
      }
    });
    client.addEventListener('handshake-failed', () => {
      if (!current() || batch.diagnosed) return;
      batch.diagnosed = true;
      for (const entry of entries) {
        if (this.entries.get(entry.id) !== entry) continue;
        this.cancel(entry);
        entry.ready = false;
        entry.diagnostic = true;
        entry.diagnosticOutcome = 'transient';
        entry.queued = true;
        this.publish(entry, {
          ...entry.value,
          availability: entry.deleted ? 'deleted' : 'unavailable',
          error: 'Event subscription unavailable. Checking agent metadata.',
        });
      }
      this.pump();
    });
    client.addEventListener('update', (event) => {
      if (current()) this.update(event.detail, entries);
    });
    client.connect(entries.map((entry) => `agent.${entry.id}.>`));
  }

  private update(event: SSEUpdateEvent, entries: Entry[]): void {
    const [prefix, id, kind] = event.subject.split('.');
    const entry = this.entries.get(id);
    if (prefix !== 'agent' || !entry || !entries.includes(entry)) return;
    if (kind === 'deleted') {
      entry.deleted = true;
      this.publish(entry, { ...entry.value, availability: 'deleted', error: 'Agent was deleted.' });
      return;
    }
    if (entry.deleted || !event.data || typeof event.data !== 'object') return;
    const data = event.data as Partial<Agent> & { ports?: ExposedPort[] };
    const patch: Partial<Agent> = {};
    if (kind === 'ports') patch.exposedPorts = data.ports ?? [];
    else if (kind === 'status') {
      if (data.phase !== undefined) patch.phase = data.phase;
      if (data.activity !== undefined) patch.activity = data.activity;
    } else return;
    entry.patch = { ...entry.patch, ...patch };
    if (entry.value.agent)
      this.publish(entry, { ...entry.value, agent: { ...entry.value.agent, ...patch } });
  }

  private pump(): void {
    if (this.disposed) return;
    for (const entry of this.entries.values()) {
      if (this.activeRequests >= SNAPSHOT_CONCURRENCY) break;
      if ((!entry.ready && !entry.diagnostic) || !entry.queued) continue;
      entry.queued = false;
      const controller = new AbortController();
      entry.controller = controller;
      const epoch = entry.epoch;
      this.activeRequests++;
      void this.snapshot(entry, epoch, controller.signal, entry.diagnostic).finally(() => {
        this.activeRequests--;
        this.pump();
        this.finishDiagnostics();
      });
    }
  }

  private finishDiagnostics(): void {
    for (const batch of this.batches) {
      if (!batch.diagnosed || batch.blocked || batch.entries.some((e) => e.diagnostic)) continue;
      if (batch.entries.some((e) => e.excluded)) {
        this.scheduleUnion();
      } else if (batch.entries.every((e) => e.diagnosticOutcome === 'readable')) {
        // GET success cannot prove SSE permission/readiness. Avoid an endless
        // authorization mismatch loop; an explicit refresh retries the batch.
        batch.blocked = true;
        batch.client.disconnect();
      }
    }
  }

  private async snapshot(
    entry: Entry,
    epoch: number,
    signal: AbortSignal,
    diagnostic: boolean
  ): Promise<void> {
    const current = (): boolean =>
      !signal.aborted && this.entries.get(entry.id) === entry && entry.epoch === epoch;
    try {
      const response = await fetch(new URL(`api/v1/agents/${entry.id}`, this.hubUrl), {
        credentials: 'include',
        signal,
      });
      if (!current()) return;
      if (!response.ok) {
        if (response.status === 404) entry.deleted = true;
        if (response.status === 403 || response.status === 404) {
          entry.excluded = true;
          entry.diagnosticOutcome = 'excluded';
          if (!diagnostic) this.scheduleUnion();
        }
        if (response.status === 401) throw new Error('Authentication expired. Sign in again.');
        throw new Error(`Agent metadata unavailable (HTTP ${response.status}).`);
      }
      const agent = (await response.json()) as Agent;
      if (!current()) return;
      if (agent.id?.toLowerCase() !== entry.id)
        throw new Error('Agent metadata does not match requested UUID.');
      if (diagnostic) {
        entry.diagnosticOutcome = 'readable';
        this.publish(entry, {
          ...entry.value,
          agent: entry.value.agent ?? agent,
          availability: entry.deleted ? 'deleted' : 'unavailable',
          error:
            'Agent metadata is readable, but its event subscription is unavailable. Retry metadata to reconnect.',
        });
      } else if (!entry.deleted)
        this.publish(entry, {
          agent: { ...agent, ...entry.patch },
          availability: 'ready',
          error: null,
        });
    } catch (error) {
      if (current())
        this.publish(entry, {
          ...entry.value,
          availability: entry.deleted ? 'deleted' : 'unavailable',
          error: error instanceof Error ? error.message : 'Agent metadata unavailable.',
        });
    } finally {
      if (current()) {
        entry.controller = null;
        entry.diagnostic = false;
        entry.patch = {};
        this.settle(entry);
      }
    }
  }
}
