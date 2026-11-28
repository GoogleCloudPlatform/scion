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
 * Shared TypeScript types for Project Triggers (Stage 2 §9.1 / §9.2,
 * GAR extension points, and P9 action-array readiness).
 */

export interface WebhookEndpointBoundAgent {
  id: string;
  name?: string;
}

export interface WebhookEndpointRateLimit {
  perSecond?: number;
  burst?: number;
}

export interface WebhookEndpoint {
  id: string;
  projectId: string;
  name: string;
  description?: string;
  url: string;
  /** 'hmac' | 'hub_token' */
  auth: string;
  allowProjectAgents?: boolean;
  signatureMode: string;
  maxBodyBytes: number;
  rateLimit?: WebhookEndpointRateLimit;
  replayWindowSeconds: number;
  secretVersion: number;
  secretRotatedAt?: string;
  configVersion: number;
  /** 'active' | 'disabled' */
  status: string;
  boundAgent?: WebhookEndpointBoundAgent;
  /** 'agent' | 'persistent' | '' */
  boundLifetime?: string;
  lastReceivedAt?: string;
  receiveCount: number;
  rejectCount: number;
  createdBy?: string;
  createdAt: string;
  updatedAt: string;
}

export interface WebhookEndpointCreated extends WebhookEndpoint {
  secret?: string;
  triggerId?: string;
  trigger?: TriggerDefinition;
}

export interface WebhookEndpointRotated extends WebhookEndpoint {
  secret: string;
  graceWindowExpires?: string;
}

export interface ListWebhookEndpointsResponse {
  scope?: string;
  endpoints: WebhookEndpoint[];
  nextCursor?: string;
  totalCount: number;
  serverTime?: string;
}

export interface TriggerSourceSpec {
  kind: string;
  id: string;
}

export interface TriggerMatchRuleSpec {
  dialect: string;
  expr: string;
}

export interface TriggerMatchSpec {
  types?: string[];
  dialect?: string;
  expr?: string;
  rules: TriggerMatchRuleSpec[];
  form?: unknown;
  rulesBroken?: boolean;
}

export interface TriggerActionSpec {
  type: string;
  agentId?: string;
  agentName?: string;
  message?: string;
  interrupt?: boolean;
  plain?: boolean;
  wake?: boolean;
  /** 'skip' | 'queue' */
  offline?: string;
  queueTtlSeconds?: number;
  /** 'suffix' | 'skip' */
  nameConflict?: string;
  template?: string;
  task?: string;
  branch?: string;
  labels?: Record<string, string>;
  ephemeral?: boolean;
  agentRole?: string;
  reuseMessage?: string;
  untrustedVars?: string[];
  noAuth?: boolean;
  body?: string;
  target?: string;
  replyInThread?: boolean;
  // Exec action fields — command argv must NEVER be displayed unredacted in the UI.
  command?: string[];
  timeoutSeconds?: number;
  contextAttributes?: string[];
  commandRedacted?: boolean;
  argc?: number;
  commandSha256?: string;
}

export interface TriggerSubjectSpec {
  dialect?: string;
  expr?: string;
  /** 'new' | 'skip' | 'reuse' */
  mode?: string;
  coalesce_window?: string;
}

export interface TriggerNotifySpec {
  onOutcomes?: string[];
  userNotice?: boolean;
  agentId?: string;
  agentName?: string;
}

export interface TriggerDefinition {
  id: string;
  projectId: string;
  name: string;
  description?: string;
  source: TriggerSourceSpec;
  match: TriggerMatchSpec;
  /** Scalar action (Stage 1/2). Mirrored when actions has 1 element under P9. */
  action?: TriggerActionSpec;
  /** Ordered actions[] list (P9, ptone/scion#9065). */
  actions?: TriggerActionSpec[];
  subject: TriggerSubjectSpec;
  maxFiresPerMinute?: number;
  maxConcurrentDispatches?: number;
  notify?: TriggerNotifySpec;
  /** 'active' | 'paused' */
  status: string;
  statusReason?: string;
  authorizationRevision: number;
  lastFireAt?: string;
  lastFireStatus?: string;
  lastFireError?: string;
  fireCount: number;
  errorCount: number;
  createdBy?: string;
  createdAt: string;
  updatedAt: string;
}

