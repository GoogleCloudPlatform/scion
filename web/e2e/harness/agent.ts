// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Containerless real-hub agent-sender fixture for E2E tests (ptone/scion#7776).
 *
 * Creates a real stored agent record on a running `startHub(...)` test hub
 * (`provisionOnly: true` against the co-located runtime broker, so no Docker
 * daemon or live agent container is needed) and mints a valid
 * `X-Scion-Agent-Token` JWT matching `pkg/hub/agenttoken.go`.
 *
 * The resulting `AgentSender` authenticates as `AgentIdentity` on both:
 *   - `RouteAgentSelf` endpoints (`/api/v1/agents/{id}/*`, e.g.
 *     `POST /api/v1/agents/{id}/outbound-message`,
 *     `POST /api/v1/agents/{id}/status`), and
 *   - `RouteAgentToken` endpoints (`/api/v1/agent/*`, e.g.
 *     `GET /api/v1/agent/gcp-token` and `/api/v1/agent/decisions*`).
 *
 * Hub prerequisite: start the hub with `runtimeBroker: true, testIdentities: true`
 * and either `testLogin: true` (the default, which passes `--session-secret`)
 * or `env: { SCION_SERVER_SESSION_SECRET: E2E_SESSION_SECRET }` so the hub's
 * `agent_signing_key` is deterministically derived.
 */

import * as crypto from 'node:crypto';
import type { ChoiceResponseSetView, ChoiceResponseView } from '../../src/shared/types.js';
import { E2E_SESSION_SECRET, type HubState } from './hub.js';

const AGENT_TOKEN_ISSUER = 'scion-hub';
const AGENT_TOKEN_AUDIENCE = 'scion-hub-api';
const CURRENT_AGENT_SCOPE_SCHEMA = 1;
const DEFAULT_REQUEST_TIMEOUT_MS = 15_000;
const DEFAULT_DELIVERY_SETTLE_TIMEOUT_MS = 10_000;

export type TestAgentRole = 'readonly' | 'baseline' | 'full';
export type TestAgentMessageMode = 'none' | 'lineage' | 'branch' | 'project' | 'hub';

/** Stock JWT scope bundles matching `ScopesForRole` in `pkg/hub/agentrole.go`. */
const ROLE_SCOPES: Record<TestAgentRole, readonly string[]> = {
  readonly: ['project:read', 'project:artifact:read'],
  baseline: [
    'project:read',
    'agent:status:update',
    'agent:token:refresh',
    'project:agent:notify',
    'agent:port:forward',
    'project:artifact:read',
    'project:artifact:write',
  ],
  full: [
    'project:read',
    'agent:status:update',
    'agent:token:refresh',
    'project:agent:notify',
    'agent:port:forward',
    'project:agent:create',
    'project:agent:sa_assign',
    'project:agent:lifecycle',
    'project:secret:read',
    'project:template:write',
    'project:agent:set_message_mode',
    'project:artifact:read',
    'project:artifact:write',
  ],
};

/** Credentials used for hub user/admin setup or history-inspection calls. */
export interface ViewerAuthOptions {
  /** Bearer token (user `accessToken` or `hub.devToken`). */
  authToken?: string;
  /** Browser session `Cookie` header value (when using cookie auth). */
  cookie?: string;
}

export interface CreateTestAgentOptions extends ViewerAuthOptions {
  /** Project UUID that owns the test agent. */
  projectId: string;
  /** Agent slug/name (default: unique `e2e-agent-<suffix>`). */
  name?: string;
  /** Stock role tier determining default JWT scopes (default: `'baseline'`). */
  role?: TestAgentRole;
  /** Initial agent `messageMode` (default on hub: `'project'`). */
  messageMode?: TestAgentMessageMode;
  /** Explicit JWT scopes override (defaults to `ROLE_SCOPES[role]`). */
  scopes?: readonly string[];
  /** Optional delegation ancestry chain (`[rootUserId, ...]`). */
  ancestry?: readonly string[];
  /**
   * Shared signing secret passed to the hub (`--session-secret` or
   * `SCION_SERVER_SESSION_SECRET`). Defaults to `E2E_SESSION_SECRET`.
   */
  signingSecret?: string;
  /** Token lifetime in seconds (default: 3600). */
  tokenTtlSeconds?: number;
  /**
   * Transition the provisioned agent to `phase: "running", activity: "idle"`
   * via `POST /api/v1/agents/{id}/status` using its own `X-Scion-Agent-Token`.
   *
   * Default `true`: validates the minted token immediately at setup time and
   * ensures chat v2 treats the agent as reachable (`!isAgentUnreachable`), so
   * answers that deliver to the agent attempt broker dispatch while
   * confirmation-only answers stay logged without delivery.
   */
  markRunning?: boolean;
}

