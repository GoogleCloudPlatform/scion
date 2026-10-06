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
 * Shared types for server and client
 */

/**
 * User role enumeration
 */
export type UserRole = 'admin' | 'member' | 'viewer';

/**
 * Personal, non-admin-controlled user preferences (tz-refactor task 11,
 * design.md §3 A "Storage and API"). Present only on the authenticated
 * caller's own user object — `GET /auth/me` / `GET /api/v1/auth/me` — never
 * on a listing or another user's record.
 */
export interface UserPreferences {
  /**
   * IANA display-timezone name, or `''`/absent for Auto (follow the
   * browser's zone). See `web/src/utils/time.ts`'s `effectiveTimeZone`.
   */
  timezone?: string | undefined;
}

/**
 * User information
 */
export interface User {
  id: string;
  email: string;
  name: string;
  avatar?: string | undefined;
  role?: UserRole | undefined;
  preferences?: UserPreferences | undefined;
}

/**
 * Admin user information from the Hub API (GET /api/v1/users)
 */
export interface AdminUser {
  id: string;
  email: string;
  displayName: string;
  avatarUrl?: string;
  role: UserRole;
  status: 'active' | 'suspended' | 'invited';
  created: string;
  lastLogin?: string;
  lastSeen?: string;
  invitedBy?: string;
  inviteNote?: string;
  _capabilities?: Capabilities;
}

/**
 * Group type enumeration
 */
export type GroupType = 'explicit' | 'project_agents';

/**
 * Group information from the Hub API (GET /api/v1/groups)
 */
export interface AdminGroup {
  id: string;
  name: string;
  slug: string;
  description?: string;
  groupType: GroupType;
  projectId?: string;
  parentId?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  ownerId?: string;
  createdBy?: string;
  created: string;
  updated: string;
  _capabilities?: Capabilities;
}

/**
 * Group member information
 */
export interface GroupMember {
  groupId: string;
  memberType: 'user' | 'group' | 'agent';
  memberId: string;
  displayName?: string;
  role: 'member' | 'admin' | 'owner';
  addedAt: string;
  addedBy?: string;
}

/**
 * Initial page data passed from SSR to client
 */
export interface PageData {
  /** Current URL path */
  path: string;
  /** Page title */
  title: string;
  /** Current user (if authenticated) */
  user?: User | undefined;
  /** Additional page-specific data */
  data?: Record<string, unknown> | undefined;
}

/**
 * Route definition for client-side routing
 */
export interface RouteConfig {
  path: string;
  component: string;
  action?: () => Promise<void>;
}

/**
 * Project status enumeration
 */
export type ProjectStatus = 'active' | 'inactive' | 'error';

/**
 * Project information from the Hub API
 */
/**
 * Project type enumeration
 */
export type ProjectType = 'linked' | 'hub-managed';

export interface GitHubAppProjectStatus {
  state: 'ok' | 'degraded' | 'error' | 'unchecked';
  error_code?: string;
  error_message?: string;
  last_token_mint?: string;
  last_error?: string;
  last_checked: string;
}

export interface GitHubTokenPermissions {
  contents?: string;
  pull_requests?: string;
  issues?: string;
  metadata?: string;
  checks?: string;
  actions?: string;
}

export interface Project {
  id: string;
  name: string;
  slug?: string;
  path: string;
  gitRemote?: string;
  projectType?: ProjectType;
  status: ProjectStatus;
  labels?: Record<string, string>;
  defaultRuntimeBrokerId?: string;
  ownerId?: string;
  ownerName?: string;
  agentCount: number;
  /** Creation and last-update times, as the hub sends them. */
  created?: string;
  updated?: string;
  /** Older names for created / updated; read as a fallback. */
  createdAt?: string;
  updatedAt?: string;
  _capabilities?: Capabilities;
  sharedDirs?: SharedDir[];
  githubInstallationId?: number | undefined;
  githubPermissions?: GitHubTokenPermissions | undefined;
  githubAppStatus?: GitHubAppProjectStatus | undefined;
  cloudLogging?: boolean;
}

/**
 * Check whether a project is a shared-workspace git project.
 */
export function isSharedWorkspace(project: Project): boolean {
  return !!project.gitRemote && project.labels?.['scion.dev/workspace-mode'] === 'shared';
}

/**
 * Check whether a project gives each agent its own empty directory (#2703):
 * no git remote and a hub-owned workspace-mode label of per-agent, or the
 * raw empty-per-agent value (the hub's ResolveProjectSharingMode treats
 * both the same on a non-git project).
 */
export function isEmptyPerAgentWorkspace(project?: {
  gitRemote?: string | undefined;
  labels?: Record<string, string> | undefined;
}): boolean {
  if (!project) return false;
  if (project.gitRemote) return false;
  const mode = project.labels?.['scion.dev/workspace-mode'];
  return mode === 'per-agent' || mode === 'empty-per-agent';
}

/**
 * Check whether a project uses worktree-per-agent workspace mode.
 */
export function isWorktreeWorkspace(project: Project): boolean {
  return (
    !!project.gitRemote && project.labels?.['scion.dev/workspace-mode'] === 'worktree-per-agent'
  );
}

/**
 * Exposed port registered by an agent for port forwarding.
 */
export interface ExposedPort {
  port: number;
  label?: string;
  host?: string;
  mode?: string;
  exposedAt: string;
  exposedBy: string;
}

