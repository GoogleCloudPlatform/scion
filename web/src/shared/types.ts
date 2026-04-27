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
 * User information
 */
export interface User {
  id: string;
  email: string;
  name: string;
  avatar?: string | undefined;
  role?: UserRole | undefined;
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
  status: 'active' | 'suspended';
  created: string;
  lastLogin?: string;
  lastSeen?: string;
  _capabilities?: Capabilities;
}

/**
 * Group type enumeration
 */
export type GroupType = 'explicit' | 'grove_agents';

/**
 * Group information from the Hub API (GET /api/v1/groups)
 */
export interface AdminGroup {
  id: string;
  name: string;
  slug: string;
  description?: string;
  groupType: GroupType;
  groveId?: string;
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
 * Grove status enumeration
 */
export type GroveStatus = 'active' | 'inactive' | 'error';

/**
 * Grove information from the Hub API
 */
/**
 * Grove type enumeration
 */
export type GroveType = 'linked' | 'hub-native';

export interface GitHubAppGroveStatus {
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

export interface Grove {
  id: string;
  name: string;
  slug?: string;
  path: string;
  gitRemote?: string;
  groveType?: GroveType;
  status: GroveStatus;
  visibility?: string;
  labels?: Record<string, string>;
  defaultRuntimeBrokerId?: string;
  ownerId?: string;
  ownerName?: string;
  agentCount: number;
  createdAt: string;
  updatedAt: string;
  _capabilities?: Capabilities;
  sharedDirs?: SharedDir[];
  githubInstallationId?: number | undefined;
  githubPermissions?: GitHubTokenPermissions | undefined;
  githubAppStatus?: GitHubAppGroveStatus | undefined;
  cloudLogging?: boolean;
}

/**
 * Check whether a grove is a shared-workspace git grove.
 */
export function isSharedWorkspace(grove: Grove): boolean {
  return !!grove.gitRemote && grove.labels?.['scion.dev/workspace-mode'] === 'shared';
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
  | 'error';

/**
 * Agent runtime activity (only meaningful when phase=running)
 */
export type AgentActivity =
  | 'idle'
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
export function isTerminalAvailable(agent: Agent): boolean {
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
}

/**
 * Applied configuration snapshot captured at agent creation time.
 */
export interface AgentAppliedConfig {
  image?: string;
  harnessConfig?: string;
  harnessAuth?: string;
  model?: string;
  profile?: string;
  task?: string;
  attach?: boolean;
  workspace?: string;
  creatorName?: string;
  templateId?: string;
  templateHash?: string;
  inlineConfig?: AgentInlineConfig;
  gcpIdentity?: GCPIdentityConfig;
}

/**
 * Agent information from the Hub API
 */
export interface Agent {
  id: string;
  name: string;
  groveId: string;
  grove?: string;
  template: string;
  phase: AgentPhase;
  activity?: AgentActivity;
  detail?: AgentDetail;
  taskSummary?: string;
  message?: string;
  lastSeen?: string;
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
  _capabilities?: Capabilities;

  // Configuration tab fields
  slug?: string;
  image?: string;
  runtime?: string;
  visibility?: string;
  createdBy?: string;
  appliedConfig?: AgentAppliedConfig;

  // Status tab fields (limits tracking)
  currentTurns?: number;
  currentModelCalls?: number;
  startedAt?: string;
  connectionState?: string;

  // Cloud Logging capability (from hub)
  cloudLogging?: boolean;
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
  createdAt: string;
  updatedAt: string;
  _capabilities?: Capabilities;
}

export interface TemplateFileInfo {
  path: string;
  size: number;
  hash: string;
  mode?: string;
}

/**
 * Runtime Broker status enumeration
 */
/**
 * Scope for environment variables and secrets
 */
export type ResourceScope = 'user' | 'grove' | 'runtime_broker' | 'hub';

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
 * Shared directory for a grove (grove-level shared filesystem between agents)
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
  autoProvide: boolean;
  endpoint?: string;
  createdBy?: string;
  createdByName?: string;
  createdAt: string;
  updatedAt: string;
  _capabilities?: Capabilities;
}

// ---------------------------------------------------------------------------
// Messages (inbox)
// ---------------------------------------------------------------------------

/**
 * Message from the Hub API (GET /api/v1/messages or GET /api/v1/agents/{id}/messages)
 */
export interface Message {
  id: string;
  groveId: string;
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
  groveId: string;
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
 * Subscription scope — watch a single agent or an entire grove.
 */
export type SubscriptionScope = 'agent' | 'grove';

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
  groveId: string;
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
 * Check whether a capability set permits a specific action.
 * Returns false (fail-closed) when capabilities are undefined.
 */
export function can(capabilities: Capabilities | undefined, action: string): boolean {
  if (!capabilities) return false;
  return capabilities.actions.includes(action);
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
 * `groves`) rather than a generic `items` key. This type is provided as a
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
  grove_minted: number;
  grove_cap: number;
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