export type ChoiceOptionInput =
  | string
  | {
      id?: string;
      label: string;
      description?: string;
    };

export interface DecisionPromptInput {
  /** Optional decision key (when omitted, the hub assigns `D1`, `D2`, ...). */
  id?: string;
  /** Markdown question prompt. */
  question: string;
  /** Choice options (2 to 5 options). */
  choices: ChoiceOptionInput[];
  /** Optional recommended choice id or label. */
  default?: string;
}

export interface AgentRecipientOptions {
  /** Explicit recipient address, e.g. `'user:alice@e2e.test'` or `'user:<uuid>'`. */
  recipient?: string;
  /** Explicit recipient user UUID. */
  recipientId?: string;
  /**
   * Conversation UUID. When set without `recipient`/`recipientId`/`conversationRef`,
   * normalized to `conversation_ref: 'conv:<uuid>'` as required by
   * `POST /api/v1/agents/{id}/outbound-message`.
   */
  conversationId?: string;
  /**
   * Human-readable or canonical conversation reference
   * (`'conv:<uuid>'`, `'#<thread-name>'`, `'@<email>'`).
   */
  conversationRef?: string;
  /** Optional thread ID (for example a topic UUID or matching `dm:...` key). */
  threadId?: string;
  /** Optional channel name (e.g. `'web'`). */
  channel?: string;
  /** Optional non-reserved message metadata. */
  metadata?: Record<string, string>;
}

export interface SendAgentMessageOptions extends AgentRecipientOptions {
  /** Message body text. */
  msg: string;
  /** Optional message type (defaults to `'input-needed'` on the hub). */
  type?: string;
  /** Optional urgent flag. */
  urgent?: boolean;
}

export interface SendChoicePromptOptions extends AgentRecipientOptions {
  /** Optional intro text rendered above the choice card. */
  msg?: string;
  /** One decision (single mode) or 2+ decisions (set mode). */
  choices: DecisionPromptInput[];
  /** Allow replacing existing decision keys with a new version (`--update`). */
  choicesUpdate?: boolean;
}

export interface ChoiceReplacedDecisionSummary {
  version: number;
  status: string;
  answer?: string;
  answered_by?: string;
}

export interface ChoiceDecisionSendResult {
  key: string;
  version: number;
  ref: string;
  row_id: string;
  replaced: ChoiceReplacedDecisionSummary | null;
}

export interface AgentOutboundSendResponse {
  message_id: string;
  status: string;
  recipient: string;
  recipient_id: string;
  conversation_id?: string;
  decisions?: ChoiceDecisionSendResult[];
}

export interface AgentRequestResult<T = unknown> {
  status: number;
  ok: boolean;
  headers: Headers;
  text: string;
  body: T;
}

/**
 * How the hub handled agent delivery for a stored `choice-response` message:
 *   - `'attempted_delivery'`: the hub called `deliverChoiceResponse` /
 *     `deliverChoiceSetResponse` to the containerless runtime broker, which
 *     rejected/failed container delivery because no container is running.
 *   - `'logged_without_delivery'`: the hub persisted the `choice-response`
 *     message with `dispatchState: "dispatched"` and skipped broker delivery
 *     (for example `outcome == "confirm"` on a matching interpreted answer).
 *   - `'agent_unreachable'`: the hub skipped broker delivery up front because
 *     the asking agent was stopped, suspended, errored, or deleted
 *     (`dispatchFailureReason` starts with `"Agent unreachable"`).
 */
export type ChoiceResponseDeliveryClassification =
  | 'attempted_delivery'
  | 'logged_without_delivery'
  | 'agent_unreachable';

export interface ChoiceResponseInspection {
  messageId: string;
  sender: string;
  senderId: string;
  recipient: string;
  recipientId: string;
  content: string;
  createdAt: string;
  dispatchState: string;
  dispatchFailureReason: string;
  response?: ChoiceResponseView;
  responseSet?: ChoiceResponseSetView;
  deliveryClassification: ChoiceResponseDeliveryClassification;
  attemptedAgentDelivery: boolean;
}