/**
 * Agent lifecycle phase (from canonical agent state model)
 */
export type AgentPhase =
  | 'created'
  | 'provisioning'
  | 'cloning'
  | 'starting'
  | 'running'
  | 'stopping'
  | 'stopped'
  | 'suspended'
  | 'error';

/**
 * Message mode controlling an agent's messaging authorization scope
 */
export type MessageMode = 'none' | 'lineage' | 'branch' | 'project' | 'hub';

// ---------------------------------------------------------------------------
// Cascade mode change types
// ---------------------------------------------------------------------------

/** Detail for a single agent affected by a cascade mode change. */
export interface CascadeAgentDetail {
  agent_id: string;
  agent_name: string;
  current_mode: MessageMode;
  new_mode: MessageMode;
}

/** Response from a cascade mode change (or dry-run preview). */
export interface CascadePreview {
  agent_id: string;
  mode: string;
  previous_mode: string;
  cascade?: {
    count: number;
    agent_ids: string[];
    details?: CascadeAgentDetail[];
  };
}

/** Present on agent LIST and DETAIL responses. Cheap: O(1) per agent. */
export interface AgentMessageability {
  /** Whether the viewer can send a message to this agent */
  canMessage: boolean;
  /** Whether this agent can send a message to the viewer */
  canReachViewer: boolean;
  /** Reason code when canMessage is false */
  reason?:
    | 'mode_none'
    | 'mode_lineage_no_ancestry'
    | 'mode_branch_no_edge'
    | 'mode_lineage_agent_to_agent'
    | 'mode_none_sender'
    | 'missing_permission'
    | 'cross_project_disabled'
    | 'cross_project_sender_mode'
    | 'cross_project_target_mode'
    | 'cross_project_inbound_none'
    | 'cross_project_origin_not_member'
    | 'cross_project_untrusted_origin'
    | 'cross_project_surface_unsupported'
    | 'denied';
  /** Reason code when canReachViewer is false */
  replyReason?: string;
}

/**
 * Cross-project inbound policy for a project.
 * Controls which external agents may send messages to agents in this project.
 */
export type CrossProjectInboundPolicy = 'none' | 'members' | 'any';

/**
 * Messaging policy for a project (GET /api/v1/projects/{id}/messaging-policy).
 */
export interface ProjectMessagingPolicy {
  /** Configured inbound policy: "none", "members", "any" */
  crossProjectInbound: CrossProjectInboundPolicy;
  /** Optimistic concurrency revision */
  revision: number;
  /** Effective policy considering Hub switch state */
  effectiveCrossProjectInbound: CrossProjectInboundPolicy;
  /** Whether the Hub-level cross-project messaging is enabled */
  hubCrossProjectEnabled: boolean;
  /** Supported cross-project conversation kinds */
  capabilities?: {
    crossProjectConversationKinds: string[];
  };
}

/**
 * Hub-level messaging settings (GET /api/v1/admin/messaging).
 */
export interface HubMessagingSettings {
  conversation_envelope_switch: boolean;
  cross_project_messaging_enabled: boolean;
  revision: number;
}

/** Present on agent DETAIL responses only. O(n) per agent — too costly for lists. */
export interface AgentMessageabilityDetail extends AgentMessageability {
  /** Count of agents this agent can directly reach */
  reachableAgentCount: number;
  /** Count of users this agent can reach */
  reachableUserCount: number;
}

/**
 * Agent runtime activity (only meaningful when phase=running)
 */
export type AgentActivity =
  | 'working'
  | 'thinking'
  | 'executing'
  | 'waiting_for_input'
  | 'blocked'
  | 'completed'
  | 'limits_exceeded'
  | 'stalled'
  | 'offline';

/**
 * Contextual metadata for the current agent state
 */
export interface AgentDetail {
  toolName?: string;
  message?: string;
  taskSummary?: string;
  currentTurns?: number;
  currentModelCalls?: number;
  startedAt?: string;
}

/**
 * Whether an agent's terminal is accessible.
 * Terminal is available when the agent is in running or stopping phase
 * and not offline.
 */
export function isTerminalAvailable(agent: {
  phase?: AgentPhase;
  activity?: AgentActivity;
}): boolean {
  if (agent.activity === 'offline') return false;
  return agent.phase === 'running' || agent.phase === 'stopping';
}

/**
 * Returns the display status string for an agent.
 * When the agent is running, shows the activity (e.g. 'thinking');
 * otherwise shows the lifecycle phase.
 */
export function getAgentDisplayStatus(agent: Agent): string {
  if (agent.phase === 'running' && agent.activity) {
    return agent.activity;
  }
  return agent.phase;
}

/**
 * Whether the agent is in a running lifecycle phase.
 */
export function isAgentRunning(agent: Agent): boolean {
  return agent.phase === 'running';
}

/**
 * Confirmation copy shown before a best-effort resume of an error-phase
 * agent (POST /start with `{ forceResume: true }`). The agent's home
 * directory and harness session are usually still intact even after a host
 * crash, but the crash itself may have corrupted that state, so the resume
 * is best-effort rather than guaranteed.
 */
export const RESUME_BEST_EFFORT_CONFIRM_MESSAGE =
  'This agent stopped unexpectedly. Resume will try to continue its previous session from the saved home directory. This may fail or behave oddly if the crash corrupted session state. Start instead begins a fresh session with the original task.';