export interface ListTriggersResponse {
  scope?: string;
  triggers: TriggerDefinition[];
  nextCursor?: string;
  totalCount: number;
  serverTime?: string;
}

export interface TriggerEventSummary {
  id: string;
  projectId: string;
  source: TriggerSourceSpec;
  type: string;
  subject: string;
  dedupKey: string;
  occurredAt: string;
  receivedAt: string;
  attributes?: Record<string, string>;
  lists?: Record<string, string[]>;
  payloadBytes: number;
  /** 'pending' | 'processed' | 'failed' */
  status: string;
  matchCount: number;
  fireCount: number;
  error?: string;
  processedAt?: string;
  createdAt: string;
}

export interface TriggerEventDetail extends TriggerEventSummary {
  headers?: Record<string, string>;
  payload?: unknown;
  payloadTruncated?: boolean;
  payloadRef?: string;
}

export interface ListTriggerEventsResponse {
  scope?: string;
  events: TriggerEventSummary[];
  nextCursor?: string;
  totalCount: number;
  serverTime?: string;
}

export interface TriggerRuleTraceElement {
  i: number;
  dialect: string;
  class: string;
  reason?: string;
  duration_ms: number;
  provenance?: string;
}

export interface TriggerRuleTrace {
  elements: TriggerRuleTraceElement[];
  unexecuted?: number[];
  budget_remaining_ms?: number;
}

export interface TriggerExecSummary {
  outcome: string;
  exitCode?: number;
  durationMs: number;
  outputBytes: number;
  startedAt?: string;
  finishedAt?: string;
}

export interface TriggerFire {
  id: string;
  triggerId: string;
  eventId: string;
  projectId: string;
  subject?: string;
  actionType?: string;
  actionIndex?: number;
  /** 'coalescing' | 'deferred' | 'firing' | 'waiting' | 'fired' | 'failed' | 'refused' | 'skipped' | 'expired' */
  status: string;
  resultRef?: string;
  targetAgentId?: string;
  waitUntil?: string;
  attempts?: number;
  error?: string;
  matchedThrough?: string;
  decidedDialect?: string;
  ruleTrace?: TriggerRuleTrace;
  notBefore?: string;
  deferredAt?: string;
  execSummary?: TriggerExecSummary;
  startedAt: string;
  finishedAt?: string;
}

export interface ListTriggerFiresResponse {
  scope?: string;
  fires: TriggerFire[];
  nextCursor?: string;
  totalCount: number;
  serverTime?: string;
}

export interface TriggerRenderUsedVar {
  path: string;
  class: string;
}

export interface TriggerRenderSlotResult {
  rendered: string;
  used: TriggerRenderUsedVar[];
}

export interface TriggerRenderSlots {
  task?: TriggerRenderSlotResult;
  name?: TriggerRenderSlotResult;
  branch?: TriggerRenderSlotResult;
  labels?: Record<string, TriggerRenderSlotResult>;
  reuseMessage?: TriggerRenderSlotResult;
  message?: TriggerRenderSlotResult;
  body?: TriggerRenderSlotResult;
}

export interface TriggerRenderError {
  slot: string;
  path?: string;
  kind: string;
  message?: string;
}

export interface TriggerRenderResponse {
  slots: TriggerRenderSlots;
  errors: TriggerRenderError[];
  notes?: string[];
}

export interface TriggerDialectOption {
  value: string;
  label: string;
}

/**
 * Dialects admitted for the `trigger` consumer in Stage 2.
 * Additional dialects (e.g., system_one) are added only when granted.
 */
export const ADMITTED_TRIGGER_DIALECTS: readonly TriggerDialectOption[] = [
  { value: 'cel', label: 'CEL' },
];

export interface TriggerRuleRowError {
  category?: string;
  message: string;
  line?: number;
  column?: number;
  dialect?: string;
  reason?: string;
}

export interface ProjectAgentOption {
  id: string;
  name: string;
  status?: string;
}

/**
 * Normalises a trigger's action(s) into a list for P9 readiness:
 * returns `actions` when non-empty, otherwise `[action]` if present.
 */