export interface WaitForChoiceResponseSelector {
  /** Exact `choice-response` message UUID, when known. */
  responseMessageId?: string;
  /** Originating choice-prompt message UUID (`sendRes.message_id`). */
  promptMessageId?: string;
}

export interface AgentSender {
  readonly id: string;
  readonly slug: string;
  readonly name: string;
  readonly projectId: string;
  readonly token: string;
  readonly baseURL: string;

  /**
   * Raw authenticated `fetch` against the test hub with `X-Scion-Agent-Token`.
   * Works on both `/api/v1/agents/{id}/*` (`RouteAgentSelf`) and
   * `/api/v1/agent/*` (`RouteAgentToken`).
   */
  request<T = unknown>(apiPath: string, init?: RequestInit): Promise<AgentRequestResult<T>>;

  /** Updates the agent's self-reported lifecycle status via `POST /api/v1/agents/{id}/status`. */
  setStatus(status: { phase?: string; activity?: string; message?: string }): Promise<void>;

  /** Sends an outbound chat message via `POST /api/v1/agents/{id}/outbound-message`. */
  sendMessage(options: SendAgentMessageOptions): Promise<AgentOutboundSendResponse>;

  /** Sends a choice prompt via `POST /api/v1/agents/{id}/outbound-message`. */
  sendChoicePrompt(options: SendChoicePromptOptions): Promise<AgentOutboundSendResponse>;

  /**
   * Lists `choice-response` rows in `conversationKey` (`topicId` or `dm:...`)
   * from `GET /api/v1/chat/conversations/{conversationKey}/messages` and
   * classifies each row's agent-delivery behavior.
   */
  inspectChoiceResponses(
    conversationKey: string,
    viewerAuth?: ViewerAuthOptions
  ): Promise<ChoiceResponseInspection[]>;

  /**
   * Polls `inspectChoiceResponses` until the selected `choice-response` row
   * settles into a non-buffered delivery outcome (`'attempted_delivery'` or
   * `'agent_unreachable'`). Pass either a `responseMessageId` string or
   * `{ responseMessageId?, promptMessageId? }`.
   *
   * On hosts without a running Docker daemon, the co-located runtime broker
   * flushes its 2s message buffer and reports the containerless delivery
   * failure back to the hub within ~3s; on hosts with Docker, the broker
   * returns 404 immediately. Once a later response to the same agent settles
   * here, any earlier response to that agent is guaranteed to have flushed as
   * well, allowing sleep-free assertions that an earlier confirmation stayed
   * `'logged_without_delivery'`.
   */
  waitForChoiceResponseDelivery(
    conversationKey: string,
    selector: string | WaitForChoiceResponseSelector,
    viewerAuth?: ViewerAuthOptions,
    timeoutMs?: number
  ): Promise<ChoiceResponseInspection>;
}

interface RawHistoryMessage {
  id: string;
  sender?: string;
  senderId?: string;
  recipient?: string;
  recipientId?: string;
  msg?: string;
  content?: string;
  type?: string;
  createdAt?: string;
  timestamp?: string;
  dispatchState?: string;
  dispatchFailureReason?: string;
}

interface RawChatHistoryResponse {
  messages?: RawHistoryMessage[];
  messageChoiceResponses?: Record<string, ChoiceResponseView>;
  messageChoiceResponseSets?: Record<string, ChoiceResponseSetView>;
}

/**
 * Derives the 32-byte HS256 `agent_signing_key` from the hub's shared signing
 * secret (`deriveSharedSigningKey(secret, "agent_signing_key")` in `pkg/hub/server.go`).
 */
export function deriveAgentSigningKey(signingSecret: string = E2E_SESSION_SECRET): Buffer {
  return crypto
    .createHash('sha256')
    .update(`scion-hub-signing-key:agent_signing_key:${signingSecret}`)
    .digest();
}

export interface MintAgentTokenOptions {
  agentId: string;
  projectId: string;
  role?: TestAgentRole;
  scopes?: readonly string[];
  ancestry?: readonly string[];
  signingSecret?: string;
  ttlSeconds?: number;
}

/**
 * Signs an HS256 agent JWT matching `AgentTokenService.SignAgentToken` in
 * `pkg/hub/agenttoken.go`.
 */