/**
 * A lifecycle action a caller can request for an agent from the UI.
 * `force-resume` posts to the same `/start` endpoint as `start`/`resume`,
 * but with a body asking the hub for a best-effort resume of an
 * error-phase agent's interrupted harness session (see
 * RESUME_BEST_EFFORT_CONFIRM_MESSAGE).
 */
export type AgentLifecycleAction =
  | 'start'
  | 'stop'
  | 'suspend'
  | 'resume'
  | 'delete'
  | 'force-resume';

/**
 * Builds the fetch RequestInit for POSTing an agent lifecycle action.
 * Only `force-resume` needs a JSON body; every other action (including the
 * plain `start` that a suspended/stopped/error agent otherwise uses) posts
 * with no body, matching the Hub's existing `/start` and `/stop` handlers.
 */
export function lifecycleActionRequestInit(action: AgentLifecycleAction): RequestInit {
  if (action === 'force-resume') {
    return {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ forceResume: true }),
    };
  }
  return { method: 'POST' };
}

/**
 * Telemetry event filter configuration.
 */
export interface TelemetryEventsConfig {
  include?: string[];
  exclude?: string[];
}

/**
 * Telemetry attribute redaction and hashing configuration.
 */
export interface TelemetryAttributesConfig {
  redact?: string[];
  hash?: string[];
}

/**
 * Telemetry sampling configuration.
 */
export interface TelemetrySamplingConfig {
  default?: number;
  rates?: Record<string, number>;
}

/**
 * Telemetry filter configuration (event filtering, attribute redaction, sampling).
 */
export interface TelemetryFilterConfig {
  enabled?: boolean;
  events?: TelemetryEventsConfig;
  attributes?: TelemetryAttributesConfig;
  sampling?: TelemetrySamplingConfig;
}

/**
 * Cloud OTLP export configuration.
 */
export interface TelemetryCloudConfig {
  enabled?: boolean;
  endpoint?: string;
  protocol?: string;
  provider?: string;
}

/**
 * Hub telemetry reporting configuration.
 */
export interface TelemetryHubConfig {
  enabled?: boolean;
  report_interval?: string;
}

/**
 * Local debug telemetry output configuration.
 */
export interface TelemetryLocalConfig {
  enabled?: boolean;
  file?: string;
  console?: boolean;
}

/**
 * Top-level telemetry configuration for an agent.
 */
export interface TelemetryConfig {
  enabled?: boolean;
  cloud?: TelemetryCloudConfig;
  hub?: TelemetryHubConfig;
  local?: TelemetryLocalConfig;
  filter?: TelemetryFilterConfig;
}

/**
 * Inline configuration values set at agent creation time.
 */
export interface AgentInlineConfig {
  max_turns?: number;
  max_model_calls?: number;
  max_duration?: string;
  model?: string;
  thinking_level?: number;
  branch?: string;
  task?: string;
  image?: string;
  telemetry?: TelemetryConfig;
}

export type SupportLevel = 'no' | 'partial' | 'yes';

export interface CapabilityField {
  support: SupportLevel;
  reason?: string;
}

export interface HarnessAdvancedCapabilities {
  harness: string;
  limits: {
    max_turns: CapabilityField;
    max_model_calls: CapabilityField;
    max_duration: CapabilityField;
  };
  telemetry: {
    enabled: CapabilityField;
    native_emitter: CapabilityField;
  };
  prompts: {
    system_prompt: CapabilityField;
    agent_instructions: CapabilityField;
  };
  auth: {
    api_key: CapabilityField;
    auth_file: CapabilityField;
    oauth_token: CapabilityField;
    vertex_ai: CapabilityField;
  };
  resume?: CapabilityField;
}

/**
 * Applied configuration snapshot captured at agent creation time.
 */
export interface AgentAppliedConfig {
  image?: string;
  harnessConfig?: string;
  harnessAuth?: string;
  noAuth?: boolean;
  model?: string;
  thinkingLevel?: number;
  profile?: string;
  task?: string;
  attach?: boolean;
  workspace?: string;
  creatorName?: string;
  templateId?: string;
  templateHash?: string;
  inlineConfig?: AgentInlineConfig;
  gcpIdentity?: GCPIdentityConfig;
  agentRole?: string;
}

/**
 * Agent information from the Hub API
 */
export interface Agent {
  id: string;
  name: string;
  projectId: string;
  project?: string;
  template: string;
  phase: AgentPhase;
  activity?: AgentActivity;
  detail?: AgentDetail;
  taskSummary?: string;
  message?: string;
  lastSeen?: string;
  lastActivityEvent?: string;
  // Backend sends "created"/"updated"; legacy frontend code uses "createdAt"/"updatedAt".
  // Accept both so existing pages and new API responses both work.
  created?: string;
  updated?: string;
  createdAt?: string;
  updatedAt?: string;
  harnessConfig?: string;
  harnessAuth?: string;
  resolvedHarness?: string;
  harnessCapabilities?: HarnessAdvancedCapabilities;
  runtimeBrokerId?: string;
  runtimeBrokerName?: string;
  /**
   * Read-only pinned placement of an agent on a flat Runtime Broker (the
   * runtime target it was pinned to and the Runtime Broker serving it).
   * Absent for an unpinned (profile-based) agent.
   */
  pinnedRuntimeTarget?: PinnedRuntimeTarget;
  _capabilities?: Capabilities;

