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
 * Client-side state manager with view-scoped SSE subscriptions
 *
 * The StateManager uses view-scoped subscriptions: the subscription scope
 * follows navigation, not individual entities. A paginated list of 200 agents
 * uses one project-level subscription, not 200 agent-level subscriptions.
 * Pagination is a rendering concern; the full state map is maintained in memory.
 *
 * See web-frontend-design.md §4.4 and §12.2.
 */

import { SSEClient } from './sse-client.js';
import type { SSEUpdateEvent } from './sse-client.js';
import type { Agent, AgentDetail, ExposedPort, Project, RuntimeBroker } from '../shared/types.js';

/** Activities that should not be overwritten by working/empty transitions */
const STICKY_ACTIVITIES = new Set(['waiting_for_input', 'completed', 'limits_exceeded']);

/**
 * Two agent objects are shallow-equal iff every field matches by reference,
 * except `detail`, which is freshly reconstructed on every merge and so is
 * compared by value instead (§7).
 */
function agentsShallowEqual(a: Agent, b: Agent): boolean {
  const aKeys = Object.keys(a);
  const bKeys = Object.keys(b);
  if (aKeys.length !== bKeys.length) return false;
  const ar = a as unknown as Record<string, unknown>;
  const br = b as unknown as Record<string, unknown>;
  for (const key of aKeys) {
    if (!Object.prototype.hasOwnProperty.call(b, key)) return false;
    if (key === 'detail') {
      if (!agentDetailEqual(a.detail, b.detail)) return false;
      continue;
    }
    if (ar[key] !== br[key]) {
      return false;
    }
  }
  return true;
}

/** Value comparison for `AgentDetail`, whose fields are all scalars. */
function agentDetailEqual(a: AgentDetail | undefined, b: AgentDetail | undefined): boolean {
  if (a === b) return true;
  if (!a || !b) return false;
  const aKeys = Object.keys(a) as (keyof AgentDetail)[];
  const bKeys = Object.keys(b) as (keyof AgentDetail)[];
  if (aKeys.length !== bKeys.length) return false;
  for (const key of aKeys) {
    if (a[key] !== b[key]) return false;
  }
  return true;
}

/** Subscription scope matches view context */
export type ViewScope =
  | { type: 'dashboard' }
  | { type: 'project'; projectId: string }
  | { type: 'agent-detail'; projectId: string; agentId: string }
  | { type: 'brokers-list' }
  | { type: 'broker-detail'; brokerId: string }
  | { type: 'chat'; spaceIds: string[]; userId: string };

/**
 * Resource a scope-level capability set was computed for.
 *
 * The Hub computes these per resource kind, so a cached set is only a valid
 * answer for the same kind it was computed for.
 */
export type ScopeResource = 'agent' | 'project';

/** Full in-memory state for the current scope */
export interface AppState {
  agents: Map<string, Agent>;
  projects: Map<string, Project>;
  brokers: Map<string, RuntimeBroker>;
  deletedProjectIds: Set<string>;
  deletedAgentIds: Set<string>;
  connected: boolean;
  scope: ViewScope | null;
  /**
   * Scope-level capabilities from the SSR-prefetched list response, keyed by
   * the resource they were computed for.
   *
   * Keyed rather than a single slot because agent-scope and project-scope
   * capabilities are different vocabularies computed by different calls
   * (`ComputeScopeCapabilities(..., "agent")` vs `..., "project"`). The Agents
   * and Projects pages declare the same `dashboard` scope, so `setScope`
   * early-returns on a navigation between them and never clears this. With one
   * shared slot the second page read the first page's capabilities and
   * rendered its "New …" button from the wrong answer.
   */
  scopeCapabilities: Map<ScopeResource, import('../shared/types.js').Capabilities>;
}

/** Events dispatched by StateManager */
export type StateEventType =
  | 'agents-updated'
  | 'agents-changed'
  | 'agents-resync'
  | 'projects-updated'
  | 'brokers-updated'
  | 'connected'
  | 'disconnected'
  | 'scope-changed'
  | 'notification-created'
  | 'user-message-created'
  | 'chat-message-received'
  | 'chat-topic-updated'
  | 'chat-presence-updated'
  | 'chat-typing-received'
  | 'chat-read-state-updated'
  | 'chat-message-edited'
  | 'chat-message-deleted'
  | 'chat-dm-promoted'
  | 'agent-created';

/**
 * Per-ID summary of a delta for an agent not yet known to `state.agents`
 * (§7). Carries only the fields a member index or off-page chip needs;
 * `pendingAgentDeltas` holds the full buffered delta separately.
 */
export interface UnknownAgentDelta {
  phase?: string;
  activity?: string;
  lastActivityEvent?: string;
}

/** Payload of the coalesced `agents-changed` event (§7). */
export interface AgentsChangedDetail {
  upserted: string[];
  deleted: string[];
  unknown: Map<string, UnknownAgentDelta>;
  generation: number;
}

/** Opaque token returned by `beginSeedEpoch`, passed to `seedAgents`/`endSeedEpoch`. */
export type SeedEpochToken = symbol;

