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
import type { Agent, Project, RuntimeBroker } from '../shared/types.js';

/** Activities that should not be overwritten by idle/empty transitions */
const STICKY_ACTIVITIES = new Set(['waiting_for_input', 'completed', 'limits_exceeded']);

/** Subscription scope matches view context */
export type ViewScope =
  | { type: 'dashboard' }
  | { type: 'project'; projectId: string }
  | { type: 'agent-detail'; projectId: string; agentId: string }
  | { type: 'brokers-list' }
  | { type: 'broker-detail'; brokerId: string };

/** Full in-memory state for the current scope */
export interface AppState {
  agents: Map<string, Agent>;
  projects: Map<string, Project>;
  brokers: Map<string, RuntimeBroker>;
  deletedProjectIds: Set<string>;
  deletedAgentIds: Set<string>;
  connected: boolean;
  scope: ViewScope | null;
  /** Scope-level capabilities from the SSR-prefetched list response */
  scopeCapabilities: import('../shared/types.js').Capabilities | undefined;
}

/** Events dispatched by StateManager */
export type StateEventType =
  | 'agents-updated'
  | 'projects-updated'
  | 'brokers-updated'
  | 'connected'
  | 'disconnected'
  | 'scope-changed'
  | 'notification-created'
  | 'user-message-created';

export class StateManager extends EventTarget {
  private state: AppState = {
    agents: new Map(),
    projects: new Map(),
    brokers: new Map(),
    deletedProjectIds: new Set(),
    deletedAgentIds: new Set(),
    connected: false,
    scope: null,
    scopeCapabilities: undefined,
  };

  /**
   * Buffer for status deltas that arrived before the agent's "created" event.
   * When a clone fails quickly, the hub may publish the "status" SSE event
   * (with phase=error) before the "created" event. Without buffering, the
   * status delta would be dropped and the UI would never reflect the error.
   * Buffered deltas are applied when the "created" event arrives.
   */
  private pendingAgentDeltas = new Map<string, Partial<Agent>>();

  private sseClient = new SSEClient();

  constructor() {
    super();

    // Wire SSE client events to state management
    this.sseClient.addEventListener('update', ((event: CustomEvent<SSEUpdateEvent>) => {
      this.handleUpdate(event.detail);
    }) as EventListener);

    this.sseClient.addEventListener('connected', () => {
      this.state.connected = true;
      this.notify('connected');
    });

    this.sseClient.addEventListener('disconnected', () => {
      this.state.connected = false;
      this.notify('disconnected');
    });
  }

  /**
   * Initialize state from server-rendered data.
   * Called once on page load with the __SCION_DATA__ payload.
   *
   * @param initialData - Agents and/or projects from the prefetched API response.
   * @param scopeCapabilities - Scope-level capabilities from the API response's
   *   top-level `_capabilities` field (if present).
   */
  hydrate(
    initialData: { agents?: Agent[]; projects?: Project[] },
    scopeCapabilities?: import('../shared/types.js').Capabilities,
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
      this.state.scopeCapabilities = scopeCapabilities;
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
    this.state.scopeCapabilities = undefined;
    this.pendingAgentDeltas.clear();

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
    }
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
      this.pendingAgentDeltas.delete(agentId);
    } else {
      const existing = this.state.agents.get(agentId);
      if (!existing && eventType !== 'created') {
        // Agent not yet in state. Buffer the delta so it can be applied
        // when the "created" event arrives. This handles the race where
        // a status update (e.g. clone error) arrives before the "created"
        // event due to concurrent SSE publishing.
        const delta = data as Partial<Agent>;
        const prev = this.pendingAgentDeltas.get(agentId);
        this.pendingAgentDeltas.set(agentId, prev ? { ...prev, ...delta } : delta);
        return;
      }
      let base = existing || ({} as Agent);

      // For "created" events, apply any buffered deltas that arrived early.
      // The buffered delta is applied AFTER the created snapshot so that
      // status updates (like phase=error) take precedence.
      let delta = data as Partial<Agent>;
      if (eventType === 'created') {
        const pending = this.pendingAgentDeltas.get(agentId);
        if (pending) {
          delta = { ...delta, ...pending };
          this.pendingAgentDeltas.delete(agentId);
        }
      }

      // Preserve sticky activities: if the incoming activity is idle/empty
      // but the existing activity is sticky, keep the existing value.
      const incomingActivity = delta.activity as string | undefined;
      if (
        incomingActivity !== undefined &&
        (incomingActivity === 'idle' || incomingActivity === '') &&
        base.activity &&
        STICKY_ACTIVITIES.has(base.activity)
      ) {
        delete delta.activity;
      }
      // Promote detail fields from SSE detail to top-level agent
      const detail = delta.detail as import('../shared/types.js').AgentDetail | undefined;
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
      const updated = { ...base, ...delta, id: agentId };
      // Preserve _capabilities from existing state when the delta doesn't
      // provide valid capabilities (SSE status deltas typically omit them).
      if (!delta._capabilities && base._capabilities) {
        updated._capabilities = base._capabilities;
      }
      this.state.agents.set(agentId, updated as Agent);
    }
    this.notify('agents-updated');
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
      const id = (delta as Record<string, unknown>).brokerId as string || brokerId;
      const updated = { ...existing, ...delta, id };
      this.state.brokers.set(id, updated as RuntimeBroker);
    }
    this.notify('brokers-updated');
  }

  private notify(event: StateEventType): void {
    this.dispatchEvent(new CustomEvent(event, { detail: this.state }));
  }

  /**
   * Seed the agents map with full objects from a REST API response.
   * Called after initial data fetch so that SSE delta merging has
   * complete baseline data. Does not trigger notifications — the
   * calling component already holds the data from its own fetch.
   */
  seedAgents(agents: Agent[]): void {
    for (const agent of agents) {
      this.state.agents.set(agent.id, agent);
    }
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
   * Seed scope-level capabilities so other pages sharing the same scope
   * can use them without re-fetching.
   */
  seedScopeCapabilities(caps: import('../shared/types.js').Capabilities): void {
    this.state.scopeCapabilities = caps;
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

  getScopeCapabilities(): import('../shared/types.js').Capabilities | undefined {
    return this.state.scopeCapabilities;
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