  // Labels and annotations
  labels?: Record<string, string>;
  annotations?: Record<string, string>;

  // Configuration tab fields
  slug?: string;
  image?: string;
  runtime?: string;
  createdBy?: string;
  // The creator's display name. Set on compact list items, which carry no
  // appliedConfig; full items carry it as appliedConfig.creatorName.
  creatorName?: string;
  appliedConfig?: AgentAppliedConfig;

  // Ordered ancestor chain [root, ..., parent]; last entry is the direct
  // parent (user or spawning agent). Drives the lineage graph view.
  ancestry?: string[];

  // Status tab fields (limits tracking)
  currentTurns?: number;
  currentModelCalls?: number;
  startedAt?: string;
  connectionState?: string;

  // Cloud Logging capability (from hub)
  cloudLogging?: boolean;

  // Port forwarding
  exposedPorts?: ExposedPort[];

  // Messaging authorization scope
  messageMode?: MessageMode;
  _messageability?: AgentMessageability | AgentMessageabilityDetail;

  // Children agent IDs (populated by some API responses)
  childrenIds?: string[];

  // Backend-driven delete lifecycle (ptone/scion#2483 §2.2). The hub always
  // sends this key on REST agents and SSE status deltas; an explicit `null`
  // means no delete is active and must clear any earlier value.
  deletion?: DeletionInfo | null;

  // Computed by the hub: provisioned but never asked to run
  // (ptone/scion#2929). Absent means false.
  provisionedOnly?: boolean;
}

/** `DeletionInfo.state` values the hub publishes (`finalizing` reads as `deleting`). */
export type DeletionState = 'deleting' | 'failed';

/**
 * Failure codes on a `failed` deletion (pkg/store/deletion_view.go). Kept
 * open-ended so an unknown future code still renders its `error` text.
 */
export type DeletionCode =
  | 'runtime_error'
  | 'conflict'
  | 'in_doubt'
  | 'abandoned'
  | 'revoke_failed'
  | 'finalize_failed'
  | 'runtime_unavailable'
  | (string & Record<never, never>);

/**
 * The hub's computed delete view for an agent (Go `store.DeletionInfo`).
 * While `deleting`, the engine renews `leaseExpiresAt` about every 20s; a
 * view whose lease passes without renewal reads as `failed`/`abandoned`.
 */
export interface DeletionInfo {
  state: DeletionState;
  code?: DeletionCode;
  error?: string;
  soft: boolean;
  claim: number;
  startedAt: string;
  /** Set while `deleting`. */
  leaseExpiresAt?: string;
  /** Set on `failed`, except `in_doubt` and finalizing rows. */
  expiresAt?: string;
  /**
   * `finalizing` when the hub row's stored state is finalizing (teardown
   * has run; finalize is running or was interrupted), on both the deleting
   * and the failed view; absent otherwise. Such a row never expires from
   * view and blocks start until a retry or force (design note D4).
   */
  stage?: DeletionStage;
}

/** `DeletionInfo.stage` values (open-ended for forward compatibility). */
export type DeletionStage = 'finalizing' | (string & Record<never, never>);

/**
 * Template configuration embedded in template detail responses.
 * Partial subset of the Go `store.TemplateConfig` struct, covering fields used by the frontend.
 */
export interface TemplateConfig {
  harness?: string;
  image?: string;
  configDir?: string;
  env?: Record<string, string>;
  detached?: boolean;
  commandArgs?: string[];
  model?: string;
  messageMode?: MessageMode;
}

/**
 * Template information from the Hub API
 */
export interface Template {
  id: string;
  name: string;
  slug: string;
  displayName?: string;
  description?: string;
  harness: string;
  defaultHarnessConfig?: string;
  status: string;
  scope: string;
  scopeId?: string;
  contentHash?: string;
  files?: TemplateFileInfo[];
  config?: TemplateConfig;
  /** Creation and last-update times, as the hub sends them. */
  created?: string;
  updated?: string;
  /** Older names for created / updated; kept optional for compatibility. */
  createdAt?: string;
  updatedAt?: string;
  _capabilities?: Capabilities;
}

export interface TemplateFileInfo {
  path: string;
  size: number;
  hash: string;
  mode?: string;
}

export interface HarnessConfigData {
  harness?: string;
  image?: string;
  user?: string;
  model?: string;
  args?: string[];
  env?: Record<string, string>;
}

export interface HarnessConfig {
  id: string;
  name: string;
  slug: string;
  displayName?: string;
  description?: string;
  harness: string;
  config?: HarnessConfigData;
  status: string;
  scope: string;
  scopeId?: string;
  contentHash?: string;
  sourceUrl?: string;
  files?: TemplateFileInfo[];
  imageStatus?: string;
  imageStatusCheckedAt?: string;
  created?: string;
  updated?: string;
  _capabilities?: Capabilities;
}

/**
 * Runtime Broker status enumeration
 */
/**
 * Scope for environment variables and secrets
 */
export type ResourceScope = 'user' | 'project' | 'runtime_broker' | 'hub';

/**
 * Injection mode for environment variables
 */
export type InjectionMode = 'always' | 'as_needed';

/**
 * Environment variable from the Hub API (GET /api/v1/env)
 */