export class StateManager extends EventTarget {
  private state: AppState = {
    agents: new Map(),
    projects: new Map(),
    brokers: new Map(),
    deletedProjectIds: new Set(),
    deletedAgentIds: new Set(),
    connected: false,
    scope: null,
    scopeCapabilities: new Map(),
  };

  /**
   * Buffer for status deltas that arrived before the agent's "created" event.
   * When a clone fails quickly, the hub may publish the "status" SSE event
   * (with phase=error) before the "created" event. Without buffering, the
   * status delta would be dropped and the UI would never reflect the error.
   * Buffered deltas are applied when the "created" event arrives.
   */
  private pendingAgentDeltas = new Map<string, Partial<Agent>>();

  /** Timers that drop a `pendingAgentDeltas` entry 30s after it was last touched (§7). */
  private pendingAgentDeltaTimers = new Map<string, ReturnType<typeof setTimeout>>();

  /** How long an unclaimed buffered delta survives before it is dropped. */
  private static readonly PENDING_DELTA_TTL_MS = 30_000;

  /**
   * Coalescing state accumulated since the last flush (§7). `setScope`
   * discards this outright on an actual scope change; it is never drained
   * into a final flush first.
   */
  private dirty: {
    upserted: Set<string>;
    deleted: Set<string>;
    unknown: Map<string, UnknownAgentDelta>;
  } = {
    upserted: new Set(),
    deleted: new Set(),
    unknown: new Map(),
  };

  /** IDs that received a `created` event since the last flush (§7). */
  private pendingCreatedIds = new Set<string>();

  private flushScheduled = false;
  private flushRafHandle: number | null = null;
  private flushTimeoutHandle: ReturnType<typeof setTimeout> | null = null;

  /**
   * Bumped by `setScope` on an actual scope change. Distinguishes deltas,
   * in-flight flush timers and SSE waits that belong to the current scope
   * from ones left over from a previous one (§7, §8).
   */
  private generation = 0;

  /** Whether a `disconnected` has been seen since the last `connected` in this generation (§7 N4). */
  private sawDisconnectThisGeneration = false;

  private sseConnectWaiters: Array<{
    generation: number;
    resolve: () => void;
    reject: (err: Error) => void;
  }> = [];

  /** Open seed epochs, keyed by the token handed to the caller (§7, §8). */
  private seedEpochs = new Map<SeedEpochToken, { deltas: Map<string, Partial<Agent>> }>();

  /**
   * "State holds the complete dashboard-scope membership" (§6.3, R2-B3, R3-B4).
   *
   * `full` also promises full `Agent` objects; `compact` promises only
   * membership (and whatever fields the compact projection carries). A
   * consumer that reads full fields from state must check
   * `isAgentSetComplete('full')` specifically — `isAgentSetComplete('compact')`
   * is also true when the flag is `full`, but a `compact` flag alone never
   * promises full fields (R10).
   *
   * Only `setScope` clears this, and only on an actual scope change. Resync,
   * label commits and partial seeds never clear it.
   */
  private completeFlag: 'full' | 'compact' | null = null;

  private sseClient = new SSEClient();

  /**
   * Current user's ID, set once by the app bootstrap. Chat notifications are
   * published on the subscriber-scoped subject `user.<id>.notification` (the
   * unscoped `notification.created` subject is readable by every session, and
   * chat payloads carry a sender name and a message preview), so the client
   * needs to know who it is before it can subscribe to its own notifications.
   */
  private currentUserId = '';

  constructor() {
    super();

    // Wire SSE client events to state management
    this.sseClient.addEventListener('update', ((event: CustomEvent<SSEUpdateEvent>) => {
      this.handleUpdate(event.detail);
    }) as EventListener);

    this.sseClient.addEventListener('connected', () => {
      this.state.connected = true;
      this.resolveSseConnectWaiters();
      // agents-resync (§7 N4): one per outage, even though `connected` can
      // fire twice per connection (onopen, then the server's own event) and
      // even though setScope's own reconnect goes through this same handler.
      // The first connect after setScope never sets sawDisconnectThisGeneration,
      // so it is not mistaken for a resync.
      if (this.sawDisconnectThisGeneration) {
        this.sawDisconnectThisGeneration = false;
        this.notify('agents-resync');
      }
      this.notify('connected');
    });

    this.sseClient.addEventListener('disconnected', () => {
      this.state.connected = false;
      this.sawDisconnectThisGeneration = true;
      this.notify('disconnected');
    });
  }

