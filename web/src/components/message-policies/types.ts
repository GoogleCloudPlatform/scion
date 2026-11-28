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
 * Shared TypeScript types for the Project Message Policies UI (U1 read-only).
 * Mirrors `pkg/store/message_policy_store.go`, `pkg/messagepolicy/validate.go`,
 * `pkg/messagepolicy/projection.go`, and `pkg/hub/handlers_message_policies.go`.
 */

export type MessagePolicyMode = 'shadow' | 'enforce';
export type MessagePolicyOutcome = 'admit' | 'reject';
export type MessagePolicyClass = 'pass' | 'violation' | 'uncertain' | 'failed';
export type MessagePolicyDecisionAction = 'admitted' | 'rejected' | 'shadow';

export interface CoverageInfo {
  enforced_surfaces: string[];
  uncovered_agent_paths: string[];
}

export interface MessagePolicySenderSelector {
  agent_ids?: string[];
  all_agents?: boolean;
}

export interface MessagePolicyRecipientSelector {
  kinds?: string[];
  agent_ids?: string[];
}

export interface MessagePolicyScope {
  surfaces: string[];
  sender?: MessagePolicySenderSelector;
  recipient?: MessagePolicyRecipientSelector;
}

export interface MessagePolicy {
  id: string;
  project_id: string;
  name: string;
  description?: string;
  enabled: boolean;
  mode: string;
  scope: MessagePolicyScope;
  violation_rule: string;
  system_one_rule?: string;
  system_one_refs?: string[];
  on_uncertain?: string;
  on_failed: string;
  on_failed_reasons?: Record<string, string>;
  reject_reason?: string;
  reject_remediation?: string;
  revision: number;
  created_by?: string;
  updated_by?: string;
  created_at: string;
  updated_at: string;
  coverage?: CoverageInfo;
}

export interface ListMessagePoliciesResponse {
  policies: MessagePolicy[];
  coverage: CoverageInfo;
  next_cursor?: string;
  total_count: number;
}

export interface RuleRefProjection {
  consumer?: string;
  rule_id?: string;
  revision?: number;
  content_hash?: string;
  dialect?: string;
  native?: boolean;
}

export interface MetricProjection {
  name: string;
  value: number;
  unit?: string;
}

export interface AnswerProjection {
  question_id: string;
  primitive: string;
  status: string;
  required: boolean;
  abstained?: boolean;
  tied?: boolean;
  bool?: boolean;
  choice?: string;
  distribution?: Record<string, number>;
  metrics?: Record<string, MetricProjection>;
  native_metrics?: Record<string, MetricProjection>;
  reported_model?: string;
  notes?: string[];
}

export interface AttemptProjection {
  instance?: string;
  instance_revision?: number;
  adapter?: string;
  adapter_version?: string;
  configured_model?: string;
  reported_model?: string;
  location?: string;
  duration_ms: number;
  requests?: number;
  retries?: number;
  outcome?: string;
  reason?: string;
  http_status_class?: string;
}

export interface InputFieldProjection {
  name: string;
  bytes: number;
  truncated?: boolean;
}

export interface EvaluationUsageProjection {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  requests?: number;
  retries?: number;
}

/**
 * Content-free System One evaluation projection persisted on
 * `message_policy_decisions.evaluation`. Carries no message body, prompt text,
 * or copy facts.
 */
export interface EvaluationProjection {
  eval_id?: string;
  rule?: RuleRefProjection;
  class?: string;
  reason?: string;
  configured_model?: string;
  resolved_model?: string;
  model_unreported?: boolean;
  instance?: string;
  answers?: Record<string, AnswerProjection>;
  attempts?: AttemptProjection[];
  fallback_used?: boolean;
  fallback_cause?: string;
  cache_hit?: boolean;
  source_eval_id?: string;
  revalidation_due?: boolean;
  input_fields?: InputFieldProjection[];
  truncated?: boolean;
  elapsed_ms?: number;
  usage?: EvaluationUsageProjection;
}

export interface MessagePolicyDecision {
  id: string;
  project_id: string;
  policy_id: string;
  policy_name?: string;
  policy_revision: number;
  mode: string;
  surface: string;
  sender_kind?: string;
  sender_id?: string;
  recipient_kind?: string;
  recipient_id?: string;
  message_id?: string;
  class: string;
  action: string;
  reason?: string;
  cel_error_reason?: string;
  evaluation?: EvaluationProjection | null;
  elapsed_ms: number;
  created_at: string;
}

export interface ListMessagePolicyDecisionsResponse {
  decisions: MessagePolicyDecision[];
  next_cursor?: string;
  total_count: number;
}

export interface ProjectAgentSummary {
  id: string;
  name: string;
}

/**
 * Non-agent messaging paths documented as outside agent-egress policy
 * evaluation (proposal §2(a)). Displayed separately from the Hub API's
 * `coverage.uncovered_agent_paths`.
 */
export const STATIC_NON_AGENT_UNCOVERED_PATHS: readonly string[] = [
  'scheduled deliveries',
  'user-originated sends',
  'inbound bridges',
];

export const KNOWN_ENFORCED_SURFACES: readonly string[] = [
  'agent_to_user',
  'agent_to_conversation',
  'agent_dm',
  'mention_delivery',
];

const SURFACE_LABELS: Record<string, string> = {
  agent_to_user: 'agent → user',
  agent_to_conversation: 'agent → conversation',
  agent_dm: 'agent DM',
  mention_delivery: 'mention delivery',
  group_post: 'group post',
  group_set: 'group-set fan-out',
  mention_fanout: 'mention fan-out',
  broadcast: 'broadcast',
};

export function formatSurfaceLabel(surface: string): string {
  return SURFACE_LABELS[surface] ?? surface;
}

export function hasSystemOneRule(policy: MessagePolicy): boolean {
  return Boolean(
    (policy.system_one_rule && policy.system_one_rule.trim().length > 0) ||
    (policy.system_one_refs && policy.system_one_refs.length > 0)
  );
}