export interface EnvVar {
  id: string;
  key: string;
  value: string;
  scope: ResourceScope;
  scopeId: string;
  description?: string;
  sensitive: boolean;
  injectionMode: InjectionMode;
  secret: boolean;
  allowProgeny?: boolean;
  created: string;
  updated: string;
  createdBy?: string;
}

/**
 * Secret type enumeration
 */
export type SecretType = 'environment' | 'variable' | 'file';

/**
 * Secret metadata from the Hub API (GET /api/v1/secrets)
 * Note: secret values are never returned from the API
 */
export interface Secret {
  id: string;
  key: string;
  type: SecretType;
  target?: string;
  scope: ResourceScope;
  scopeId: string;
  description?: string;
  injectionMode: InjectionMode;
  allowProgeny?: boolean;
  version: number;
  secretRef?: string;
  created: string;
  updated: string;
  createdBy?: string;
  updatedBy?: string;
}

/**
 * Shared directory for a project (project-level shared filesystem between agents)
 */
export interface SharedDir {
  name: string;
  read_only?: boolean;
  in_workspace?: boolean;
}

export type BrokerStatus = 'online' | 'offline' | 'degraded';

/**
 * Capabilities advertised by a Runtime Broker
 */
export interface BrokerCapabilities {
  webPTY: boolean;
  sync: boolean;
  attach: boolean;
}

/**
 * Runtime profile available on a broker
 */
export interface BrokerProfile {
  name: string;
  type: string;
  available: boolean;
}

/**
 * Runtime Broker information from the Hub API
 */
/** Stored descriptor of a flat Runtime Broker's single runtime target. */
export interface RuntimeTargetDescriptor {
  id: string;
  type: string;
  displayName?: string;
}

/** Read-only view of an agent's pinned placement (Hub `pinnedRuntimeTarget`). */
export interface PinnedRuntimeTarget {
  id: string;
  type: string;
  runtimeBrokerId: string;
}

export interface RuntimeBroker {
  id: string;
  name: string;
  slug: string;
  version: string;
  status: BrokerStatus;
  connectionState: string;
  lastHeartbeat: string;
  capabilities?: BrokerCapabilities;
  profiles?: BrokerProfile[];
  /** Present only for a flat Runtime Broker (single runtime target). */
  runtimeTarget?: RuntimeTargetDescriptor;
  autoProvide: boolean;
  endpoint?: string;
  labels?: Record<string, string>;
  createdBy?: string;
  createdByName?: string;
  /** Creation and last-update times, as the hub sends them. */
  created?: string;
  updated?: string;
  /** Older names for created / updated; read as a fallback. */
  createdAt?: string;
  updatedAt?: string;
  _capabilities?: Capabilities;
  /**
   * The broker's effective max_agents_per_broker ceiling (ptone/scion#2061
   * P2.2, design.md §5.6, §5.9). Mirrors Go
   * RuntimeBrokerWithCapabilities.AgentLimit (pkg/hub/response_types.go)
   * exactly — hand-written since there is no Go->TS generator (design.md
   * §6). Absent when unlimited, or when resolution didn't run or failed;
   * never 0 (a non-positive effective limit means unlimited).
   */
  agentLimit?: number;
  /**
   * The number of active max_agents_per_broker reservations held by this
   * broker. Absent only when resolution didn't run or failed. Unlike
   * agentLimit, it is still present (possibly non-zero) when the broker is
   * unlimited — agentLimit's absence there means "no cap", not "no count".
   */
  agentCount?: number;
  /**
   * The precedence step that produced agentLimit: "broker" | "entitlement" |
   * "hub_default" | "unlimited" | "not_enforced". This names the step, not
   * whether the result is a cap: when the effective limit is <= 0
   * (unlimited), agentLimit is absent but agentLimitSource is still
   * whichever step produced it ("broker" for a settings.maxAgents=0
   * override, "entitlement"/"hub_default" for a 0 binding or default).
   * "unlimited" itself means no limit definition or no quota service is
   * configured hub-wide — in that case resolution does not count either,
   * and all three fields (agentLimit/agentCount/agentLimitSource) are
   * absent together.
   *
   * "not_enforced" (design.md Amendment A1) means the P1b enforcement
   * switch is off: agentLimit keeps whatever the precedence steps resolved
   * (a cap, or absent when that resolves to unlimited, exactly as above),
   * but the value is informational only — it is not currently applied.
   * Renderers must show this visibly, not only in a tooltip.
   */
  agentLimitSource?: string;
}

/**
 * General per-broker settings document (ptone/scion#2061 P2,
 * ptone/scion#2177). Mirrors the Go store.BrokerSettings JSON tags exactly
 * (pkg/store/models.go) — hand-written since there is no Go->TS generator
 * (design.md §6). undefined/absent means "inherit" (fall through to the
 * entitlement engine / hub-wide default); 0 means unlimited.
 */
export interface BrokerSettings {
  maxAgents?: number;
}

/**
 * The resolved value of one broker-settings key plus the precedence step
 * that produced it (design.md §5.2, §5.9). Mirrors Go EffectiveSetting
 * (pkg/hub/broker_settings_handlers.go).
 */