  /**
   * Initialize state from server-rendered data.
   * Called once on page load with the __SCION_DATA__ payload.
   *
   * @param initialData - Agents and/or projects from the prefetched API response.
   * @param scopeCapabilities - Scope-level capabilities from the API response's
   *   top-level `_capabilities` field (if present). The payload is prefetched
   *   for one page, so these belong to whichever list it carries; they are
   *   attributed to that resource. When the payload carries both lists the
   *   owner is ambiguous, so they are dropped rather than guessed — the page
   *   then fetches its own, which is correct if slower.
   */
  hydrate(
    initialData: { agents?: Agent[]; projects?: Project[] },
    scopeCapabilities?: import('../shared/types.js').Capabilities
  ): void {
    if (initialData.agents) {
      for (const agent of initialData.agents) {
        this.state.agents.set(agent.id, agent);
      }
    }

    if (initialData.projects) {
      for (const project of initialData.projects) {
        this.state.projects.set(project.id, project);
      }
    }

    if (scopeCapabilities) {
      const hasAgents = Array.isArray(initialData.agents);
      const hasProjects = Array.isArray(initialData.projects);
      if (hasAgents && !hasProjects) {
        this.state.scopeCapabilities.set('agent', scopeCapabilities);
      } else if (hasProjects && !hasAgents) {
        this.state.scopeCapabilities.set('project', scopeCapabilities);
      }
    }
  }

  /**
   * Record the signed-in user so scoped subscriptions can include the
   * per-user notification subject. Called by the app bootstrap once the
   * session user is known.
   *
   * If a scope is already active the SSE connection is reopened, because the
   * subject list computed without a user ID is missing that subscription and
   * the notification tray would never refresh.
   */
  setCurrentUserId(userId: string): void {
    if (this.currentUserId === userId) return;
    this.currentUserId = userId;
    if (this.state.scope) {
      const subjects = this.subjectsForScope(this.state.scope);
      if (subjects.length > 0) {
        this.sseClient.connect(subjects);
      }
    }
  }

  /**
   * Set the view scope. Closes any existing SSE connection and opens
   * a new one with subjects matching the view context.
   * Called by the router on navigation.
   */
  setScope(scope: ViewScope): void {
    // Skip if scope is unchanged
    if (this.state.scope && this.scopeEquals(this.state.scope, scope)) {
      return;
    }

    this.state.scope = scope;

    // Clear state from previous scope
    this.state.agents.clear();
    this.state.projects.clear();
    this.state.brokers.clear();
    this.state.deletedProjectIds.clear();
    this.state.deletedAgentIds.clear();
    this.state.scopeCapabilities.clear();
    for (const timer of this.pendingAgentDeltaTimers.values()) {
      clearTimeout(timer);
    }
    this.pendingAgentDeltaTimers.clear();
    this.pendingAgentDeltas.clear();

    // §7: an actual scope change discards the dirty set outright (no final
    // flush), bumps the generation, invalidates seed-epoch tokens and clears
    // the completeness flag. Nothing here is drained first.
    this.cancelScheduledFlush();
    this.dirty.upserted.clear();
    this.dirty.deleted.clear();
    this.dirty.unknown.clear();
    this.pendingCreatedIds.clear();
    this.seedEpochs.clear();
    this.completeFlag = null;
    this.generation++;
    this.sawDisconnectThisGeneration = false;
    this.rejectStaleSseConnectWaiters();

    const subjects = this.subjectsForScope(scope);
    if (subjects.length > 0) {
      this.sseClient.connect(subjects);
    }

    this.notify('scope-changed');
  }

  /**
   * Map view scope to event subject patterns.
   * Matches the subscription tiers defined in §12.2.
   * notification.> is always included so the notification tray shares the
   * single SSE connection rather than opening its own (avoids exhausting
   * the browser's 6-connection-per-origin HTTP/1.1 limit).
   */
  private subjectsForScope(scope: ViewScope): string[] {
    const subs = ((): string[] => {
      switch (scope.type) {
        case 'dashboard':
          return ['project.>', 'notification.>'];

        case 'project':
          return [`project.${scope.projectId}.>`, 'notification.>'];

        case 'agent-detail':
          return [`project.${scope.projectId}.>`, `agent.${scope.agentId}.>`, 'notification.>'];

        case 'brokers-list':
          return ['broker.>', 'notification.>'];

        case 'broker-detail':
          return ['broker.>', 'notification.>'];

        case 'chat': {
          const chatSubs: string[] = [];
          for (const spaceId of scope.spaceIds) {
            chatSubs.push(`project.${spaceId}.chat.>`);
            // Also subscribe to project-level agent events for the members sidebar
            chatSubs.push(`project.${spaceId}.agent.>`);
          }
          chatSubs.push(`user.${scope.userId}.chat.>`);
          chatSubs.push('notification.>');
          return chatSubs;
        }
      }
    })();

    // Chat notifications arrive on the subscriber-scoped subject, which the
    // server authorizes against the session user. It is added in every scope,
    // not just chat: a mention must still reach the tray and the title badge
    // while the user is looking at the agent list. Note that the chat scope's
    // `user.<id>.chat.>` does not cover it — `notification` is not under `chat`.
    const userId = this.currentUserId || (scope.type === 'chat' ? scope.userId : '');
    if (userId) {
      subs.push(`user.${userId}.notification`);
    }
    return subs;
  }