export function getTriggerActions(
  trigger: Pick<TriggerDefinition, 'action' | 'actions'>
): TriggerActionSpec[] {
  if (trigger.actions && trigger.actions.length > 0) {
    return trigger.actions;
  }
  if (trigger.action) {
    return [trigger.action];
  }
  return [];
}

/**
 * Extracts per-row (0-based) compile errors from a Hub 400 `invalid_rule` JSON body.
 * The Hub returns 1-based `element` indices in `error.details.elements[]`.
 */
export function parseTriggerRuleRowErrors(body: unknown): Record<number, TriggerRuleRowError> {
  const out: Record<number, TriggerRuleRowError> = {};
  if (!body || typeof body !== 'object') return out;
  const errObj = (body as { error?: { code?: string; details?: Record<string, unknown> } }).error;
  if (!errObj || errObj.code !== 'invalid_rule') return out;
  const details = errObj.details;
  if (!details || typeof details !== 'object') return out;

  const elements = details['elements'];
  if (Array.isArray(elements)) {
    for (const raw of elements) {
      if (!raw || typeof raw !== 'object') continue;
      const entry = raw as Record<string, unknown>;
      const elNum = typeof entry['element'] === 'number' ? entry['element'] : undefined;
      if (elNum === undefined || elNum < 1) continue;
      const idx = elNum - 1;
      const message =
        typeof entry['message'] === 'string' && entry['message']
          ? entry['message']
          : typeof entry['reason'] === 'string' && entry['reason']
            ? entry['reason']
            : 'Invalid match rule';
      const rowErr: TriggerRuleRowError = { message };
      if (typeof entry['category'] === 'string') rowErr.category = entry['category'];
      if (typeof entry['line'] === 'number') rowErr.line = entry['line'];
      if (typeof entry['column'] === 'number') rowErr.column = entry['column'];
      if (typeof entry['dialect'] === 'string') rowErr.dialect = entry['dialect'];
      if (typeof entry['reason'] === 'string') rowErr.reason = entry['reason'];
      out[idx] = rowErr;
    }
  } else if (typeof details['message'] === 'string') {
    const rowErr: TriggerRuleRowError = { message: details['message'] };
    if (typeof details['category'] === 'string') rowErr.category = details['category'];
    if (typeof details['line'] === 'number') rowErr.line = details['line'];
    if (typeof details['column'] === 'number') rowErr.column = details['column'];
    out[0] = rowErr;
  }
  return out;
}

/**
 * Formats an exec action summary without ever revealing unredacted argv.
 */
export function formatRedactedExecSummary(action: TriggerActionSpec): string {
  const argc =
    typeof action.argc === 'number'
      ? action.argc
      : Array.isArray(action.command)
        ? action.command.length
        : 0;
  const sha = action.commandSha256 ? action.commandSha256.slice(0, 12) : '';
  if (sha) {
    return `exec (${argc} arg${argc === 1 ? '' : 's'}, sha256:${sha}…, redacted)`;
  }
  return `exec (${argc} arg${argc === 1 ? '' : 's'}, redacted)`;
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Splits a free-text agent reference into `{ agentId, agentName }`:
 * UUID-shaped values map to `agentId` (resolved via GetAgent); any other
 * non-empty string maps to `agentName` (resolved by slug/name on the Hub).
 */
export function splitAgentRef(raw: string): { agentId: string; agentName: string } {
  const val = raw.trim();
  if (!val) return { agentId: '', agentName: '' };
  return UUID_RE.test(val) ? { agentId: val, agentName: '' } : { agentId: '', agentName: val };
}

/**
 * Returns the Shoelace badge variant for a trigger fire status.
 */
export function fireStatusBadgeVariant(
  status: string
): 'success' | 'danger' | 'warning' | 'primary' | 'neutral' {
  switch (status) {
    case 'fired':
      return 'success';
    case 'failed':
    case 'refused':
    case 'expired':
      return 'danger';
    case 'waiting':
    case 'coalescing':
    case 'deferred':
      return 'warning';
    case 'firing':
      return 'primary';
    case 'skipped':
    default:
      return 'neutral';
  }
}