export interface EffectiveSetting {
  /** null only when resolution errored outright; source is then "" too.
   * Every other outcome, including "no quota configured" (source
   * "unlimited"), is a concrete number (0 = unlimited). */
  value: number | null;
  /** "broker" | "entitlement" | "hub_default" | "unlimited" | "not_enforced" | "" */
  source: string;
  /** Current active-reservation count for this key, the same value Reserve
   * counts against (shared via brokerCapacity, AC-P2-9/AC-P2-10). Omitted
   * when resolution failed or the key isn't quota-backed. */
  count?: number;
  /** What value/source would apply if this key's own broker override were
   * cleared (the entitlement engine: bindings, then the hub-wide default).
   * Populated in every state, including while an override is active, so the
   * UI can label "Use hub default (N)" correctly at exactly the moment an
   * admin is deciding whether to clear it. */
  inherited: InheritedSetting;
}

/**
 * EffectiveSetting.inherited's shape (design.md §5.6, review round 2 R2).
 * Mirrors Go InheritedSetting (pkg/hub/broker_settings_handlers.go).
 */
export interface InheritedSetting {
  /** null only when resolution errored; source is then "" too. */
  value: number | null;
  /** "entitlement" | "hub_default" | "unlimited" | "" */
  source: string;
}

/**
 * GET/PUT /api/v1/runtime-brokers/{id}/settings response (design.md §5.4).
 * Mirrors Go BrokerSettingsResponse (pkg/hub/broker_settings_handlers.go).
 */
export interface BrokerSettingsResponse {
  brokerId: string;
  /** Stored values only; a key absent here means "inherit". */
  settings: BrokerSettings;
  effective: {
    maxAgents: EffectiveSetting;
  };
  /** Optimistic concurrency revision; 0 when the broker has no settings row. */
  revision: number;
  updatedBy?: string;
  /** Absent when the broker has no settings row yet. */
  updated?: string;
  /** Per-key write permission for the caller. */
  _capabilities: {
    update: boolean;
  };
}

// ---------------------------------------------------------------------------
// Messages (inbox)
// ---------------------------------------------------------------------------

/**
 * Message from the Hub API (GET /api/v1/messages or GET /api/v1/agents/{id}/messages)
 */