  private scopeEquals(a: ViewScope, b: ViewScope): boolean {
    if (a.type !== b.type) return false;
    if (a.type === 'dashboard' && b.type === 'dashboard') return true;
    if (a.type === 'brokers-list' && b.type === 'brokers-list') return true;
    if (a.type === 'broker-detail' && b.type === 'broker-detail') return a.brokerId === b.brokerId;
    if (a.type === 'project' && b.type === 'project') return a.projectId === b.projectId;
    if (a.type === 'agent-detail' && b.type === 'agent-detail') {
      return a.projectId === b.projectId && a.agentId === b.agentId;
    }
    if (a.type === 'chat' && b.type === 'chat') {
      return (
        a.userId === b.userId &&
        a.spaceIds.length === b.spaceIds.length &&
        a.spaceIds.every((id, i) => id === b.spaceIds[i])
      );
    }
    return false;
  }

  /**
   * Handle delta updates from SSE.
   * The server sends events with structure: { subject: string, data: unknown }
   * Subject format follows the event schema in §12.3.
   */
  private handleUpdate(update: SSEUpdateEvent): void {
    const { subject, data } = update;
    const parts = subject.split('.');

    // Notification events: notification.created
    if (parts[0] === 'notification') {
      this.notify('notification-created');
      return;
    }

    // User-scoped notifications: user.{userId}.notification
    //
    // Without an explicit case here the subject is silently dropped: it has
    // three tokens, and the user-scoped chat branch below requires four, so
    // it falls past every branch to the end of handleUpdate. The tray and the
    // unread badge would then never hear about a chat notification. (It does
    // not get misrouted to 'chat-message-received' — measured, not assumed.)
    if (parts[0] === 'user' && parts.length === 3 && parts[2] === 'notification') {
      this.notifyWithData('notification-created', data);
      return;
    }

    // User-scoped chat events: user.{userId}.chat.{dm|typing|message.edited|message.deleted}
    if (parts[0] === 'user' && parts.length >= 4 && parts[2] === 'chat') {
      // Human-to-human DMs have no project, so their typing events arrive on
      // the user-scoped subject rather than project.{id}.chat.typing.
      if (parts[3] === 'dm' && parts.length >= 5 && parts[4] === 'promoted') {
        this.notifyWithData('chat-dm-promoted', data);
      } else if (parts[3] === 'typing') {
        this.notifyWithData('chat-typing-received', data);
      } else if (parts[3] === 'read-state') {
        // A DM peer advanced their read watermark — drives the "seen" tick.
        this.notifyWithData('chat-read-state-updated', data);
      } else if (parts[3] === 'message' && parts.length >= 5) {
        // Phase-3: user-scoped message.edited / message.deleted for DMs.
        const subType = parts[4];
        if (subType === 'edited') {
          this.notifyWithData('chat-message-edited', data);
        } else if (subType === 'deleted') {
          this.notifyWithData('chat-message-deleted', data);
        }
      } else {
        this.notifyWithData('chat-message-received', data);
      }
      return;
    }

    // Broker-scoped events: broker.{brokerId}.{eventType}
    if (parts[0] === 'broker' && parts.length >= 3) {
      const brokerId = parts[1];
      const eventType = parts[2];
      this.handleBrokerEvent(brokerId, eventType, data);
      return;
    }

    // Agent-scoped events: agent.{agentId}.{eventType}
    if (parts[0] === 'agent' && parts.length >= 3) {
      const agentId = parts[1];
      const eventType = parts[2];
      this.handleAgentEvent(agentId, eventType, data);
      return;
    }

    // Project-scoped events
    if (parts[0] === 'project' && parts.length >= 3) {
      const projectId = parts[1];

      // Project agent events: project.{projectId}.agent.{eventType}
      if (parts[2] === 'agent' && parts.length >= 4) {
        const eventType = parts[3];
        const agentData = data as Record<string, unknown>;
        const agentId = agentData.agentId as string;
        if (agentId) {
          this.handleAgentEvent(agentId, eventType, data);
        }
        return;
      }

      // Project broker events: project.{projectId}.broker.{eventType}
      if (parts[2] === 'broker') {
        // Broker events don't affect agent/project state maps currently
        return;
      }

      // Chat events: project.{projectId}.chat.{eventType}[.{subType}]
      if (parts[2] === 'chat' && parts.length >= 4) {
        const chatEventType = parts[3];
        // Include projectId and the SSE payload so consumers can filter by conversation
        const chatDetail = { projectId, ...(data as Record<string, unknown>) };
        if (chatEventType === 'message' && parts.length >= 5) {
          // Phase-3: message.edited / message.deleted
          const subType = parts[4];
          if (subType === 'edited') {
            this.notifyWithData('chat-message-edited', chatDetail);
          } else if (subType === 'deleted') {
            this.notifyWithData('chat-message-deleted', chatDetail);
          }
          return;
        }
        if (chatEventType === 'message') {
          this.notifyWithData('chat-message-received', chatDetail);
        } else if (chatEventType === 'topic') {
          this.notifyWithData('chat-topic-updated', chatDetail);
        } else if (chatEventType === 'presence') {
          this.notifyWithData('chat-presence-updated', chatDetail);
        } else if (chatEventType === 'typing') {
          this.notifyWithData('chat-typing-received', chatDetail);
        }
        // Also dispatch the legacy user-message-created for v1 compat
        if (chatEventType === 'message') {
          this.notify('user-message-created');
        }
        return;
      }

      // User-targeted message events: project.{projectId}.user.{userId}
      if (parts[2] === 'user') {
        this.notify('user-message-created');
        return;
      }

      // Project metadata events: project.{projectId}.updated or project.*.summary
      this.handleProjectEvent(projectId, parts[2], data);
    }
  }