export function mintAgentToken(options: MintAgentTokenOptions): string {
  const nowSec = Math.floor(Date.now() / 1000);
  const ttl = options.ttlSeconds ?? 3600;
  const role = options.role ?? 'baseline';
  const scopes = options.scopes ? [...options.scopes] : [...ROLE_SCOPES[role]];

  const header = { alg: 'HS256', typ: 'JWT' };
  const payload: Record<string, unknown> = {
    iss: AGENT_TOKEN_ISSUER,
    sub: options.agentId,
    aud: [AGENT_TOKEN_AUDIENCE],
    iat: nowSec,
    nbf: nowSec,
    exp: nowSec + ttl,
    jti: crypto.randomUUID(),
    project_id: options.projectId,
    scopes,
    scope_schema: CURRENT_AGENT_SCOPE_SCHEMA,
  };
  if (options.ancestry && options.ancestry.length > 0) {
    payload.ancestry = [...options.ancestry];
  }

  const encodedHeader = Buffer.from(JSON.stringify(header)).toString('base64url');
  const encodedPayload = Buffer.from(JSON.stringify(payload)).toString('base64url');
  const signingInput = `${encodedHeader}.${encodedPayload}`;
  const key = deriveAgentSigningKey(options.signingSecret ?? E2E_SESSION_SECRET);
  const signature = crypto.createHmac('sha256', key).update(signingInput).digest('base64url');
  return `${signingInput}.${signature}`;
}

function buildViewerHeaders(
  hub: Pick<HubState, 'devToken'>,
  viewerAuth?: ViewerAuthOptions,
  hasJsonBody = false
): Record<string, string> {
  const headers: Record<string, string> = {};
  if (hasJsonBody) {
    headers['Content-Type'] = 'application/json';
  }
  if (viewerAuth?.cookie) {
    headers.Cookie = viewerAuth.cookie;
  } else {
    const bearer = viewerAuth?.authToken ?? hub.devToken;
    if (!bearer) {
      throw new Error(
        'agent fixture: viewer credential required (pass authToken or cookie, or use a hub with devToken)'
      );
    }
    headers.Authorization = `Bearer ${bearer}`;
  }
  return headers;
}

function parseBody<T>(text: string): T {
  if (!text) return null as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    return text as T;
  }
}

function buildOutboundAddressing(options: AgentRecipientOptions): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (options.recipient) out.recipient = options.recipient;
  if (options.recipientId) out.recipient_id = options.recipientId;
  if (options.threadId) out.thread_id = options.threadId;
  if (options.channel) out.channel = options.channel;
  if (options.metadata) out.metadata = options.metadata;

  if (options.conversationRef) {
    out.conversation_ref = options.conversationRef;
  } else if (options.conversationId) {
    if (options.recipient || options.recipientId) {
      out.conversation_id = options.conversationId;
    } else {
      out.conversation_ref = options.conversationId.startsWith('conv:')
        ? options.conversationId
        : `conv:${options.conversationId}`;
    }
  }
  return out;
}

/** Canonical DM conversation key between an agent and a user (`dm:agent:<agentId>:user:<userId>`). */
export function canonicalAgentUserDMKey(agentId: string, userId: string): string {
  return `dm:agent:${agentId}:user:${userId}`;
}

/** Classifies a stored `choice-response` message's dispatch state and failure reason. */
export function classifyChoiceResponseDelivery(
  dispatchState: string | undefined,
  dispatchFailureReason: string | undefined
): ChoiceResponseDeliveryClassification {
  const state = dispatchState ?? '';
  const reason = dispatchFailureReason ?? '';
  if (state === 'failed') {
    if (reason.startsWith('Agent unreachable')) {
      return 'agent_unreachable';
    }
    return 'attempted_delivery';
  }
  return 'logged_without_delivery';
}

/**
 * Provisions a real containerless agent on `hub` (`provisionOnly: true`) and
 * returns an `AgentSender` authenticated via `X-Scion-Agent-Token`.
 */