export interface Message {
  id: string;
  projectId: string;
  sender: string;
  senderId: string;
  recipient: string;
  recipientId: string;
  msg: string;
  type: string;
  urgent?: boolean;
  broadcasted?: boolean;
  read?: boolean;
  agentId: string;
  createdAt: string;
  /** Channel the message was sent through (e.g. "web", "discord"). Phase 0 addition. */
  channel?: string;
  /** Thread identifier (e.g. "agent:<agentId>"). Phase 0 addition. */
  threadId?: string;
  /** Group identifier for related messages. */
  groupId?: string;
  /** Dispatch state: "pending", "dispatched", or "failed". */
  dispatchState?: string;
  /** Reason for dispatch failure, if any. */
  dispatchFailureReason?: string;
  /**
   * Machine-readable dispatch failure code, e.g. "agent_unreachable"
   * (nc-delivery-unreachable). Only present on rows returned by the chat v2
   * send response; history rows fall back to matching the reason prefix.
   */
  dispatchFailureCode?: string;
  /** Whether the message was sent with plain formatting. */
  plain?: boolean;
  /** File attachment paths. */
  attachments?: string[];
  /** Arbitrary metadata attached to the message. */
  metadata?: Record<string, unknown>;
  /** Server-derived sender project ID for cross-project provenance. */
  senderProjectId?: string;
  /** Server-derived recipient project ID for cross-project provenance. */
  recipientProjectId?: string;
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

/**
 * Notification from the Hub API (GET /api/v1/notifications)
 */
export interface Notification {
  id: string;
  subscriptionId: string;
  agentId: string;
  projectId: string;
  subscriberType: string;
  subscriberId: string;
  status: string;
  message: string;
  dispatched: boolean;
  acknowledged: boolean;
  createdAt: string;
}

// ---------------------------------------------------------------------------
// Notification Subscriptions
// ---------------------------------------------------------------------------

/**
 * Subscription scope — watch a single agent or an entire project.
 */
export type SubscriptionScope = 'agent' | 'project';

/**
 * Notification subscription from the Hub API
 * (GET /api/v1/notifications/subscriptions)
 */
export interface Subscription {
  id: string;
  scope: SubscriptionScope;
  agentId?: string;
  agentSlug?: string;
  subscriberType: string;
  subscriberId: string;
  projectId: string;
  triggerActivities: string[];
  createdAt: string;
  createdBy: string;
}

// ---------------------------------------------------------------------------
// Access control capabilities
// ---------------------------------------------------------------------------

/**
 * Capabilities attached to API resource responses.
 * Each resource includes `_capabilities: { actions: [...] }` describing
 * what the current user is allowed to do with that resource.
 */
export interface Capabilities {
  actions: string[];
}

/**
 * Membership-specific capabilities returned in the project members API
 * `_capabilities` field. Provides granular boolean flags indicating which
 * tiers of project membership the current user can manage.
 */
export interface MembershipCapabilities {
  canManageMembers: boolean;
  canManageAdmins: boolean;
  canManageOwners: boolean;
  canTransfer: boolean;
  actions: string[];
  /**
   * Whether the actor may grant and remove custom project roles
   * (ptone/scion#2529). Decided server-side by the same authority function
   * the members PUT uses; the UI never infers it from owner authority.
   * Optional: absent means false.
   */
  canManageCustomRoles?: boolean;
}

/**
 * One project-scope role binding as returned by the project members API,
 * enriched with role and display names.
 */
export interface ProjectMemberBinding {
  id: string;
  roleDefinitionId: string;
  roleName: string;
  principalType: string;
  principalId: string;
  principalDisplayName?: string;
  scopeType: string;
  scopeId: string;
  createdAt: string;
  notBefore?: string;
  expiresAt?: string;
  /** 'direct' for direct bindings, otherwise the group it is inherited through. */
  source: string;
  sourceGroupName?: string;
  /** 'builtin' (owner/admin/member) or 'custom'. */
  roleKind?: 'builtin' | 'custom';
}

/**
 * One principal's project membership: the item type of
 * `GET /api/v1/projects/{id}/members?groupBy=principal` and the body of
 * `PUT /api/v1/projects/{id}/members/principals/{type}/{id}`.
 */
export interface ProjectMemberGroup {
  principalType: string;
  principalId: string;
  principalDisplayName?: string;
  /** The built-in membership role name, or '' when the principal holds none. */
  builtInRoleName: string;
  /** Built-in binding first, then custom bindings by role name. */
  bindings: ProjectMemberBinding[];
  /** PUT responses only. */
  changed?: boolean;
}

/**
 * A project-scoped role the members dialog can offer, from
 * `GET /api/v1/projects/{id}/members/assignable-roles`. `grantable` is the
 * members PUT's decision for newly creating the role on a principal that
 * does not hold it (principal-agnostic, op=add).
 */
export interface AssignableProjectRole {
  id: string;
  name: string;
  description: string;
  roleKind: 'builtin' | 'custom';
  grantable: boolean;
  /** Empty when grantable; otherwise the PUT's refusal reason. */
  reason: string;
  denialCode?: string;
  details?: Record<string, unknown>;
}

/**
 * Check whether a capability set permits a specific action.
 * Returns false (fail-closed) when capabilities are undefined.
 */
export function can(capabilities: Capabilities | undefined, action: string): boolean {
  if (!capabilities) return false;
  return capabilities.actions.includes(action);
}

/**
 * Whether the viewer may run agent lifecycle actions (start, stop, suspend,
 * restart, restore).
 *
 * These are authorized server-side by `authorizeAgentLifecycle` with the
 * `agent.lifecycle` permission (ActionLifecycle). Project owners/admins hold
 * it for every agent in the project; other users get it on agents they own or
 * spawned. It is deliberately separate from `attach`, which gates terminal /
 * exec / env access and is NOT granted to owners/admins on other members'
 * agents, because those agents run with their owner's secrets
 * (miller79/scion#88).
 */
export function canLifecycle(capabilities: Capabilities | undefined): boolean {
  return can(capabilities, 'lifecycle');
}

/**
 * Whether the viewer may offer the message composer for an agent. Messaging is
 * authorized server-side by `authorizeAgentMessage` (a scope-level axis with no
 * per-agent capability), so the UI uses per-agent management capability as the
 * proxy: `lifecycle` (owners/admins and the agent's creator) or `attach`.
 * `_messageability` remains the authoritative per-agent signal where present.
 */
export function canMessageAgent(capabilities: Capabilities | undefined): boolean {
  return can(capabilities, 'lifecycle') || can(capabilities, 'attach');
}

/**
 * Check whether a capability set permits any of the given actions.
 * Returns false (fail-closed) when capabilities are undefined.
 */
export function canAny(capabilities: Capabilities | undefined, ...actions: string[]): boolean {
  if (!capabilities) return false;
  return actions.some((a) => capabilities.actions.includes(a));
}

/**
 * Generic wrapper for paginated list responses from the Hub API.
 *
 * Note: The Hub API returns list responses with named keys (e.g., `agents`,
 * `projects`) rather than a generic `items` key. This type is provided as a
 * convenience for new code. Existing components that parse `data.agents` etc.
 * continue to work — the important part is that each item now carries
 * `_capabilities` and the response includes scope-level capabilities.
 */
export interface ListResponse<T> {
  items: T[];
  _capabilities?: Capabilities;
  nextCursor?: string;
  totalCount?: number;
}

// ---------------------------------------------------------------------------
// GCP Identity types
// ---------------------------------------------------------------------------

export type GCPVerificationStatus = 'unverified' | 'verified' | 'failed';

export interface GCPServiceAccount {
  id: string;
  scope: string;
  scopeId: string;
  email: string;
  projectId: string;
  displayName: string;
  defaultScopes: string[];
  verified: boolean;
  verifiedAt: string | null;
  verificationStatus?: GCPVerificationStatus;
  verificationError?: string;
  createdBy: string;
  createdAt: string;
  managed?: boolean;
  managedBy?: string;
  _capabilities?: Capabilities;
}

export interface GCPMintQuotaInfo {
  project_minted: number;
  project_cap: number;
  hub_minted?: number;
  hub_cap?: number;
  global_minted: number;
  global_cap: number;
}

export interface GCPIdentityConfig {
  metadataMode: 'block' | 'passthrough' | 'assign';
  serviceAccountId?: string;
  serviceAccountEmail?: string;
  projectId?: string;
}

export interface GCPIdentityAssignment {
  metadataMode: 'block' | 'passthrough' | 'assign';
  serviceAccountId?: string;
}

// ---------------------------------------------------------------------------
// Policy types (mirrors Go store.Policy)
// ---------------------------------------------------------------------------

/**
 * Condition matching agents delegated from a specific principal.
 */
export interface DelegatedFromCondition {
  principalType: string;
  principalId: string;
}

/**
 * Optional conditional logic for policies.
 */
export interface PolicyConditions {
  labels?: Record<string, string>;
  validFrom?: string;
  validUntil?: string;
  sourceIps?: string[];
  delegatedFrom?: DelegatedFromCondition;
  delegatedFromGroup?: string;
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

export type SkillScope = 'core' | 'global' | 'project' | 'user';
export type SkillVersionStatus = 'draft' | 'published' | 'deprecated' | 'archived';

export interface Skill {
  id: string;
  name: string;
  slug: string;
  description?: string;
  tags?: string[];
  scope: SkillScope;
  scopeId?: string;
  status: string;
  ownerId?: string;
  createdBy?: string;
  created: string;
  updated: string;
  _capabilities?: Capabilities;
}

export interface SkillVersion {
  id: string;
  skillId: string;
  version: string;
  status: SkillVersionStatus;
  contentHash?: string;
  files?: SkillFile[];
  publisherId?: string;
  deprecationMessage?: string;
  replacementUri?: string;
  downloadCount: number;
  created: string;
}

export interface SkillFile {
  path: string;
  size: number;
  hash?: string;
  mode?: string;
}

export interface SkillUploadUrl {
  path: string;
  url: string;
  method: string;
  headers?: Record<string, string>;
  expires: string;
}

export interface SkillDownloadUrl {
  path: string;
  url: string;
  size: number;
  hash?: string;
}

// Skill Registry types (admin only)

export type SkillRegistryStatus = 'active' | 'disabled';
export type SkillRegistryTrustLevel = 'trusted' | 'pinned';
export type SkillRegistryType = 'hub' | 'gcp';

export interface SkillRegistry {
  id: string;
  name: string;
  endpoint: string;
  description?: string;
  type: SkillRegistryType;
  trustLevel: SkillRegistryTrustLevel;
  resolvePath?: string;
  status: SkillRegistryStatus;
  createdBy?: string;
  created: string;
  updated: string;
}

/**
 * Policy effect: allow or deny.
 */
export type PolicyEffect = 'allow' | 'deny';

/**
 * Access control policy from the Hub API.
 * Mirrors the Go `store.Policy` struct.
 */
export interface Policy {
  id: string;
  name: string;
  description?: string;
  scopeType: string;
  scopeId: string;
  resourceType: string;
  resourceId?: string;
  actions: string[];
  effect: PolicyEffect;
  conditions?: PolicyConditions;
  priority: number;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  created: string;
  updated: string;
  createdBy?: string;
}

/**
 * Minimal pre-start hook fields used by the inherited-hub indicator.
 */
export interface PreStartHookSummary {
  id: string;
  name: string;
  slug: string;
  scope: 'project' | 'hub';
  status: string;
}

/**
 * Pre-start hook from the Hub API (project- or hub-scoped).
 * Mirrors the Go `store.ProjectPreStartHook` struct.
 */
export interface PreStartHook extends PreStartHookSummary {
  /** Empty for hub-scoped hooks. */
  projectId?: string;
  description?: string;
  /** Raw script content. Bounded to 64 KB by the Hub API. */
  script: string;
  createdBy?: string;
  updatedBy?: string;
  created: string;
  updated: string;
}

// =============================================================================
// Session Metrics (DB-backed, from M3/M4 milestone)
// =============================================================================

/**
 * A single agent session metrics record.
 * Mirrors the Go `store.AgentSessionMetrics` struct.
 */
export interface AgentSessionMetrics {
  id: string;
  agentId: string;
  projectId: string;
  sessionId: string;
  startedAt: string;
  endedAt?: string;
  status?: string;
  turnCount?: number;
  model?: string;
  tokensInput?: number;
  tokensOutput?: number;
  tokensCached?: number;
  tokensReasoning?: number;
  toolCalls?: Record<string, ToolCallStats>;
  languages?: string[];
  createdAt: string;
}

/** Tool call statistics within a session. */
export interface ToolCallStats {
  calls: number;
  success: number;
  error: number;
}

/** Tool usage summary in aggregate responses. */
export interface ToolUsageSummary {
  name: string;
  calls: number;
  success: number;
  error: number;
}

/** Model usage summary in aggregate responses. */
export interface ModelUsageSummary {
  model: string;
  sessions: number;
}

/**
 * Aggregate session metrics for a single agent.
 * Response from GET /api/v1/agents/{id}/metrics/summary.
 */
export interface AgentMetricsSummary {
  agentId: string;
  totalSessions: number;
  totalTokensInput: number;
  totalTokensOutput: number;
  totalTokensCached: number;
  totalTokensReasoning: number;
  totalToolCalls: number;
  avgSessionDurationMs: number;
  avgTokensPerSession: number;
  mostUsedTools: ToolUsageSummary[];
  mostUsedModels: ModelUsageSummary[];
}

/**
 * Aggregate session metrics for a project.
 * Response from GET /api/v1/projects/{id}/metrics/summary.
 */
export interface ProjectSessionMetricsSummary {
  projectId: string;
  totalSessions: number;
  totalTokensInput: number;
  totalTokensOutput: number;
  totalTokensCached: number;
  totalTokensReasoning: number;
  activeAgents: number;
  mostUsedTools: ToolUsageSummary[];
  mostUsedModels: ModelUsageSummary[];
}