  private handleAgentEvent(agentId: string, eventType: string, data: unknown): void {
    if (eventType === 'deleted') {
      this.state.agents.delete(agentId);
      this.state.deletedAgentIds.add(agentId);
      this.clearPendingAgentDelta(agentId);
      this.dirty.upserted.delete(agentId);
      this.dirty.unknown.delete(agentId);
      this.pendingCreatedIds.delete(agentId);
      this.dirty.deleted.add(agentId);
      this.scheduleFlush();
      return;
    }

    if (eventType === 'ports') {
      // Ports carry only exposedPorts, and there is nothing sensible to
      // buffer for an agent state does not know about yet — unlike a status
      // delta, a stale port list is never a race worth preserving.
      const portsData = data as { ports: ExposedPort[] };
      const existing = this.state.agents.get(agentId);
      if (!existing) {
        return;
      }
      const exposedPorts = portsData.ports ?? [];
      const updated = { ...existing, exposedPorts } as Agent;
      if (agentsShallowEqual(existing, updated)) {
        return;
      }
      this.state.agents.set(agentId, updated);
      this.recordSeedEpochDelta(agentId, { exposedPorts });
      this.dirty.upserted.add(agentId);
      this.scheduleFlush();
      return;
    }

    const existing = this.state.agents.get(agentId);
    if (!existing && eventType !== 'created') {
      // Agent not yet in state. Buffer the delta so it can be applied when
      // the "created" event arrives (a status update, e.g. clone error, can
      // arrive before "created" due to concurrent SSE publishing), and
      // record it in dirty.unknown so it surfaces at the next flush (§7).
      const delta = data as Partial<Agent>;
      this.bufferAgentDelta(agentId, delta);
      this.recordUnknownDirty(agentId, delta);
      this.scheduleFlush();
      return;
    }
    const base = existing || ({} as Agent);

    // For "created" events, apply any buffered deltas that arrived early.
    // The buffered delta is applied AFTER the created snapshot so that
    // status updates (like phase=error) take precedence.
    let delta = data as Partial<Agent>;
    if (eventType === 'created') {
      const pending = this.pendingAgentDeltas.get(agentId);
      if (pending) {
        delta = { ...delta, ...pending };
      }
      this.clearPendingAgentDelta(agentId);
    }

    // Preserve sticky activities: if the incoming activity is working/empty
    // but the existing activity is sticky, keep the existing value.
    const incomingActivity = delta.activity as string | undefined;
    if (
      incomingActivity !== undefined &&
      (incomingActivity === 'working' || incomingActivity === '') &&
      base.activity &&
      STICKY_ACTIVITIES.has(base.activity)
    ) {
      delete delta.activity;
    }
    // Promote detail fields from SSE detail to top-level agent
    const detail = delta.detail as AgentDetail;
    if (detail) {
      if (detail.message) {
        (delta as Record<string, unknown>).message = detail.message;
      }
      if (detail.currentTurns !== undefined) {
        (delta as Record<string, unknown>).currentTurns = detail.currentTurns;
      }
      if (detail.currentModelCalls !== undefined) {
        (delta as Record<string, unknown>).currentModelCalls = detail.currentModelCalls;
      }
      if (detail.startedAt) {
        (delta as Record<string, unknown>).startedAt = detail.startedAt;
      }
    }
    // Ensure id is always set
    const updated = { ...base, ...delta, id: agentId } as Agent;
    // Preserve _capabilities from existing state when the delta doesn't
    // provide valid capabilities (SSE status deltas typically omit them).
    if (!delta._capabilities && base._capabilities) {
      updated._capabilities = base._capabilities;
    }

    // §7: a merge that changes nothing (by value; `detail` is compared
    // field-by-field) is a no-op — no mutation, no dirty entry, no flush.
    // A "created" event still signals agent-created below even when its
    // content happens to match what state already holds.
    const changed = !existing || !agentsShallowEqual(existing, updated);
    if (changed) {
      this.state.agents.set(agentId, updated);
      this.recordSeedEpochDelta(agentId, delta);
      this.dirty.upserted.add(agentId);
      this.dirty.unknown.delete(agentId);
    }
    if (eventType === 'created') {
      // A legitimate SSE creation for this ID. Signalled separately from
      // agents-changed so consumers that suppress re-adding a
      // server-omitted agent (see chat.ts loop guard) know the suppression
      // no longer applies to this ID.
      this.pendingCreatedIds.add(agentId);
    }
    if (changed || eventType === 'created') {
      this.scheduleFlush();
    }
  }