export async function createTestAgent(
  hub: Pick<HubState, 'baseURL' | 'devToken'>,
  options: CreateTestAgentOptions
): Promise<AgentSender> {
  const suffix = crypto.randomBytes(4).toString('hex');
  const requestedName = options.name ?? `e2e-agent-${suffix}`;
  const role = options.role ?? 'baseline';

  const createRes = await fetch(
    `${hub.baseURL}/api/v1/projects/${encodeURIComponent(options.projectId)}/agents`,
    {
      method: 'POST',
      headers: buildViewerHeaders(hub, options, true),
      body: JSON.stringify({
        name: requestedName,
        provisionOnly: true,
        agentRole: role,
        ...(options.messageMode ? { messageMode: options.messageMode } : {}),
      }),
      signal: AbortSignal.timeout(DEFAULT_REQUEST_TIMEOUT_MS),
    }
  );
  const createText = await createRes.text();
  if (createRes.status !== 201 && createRes.status !== 200) {
    throw new Error(
      `createTestAgent: POST /api/v1/projects/${options.projectId}/agents answered ${createRes.status}: ${createText}`
    );
  }
  const parsed = parseBody<{
    agent?: { id: string; slug: string; name: string; projectId?: string };
    id?: string;
    slug?: string;
    name?: string;
    projectId?: string;
  }>(createText);
  const agentRecord = parsed.agent ?? parsed;
  if (!agentRecord?.id || !agentRecord?.slug) {
    throw new Error(
      `createTestAgent: response missing agent id/slug (${createRes.status}): ${createText}`
    );
  }

  const id = agentRecord.id;
  const slug = agentRecord.slug;
  const name = agentRecord.name || slug;
  const projectId = agentRecord.projectId || options.projectId;

  const token = mintAgentToken({
    agentId: id,
    projectId,
    role,
    ...(options.scopes !== undefined ? { scopes: options.scopes } : {}),
    ...(options.ancestry !== undefined ? { ancestry: options.ancestry } : {}),
    ...(options.signingSecret !== undefined ? { signingSecret: options.signingSecret } : {}),
    ...(options.tokenTtlSeconds !== undefined ? { ttlSeconds: options.tokenTtlSeconds } : {}),
  });

  const request = async <T = unknown>(
    apiPath: string,
    init: RequestInit = {}
  ): Promise<AgentRequestResult<T>> => {
    const normalizedPath = apiPath.startsWith('/') ? apiPath : `/${apiPath}`;
    const headers = new Headers(init.headers);
    if (!headers.has('X-Scion-Agent-Token')) {
      headers.set('X-Scion-Agent-Token', token);
    }
    if (init.body !== undefined && !headers.has('Content-Type')) {
      headers.set('Content-Type', 'application/json');
    }
    const res = await fetch(`${hub.baseURL}${normalizedPath}`, {
      ...init,
      headers,
      signal: init.signal ?? AbortSignal.timeout(DEFAULT_REQUEST_TIMEOUT_MS),
    });
    const text = await res.text();
    return {
      status: res.status,
      ok: res.ok,
      headers: res.headers,
      text,
      body: parseBody<T>(text),
    };
  };

  const setStatus = async (status: {
    phase?: string;
    activity?: string;
    message?: string;
  }): Promise<void> => {
    const res = await request<{ applied?: boolean; reason?: string }>(
      `/api/v1/agents/${encodeURIComponent(id)}/status`,
      {
        method: 'POST',
        body: JSON.stringify(status),
      }
    );
    if (!res.ok) {
      const hint =
        res.status === 401
          ? ' (ensure startHub ran with testLogin: true or SCION_SERVER_SESSION_SECRET matching signingSecret)'
          : '';
      throw new Error(`agent.setStatus(${id}) answered ${res.status}${hint}: ${res.text}`);
    }
  };

  if (options.markRunning ?? true) {
    await setStatus({ phase: 'running', activity: 'idle' });
  }

  const sendOutbound = async (
    label: string,
    payload: Record<string, unknown>
  ): Promise<AgentOutboundSendResponse> => {
    const res = await request<AgentOutboundSendResponse>(
      `/api/v1/agents/${encodeURIComponent(id)}/outbound-message`,
      {
        method: 'POST',
        body: JSON.stringify(payload),
      }
    );
    if (!res.ok) {
      throw new Error(`${label} answered ${res.status}: ${res.text}`);
    }
    return res.body;
  };

  const sendMessage = async (
    sendOpts: SendAgentMessageOptions
  ): Promise<AgentOutboundSendResponse> => {
    const payload: Record<string, unknown> = {
      ...buildOutboundAddressing(sendOpts),
      msg: sendOpts.msg,
      ...(sendOpts.type ? { type: sendOpts.type } : {}),
      ...(sendOpts.urgent !== undefined ? { urgent: sendOpts.urgent } : {}),
    };
    return sendOutbound('agent.sendMessage', payload);
  };

  const sendChoicePrompt = async (
    choiceOpts: SendChoicePromptOptions
  ): Promise<AgentOutboundSendResponse> => {
    const payload: Record<string, unknown> = {
      ...buildOutboundAddressing(choiceOpts),
      msg: choiceOpts.msg ?? '',
      choices: choiceOpts.choices,
      ...(choiceOpts.choicesUpdate ? { choices_update: true } : {}),
    };
    return sendOutbound('agent.sendChoicePrompt', payload);
  };

  const inspectChoiceResponses = async (
    conversationKey: string,
    viewerAuth?: ViewerAuthOptions
  ): Promise<ChoiceResponseInspection[]> => {
    const res = await fetch(
      `${hub.baseURL}/api/v1/chat/conversations/${encodeURIComponent(conversationKey)}/messages?limit=100`,
      {
        headers: buildViewerHeaders(hub, viewerAuth ?? options),
        signal: AbortSignal.timeout(DEFAULT_REQUEST_TIMEOUT_MS),
      }
    );
    const text = await res.text();
    if (!res.ok) {
      throw new Error(
        `agent.inspectChoiceResponses(${conversationKey}) answered ${res.status}: ${text}`
      );
    }
    const history = parseBody<RawChatHistoryResponse>(text);
    const messages = history.messages ?? [];
    const singleViews = history.messageChoiceResponses ?? {};
    const setViews = history.messageChoiceResponseSets ?? {};

    return messages
      .filter((m) => m.type === 'choice-response')
      .map((m) => {
        const dispatchState = m.dispatchState ?? '';
        const dispatchFailureReason = m.dispatchFailureReason ?? '';
        const deliveryClassification = classifyChoiceResponseDelivery(
          dispatchState,
          dispatchFailureReason
        );
        return {
          messageId: m.id,
          sender: m.sender ?? '',
          senderId: m.senderId ?? '',
          recipient: m.recipient ?? '',
          recipientId: m.recipientId ?? '',
          content: m.msg ?? m.content ?? '',
          createdAt: m.createdAt ?? m.timestamp ?? '',
          dispatchState,
          dispatchFailureReason,
          ...(singleViews[m.id] ? { response: singleViews[m.id] } : {}),
          ...(setViews[m.id] ? { responseSet: setViews[m.id] } : {}),
          deliveryClassification,
          attemptedAgentDelivery: deliveryClassification === 'attempted_delivery',
        };
      });
  };

  const waitForChoiceResponseDelivery = async (
    conversationKey: string,
    selector: string | WaitForChoiceResponseSelector,
    viewerAuth?: ViewerAuthOptions,
    timeoutMs: number = DEFAULT_DELIVERY_SETTLE_TIMEOUT_MS
  ): Promise<ChoiceResponseInspection> => {
    const normalized: WaitForChoiceResponseSelector =
      typeof selector === 'string' ? { responseMessageId: selector } : selector;
    if (!normalized.responseMessageId && !normalized.promptMessageId) {
      throw new Error(
        'waitForChoiceResponseDelivery: provide responseMessageId or promptMessageId'
      );
    }
    const label = normalized.responseMessageId ?? `prompt:${normalized.promptMessageId}`;
    const matchesSelector = (item: ChoiceResponseInspection): boolean => {
      if (normalized.responseMessageId && item.messageId !== normalized.responseMessageId) {
        return false;
      }
      if (normalized.promptMessageId) {
        const promptId = item.response?.promptMessageId ?? item.responseSet?.promptMessageId;
        if (promptId !== normalized.promptMessageId) return false;
      }
      return true;
    };

    const deadline = Date.now() + timeoutMs;
    let lastSeen: ChoiceResponseInspection | undefined;
    while (Date.now() <= deadline) {
      const items = await inspectChoiceResponses(conversationKey, viewerAuth);
      const match = items.find(matchesSelector);
      if (match) {
        lastSeen = match;
        if (match.deliveryClassification !== 'logged_without_delivery') {
          return match;
        }
      }
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    throw new Error(
      `waitForChoiceResponseDelivery(${label}) did not observe a settled delivery attempt within ${timeoutMs}ms` +
        (lastSeen ? ` (last state: ${JSON.stringify(lastSeen)})` : ' (message not found)')
    );
  };

  return {
    id,
    slug,
    name,
    projectId,
    token,
    baseURL: hub.baseURL,
    request,
    setStatus,
    sendMessage,
    sendChoicePrompt,
    inspectChoiceResponses,
    waitForChoiceResponseDelivery,
  };
}