  /** Buffer a delta for an agent not yet known to state, refreshing its 30s TTL (§7). */
  private bufferAgentDelta(agentId: string, delta: Partial<Agent>): void {
    const prev = this.pendingAgentDeltas.get(agentId);
    this.pendingAgentDeltas.set(agentId, prev ? { ...prev, ...delta } : delta);

    const prevTimer = this.pendingAgentDeltaTimers.get(agentId);
    if (prevTimer !== undefined) {
      clearTimeout(prevTimer);
    }
    const timer = setTimeout(() => {
      this.pendingAgentDeltas.delete(agentId);
      this.pendingAgentDeltaTimers.delete(agentId);
    }, StateManager.PENDING_DELTA_TTL_MS);
    this.pendingAgentDeltaTimers.set(agentId, timer);
  }

  /** Drop a buffered delta and its expiry timer, e.g. once "created" or "deleted" resolves it. */
  private clearPendingAgentDelta(agentId: string): void {
    this.pendingAgentDeltas.delete(agentId);
    const timer = this.pendingAgentDeltaTimers.get(agentId);
    if (timer !== undefined) {
      clearTimeout(timer);
      this.pendingAgentDeltaTimers.delete(agentId);
    }
  }

  /** Record (last-value-wins, per field) the delta for an ID still unknown to state.agents. */
  private recordUnknownDirty(agentId: string, delta: Partial<Agent>): void {
    const existing = this.dirty.unknown.get(agentId) ?? {};
    const next: UnknownAgentDelta = { ...existing };
    if (delta.phase !== undefined) next.phase = delta.phase;
    if (delta.activity !== undefined) next.activity = delta.activity as string;
    if (delta.lastActivityEvent !== undefined) next.lastActivityEvent = delta.lastActivityEvent;
    this.dirty.unknown.set(agentId, next);
  }

  /** Record a delta applied to a known agent into every open seed epoch (§7, §8). */
  private recordSeedEpochDelta(agentId: string, delta: Partial<Agent>): void {
    if (this.seedEpochs.size === 0) return;
    for (const epoch of this.seedEpochs.values()) {
      const prev = epoch.deltas.get(agentId);
      epoch.deltas.set(agentId, prev ? { ...prev, ...delta } : delta);
    }
  }

  private handleProjectEvent(projectId: string, eventType: string, data: unknown): void {
    if (eventType === 'deleted') {
      this.state.projects.delete(projectId);
      this.state.deletedProjectIds.add(projectId);
    } else if (eventType === 'summary') {
      // Dashboard summary event: project.*.summary
      const summaryData = data as Partial<Project> & { projectId?: string };
      const id = summaryData.projectId || projectId;
      const existing = this.state.projects.get(id) || ({} as Project);
      const updated = { ...existing, ...summaryData, id };
      if (!summaryData._capabilities && existing._capabilities) {
        updated._capabilities = existing._capabilities;
      }
      this.state.projects.set(id, updated as Project);
    } else {
      // Project lifecycle events: created, updated
      const projectData = data as Partial<Project> & { projectId?: string };
      const id = projectData.projectId || projectId;
      const existing = this.state.projects.get(id) || ({} as Project);
      const updated = { ...existing, ...projectData, id };
      if (!projectData._capabilities && existing._capabilities) {
        updated._capabilities = existing._capabilities;
      }
      this.state.projects.set(id, updated as Project);
    }
    this.notify('projects-updated');
  }

  private handleBrokerEvent(brokerId: string, eventType: string, data: unknown): void {
    if (eventType === 'deleted') {
      this.state.brokers.delete(brokerId);
    } else {
      // Merge delta into existing broker state
      const existing = this.state.brokers.get(brokerId) || ({} as RuntimeBroker);
      const delta = data as Partial<RuntimeBroker>;
      // Map brokerId field from event payload to id
      const id = ((delta as Record<string, unknown>).brokerId as string) || brokerId;
      const updated = { ...existing, ...delta, id };
      this.state.brokers.set(id, updated as RuntimeBroker);
    }
    this.notify('brokers-updated');
  }

  private notify(event: StateEventType): void {
    this.dispatchEvent(new CustomEvent(event, { detail: this.state }));
  }

  /** Dispatch an event with additional SSE payload data for consumer filtering. */
  private notifyWithData(event: StateEventType, data: unknown): void {
    this.dispatchEvent(new CustomEvent(event, { detail: { state: this.state, data } }));
  }

  /**
   * Schedule a coalesced flush: one per `requestAnimationFrame`, or after
   * 100ms if no frame arrives (a hidden tab throttles rAF) (§7). Whichever
   * fires first performs the flush and cancels the other.
   */
  private scheduleFlush(): void {
    if (this.flushScheduled) return;
    this.flushScheduled = true;

    if (typeof requestAnimationFrame === 'function') {
      this.flushRafHandle = requestAnimationFrame(() => this.flush());
    }
    this.flushTimeoutHandle = setTimeout(() => this.flush(), 100);
  }

  /** Cancel any pending flush timers without running the flush (used by setScope). */
  private cancelScheduledFlush(): void {
    this.flushScheduled = false;
    if (this.flushRafHandle !== null) {
      if (typeof cancelAnimationFrame === 'function') {
        cancelAnimationFrame(this.flushRafHandle);
      }
      this.flushRafHandle = null;
    }
    if (this.flushTimeoutHandle !== null) {
      clearTimeout(this.flushTimeoutHandle);
      this.flushTimeoutHandle = null;
    }
  }

  /**
   * Emit the coalesced notifications for everything dirtied since the last
   * flush, in order: `agent-created` per created ID, `agents-changed`, then
   * the legacy `agents-updated` once for the whole flush (§7).
   */
  private flush(): void {
    if (!this.flushScheduled) return;
    this.cancelScheduledFlush();

    const createdIds = Array.from(this.pendingCreatedIds);
    const upserted = Array.from(this.dirty.upserted);
    const deleted = Array.from(this.dirty.deleted);
    const unknown = new Map(this.dirty.unknown);

    this.pendingCreatedIds.clear();
    this.dirty.upserted.clear();
    this.dirty.deleted.clear();
    this.dirty.unknown.clear();

    for (const agentId of createdIds) {
      this.notifyWithData('agent-created', { agentId });
    }

    const changed: AgentsChangedDetail = {
      upserted,
      deleted,
      unknown,
      generation: this.generation,
    };
    this.notifyWithData('agents-changed', changed);

    this.notify('agents-updated');
  }

  /**
   * Seed the agents map with objects from a REST API response. Called after
   * a fetch so that SSE delta merging has baseline data. Does not trigger
   * notifications — the calling component already holds the data from its
   * own fetch.
   *
   * `token`, from `beginSeedEpoch`, re-applies the merged deltas recorded
   * for each ID while the epoch was open, so a delta that landed after this
   * REST snapshot was taken is not clobbered by it (§7, §8). A token
   * invalidated by a scope change (or never opened, or already ended) makes
   * this call a complete no-op: the snapshot may belong to a scope state no
   * longer holds.
   *
   * `partial: true` merges each object into the existing one instead of
   * replacing it, so a compact-view seed cannot strip full fields a fuller
   * seed already recorded (§6.3).
   *
   * Every seed skips tombstoned IDs (`deletedAgentIds`): a `deleted` event
   * is always fresher than a REST snapshot that still lists the agent.
   */
  seedAgents(agents: Agent[], options?: { token?: SeedEpochToken; partial?: boolean }): void {
    const token = options?.token;
    if (token !== undefined && !this.seedEpochs.has(token)) {
      return;
    }
    const recordedDeltas = token !== undefined ? this.seedEpochs.get(token)?.deltas : undefined;
    const partial = options?.partial ?? false;

    for (const agent of agents) {
      if (this.state.deletedAgentIds.has(agent.id)) {
        continue;
      }
      let toStore: Agent = agent;
      if (partial) {
        const existing = this.state.agents.get(agent.id);
        toStore = existing ? ({ ...existing, ...agent, id: agent.id } as Agent) : agent;
      }
      const recorded = recordedDeltas?.get(agent.id);
      if (recorded) {
        toStore = { ...toStore, ...recorded, id: agent.id } as Agent;
      }
      this.state.agents.set(agent.id, toStore);
    }
  }

  /**
   * Start recording per-ID merged deltas applied while a REST fetch is in
   * flight, so `seedAgents` can re-apply anything fresher than the response
   * it is about to seed (§7, §8). Returns a token to pass to `seedAgents`
   * and `endSeedEpoch`.
   */
  beginSeedEpoch(): SeedEpochToken {
    const token: SeedEpochToken = Symbol('seed-epoch');
    this.seedEpochs.set(token, { deltas: new Map() });
    return token;
  }

  /**
   * Close a seed epoch. Idempotent, and a no-op for a token already
   * invalidated by a scope change.
   */
  endSeedEpoch(token: SeedEpochToken): void {
    this.seedEpochs.delete(token);
  }

  /**
   * Mark that state holds the complete dashboard-scope membership. Upgrades
   * `compact` to `full`; never downgrades `full` to `compact` (§6.3).
   */
  markAgentSetComplete(view: 'full' | 'compact'): void {
    if (view === 'full') {
      this.completeFlag = 'full';
    } else if (this.completeFlag !== 'full') {
      this.completeFlag = 'compact';
    }
  }

  /**
   * True iff the completeness flag is set and satisfies `need`: `compact` is
   * satisfied by either flag value, `full` only by a `full` flag (§6.3, R10).
   */
  isAgentSetComplete(need: 'full' | 'compact'): boolean {
    if (this.completeFlag === null) return false;
    return need === 'compact' || this.completeFlag === 'full';
  }

  /** The scope generation, bumped by `setScope` on every actual scope change (§7, §8). */
  get scopeGeneration(): number {
    return this.generation;
  }

  /**
   * Resolves once the SSE connection is live in scope generation
   * `generation`: at once if it already is, otherwise on the next
   * `connected` of that generation. Rejects immediately if `generation` is
   * already stale, and rejects if the generation changes while waiting
   * (§7, §8).
   */
  sseConnected(generation: number): Promise<void> {
    if (generation !== this.generation) {
      return Promise.reject(new Error('scope generation changed'));
    }
    if (this.state.connected) {
      return Promise.resolve();
    }
    return new Promise<void>((resolve, reject) => {
      this.sseConnectWaiters.push({ generation, resolve, reject });
    });
  }

  /** Resolve every waiter registered for the current generation. */
  private resolveSseConnectWaiters(): void {
    const gen = this.generation;
    const waiters = this.sseConnectWaiters;
    this.sseConnectWaiters = waiters.filter((w) => w.generation !== gen);
    for (const w of waiters) {
      if (w.generation === gen) {
        w.resolve();
      }
    }
  }

  /** Reject every waiter whose generation no longer matches the current one. */
  private rejectStaleSseConnectWaiters(): void {
    const gen = this.generation;
    const waiters = this.sseConnectWaiters;
    this.sseConnectWaiters = waiters.filter((w) => w.generation === gen);
    for (const w of waiters) {
      if (w.generation !== gen) {
        w.reject(new Error('scope generation changed'));
      }
    }
  }

  /**
   * Remove a stale agent from the shared map, e.g. one an authoritative
   * members fetch no longer returns because the client missed its SSE
   * `deleted` event (backgrounded tab, dropped connection). Does not
   * notify — the caller already owns the UI update from its own fetch,
   * and notifying here would re-trigger SSE merge consumers that read
   * from this map, re-adding the entry they're removing.
   */
  removeAgent(id: string): void {
    this.state.agents.delete(id);
    this.pendingAgentDeltas.delete(id);
  }

  /**
   * Seed the projects map with full objects from a REST API response.
   * Does not trigger notifications.
   */
  seedProjects(projects: Project[]): void {
    for (const project of projects) {
      this.state.projects.set(project.id, project);
    }
  }

  /**
   * Seed scope-level capabilities for one resource kind, so a later visit to
   * the same page can use them without re-fetching.
   *
   * `resource` is required: a capability set answers "what may I do with
   * <resource>", and storing it unkeyed let one page's answer be read as
   * another's.
   */
  seedScopeCapabilities(
    resource: ScopeResource,
    caps: import('../shared/types.js').Capabilities
  ): void {
    this.state.scopeCapabilities.set(resource, caps);
  }

  /**
   * Seed the brokers map with full objects from a REST API response.
   * Does not trigger notifications.
   */
  seedBrokers(brokers: RuntimeBroker[]): void {
    for (const broker of brokers) {
      this.state.brokers.set(broker.id, broker);
    }
  }

  /** Expose the SSE client for debug instrumentation */
  get sseClientInstance(): SSEClient {
    return this.sseClient;
  }

  /** Current subscription subjects from the SSE client */
  get currentSubjects(): string[] {
    return this.sseClient.currentSubjects;
  }

  /** Snapshot of current state for debug display */
  getStateSnapshot(): {
    agentCount: number;
    projectCount: number;
    brokerCount: number;
    agentIds: string[];
    projectIds: string[];
    brokerIds: string[];
    deletedProjectIds: string[];
    deletedAgentIds: string[];
  } {
    return {
      agentCount: this.state.agents.size,
      projectCount: this.state.projects.size,
      brokerCount: this.state.brokers.size,
      agentIds: Array.from(this.state.agents.keys()),
      projectIds: Array.from(this.state.projects.keys()),
      brokerIds: Array.from(this.state.brokers.keys()),
      deletedProjectIds: Array.from(this.state.deletedProjectIds),
      deletedAgentIds: Array.from(this.state.deletedAgentIds),
    };
  }

  /** Disconnect the SSE connection. Called on page unload. */
  disconnect(): void {
    this.sseClient.disconnect();
    this.state.connected = false;
  }

  // --- Getters ---
  // The full state map is maintained regardless of pagination.
  // Components render the slice they need.

  getAgents(): Agent[] {
    return Array.from(this.state.agents.values());
  }

  getAgent(id: string): Agent | undefined {
    return this.state.agents.get(id);
  }

  getProjects(): Project[] {
    return Array.from(this.state.projects.values());
  }

  getProject(id: string): Project | undefined {
    return this.state.projects.get(id);
  }

  getBrokers(): RuntimeBroker[] {
    return Array.from(this.state.brokers.values());
  }

  getBroker(id: string): RuntimeBroker | undefined {
    return this.state.brokers.get(id);
  }

  getDeletedProjectIds(): Set<string> {
    return this.state.deletedProjectIds;
  }

  getDeletedAgentIds(): Set<string> {
    return this.state.deletedAgentIds;
  }

  /**
   * Scope-level capabilities previously seeded for `resource`, or undefined
   * when none were. Never returns another resource's capabilities.
   */
  getScopeCapabilities(
    resource: ScopeResource
  ): import('../shared/types.js').Capabilities | undefined {
    return this.state.scopeCapabilities.get(resource);
  }

  get isConnected(): boolean {
    return this.state.connected;
  }

  get currentScope(): ViewScope | null {
    return this.state.scope;
  }
}

/** Singleton instance — accessed via import */
export const stateManager = new StateManager();
