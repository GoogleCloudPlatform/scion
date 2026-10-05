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
 * Wake-on-send for chat v2.
 *
 * A send carries `offer_wake: true`. When the primary recipient is suspended
 * and the caller may wake it, the hub answers 409 `agent_not_running` with
 * `details.canWake === true` and persists nothing. The thread then asks the
 * user whether to wake the agent and, if confirmed, resends the same message
 * with `wake: true`; the hub resumes the agent, waits for it to be ready and
 * delivers the message as its first input. When the caller may not wake the
 * agent (or it is stopped, errored or deleted) the hub keeps the ordinary
 * failed "Agent unreachable" row, so no wake is ever offered.
 */

import { showConfirm } from '../confirm-dialog.js';
import { chatDraftStorageKey } from '../../../client/chat-drafts.js';

/** Client-only dispatch state shown on the optimistic bubble while waking. */
export const WAKING_DISPATCH_STATE = 'waking';

// The hub's wake budget (handlers_chat_v2.go chatWakeWriteBudget): a 90s
// resume, then 30s per recipient (primary plus each @mention), plus 30s.
const HUB_WAKE_RESUME_MS = 90_000;
const HUB_DELIVERY_PER_RECIPIENT_MS = 30_000;
const HUB_WAKE_SLACK_MS = 30_000;
/** Extra time past the hub's budget before giving up on confirmation. */
const WAKE_CONFIRM_MARGIN_MS = 30_000;
/**
 * Upper bound for confirmation: the hub forgets idempotency keys after 5
 * minutes, after which a retry would no longer be recognised and could send
 * again, so confirmation must end safely before that.
 */
export const WAKE_CONFIRM_MAX_MS = 5 * 60_000 - 30_000;

/**
 * How long a wake send to `recipients` agents (primary plus @mentions)
 * keeps confirming its outcome after a dropped connection: the hub's wake
 * budget for that many recipients plus a margin, capped below the hub's
 * idempotency TTL.
 *
 * Both limits fail safe. The caller counts the composer's accepted
 * mentions, while the hub resolves recipients from the content, so typed
 * mentions can undercount; and from 5 recipients up the cap is at or below
 * the hub's budget. Either way confirmation may give up early and report
 * an unknown outcome while the hub is still working; it never causes a
 * duplicate, since giving up sends nothing.
 */
export function wakeConfirmBudgetMs(recipients: number): number {
  const n = Math.max(1, Math.floor(recipients));
  const hubBudget = HUB_WAKE_RESUME_MS + n * HUB_DELIVERY_PER_RECIPIENT_MS + HUB_WAKE_SLACK_MS;
  return Math.min(hubBudget + WAKE_CONFIRM_MARGIN_MS, WAKE_CONFIRM_MAX_MS);
}

/** Delay between wake-send confirmation retries. */
export const WAKE_RETRY_DELAY_MS = 3_000;

/** Shown when a wake send's outcome could not be confirmed. */
export const WAKE_OUTCOME_UNKNOWN_MESSAGE =
  'Could not confirm whether the message was delivered. Check the conversation before sending it again.';

/** The agent a send offered to wake. */
export interface WakeOffer {
  agentId: string;
  agentSlug: string;
}

/**
 * Reads a wake offer from a parsed error response body. Returns null unless
 * the hub explicitly said the caller may wake the agent: a permission gate
 * fails closed.
 */
export function wakeOfferFromErrorBody(data: unknown): WakeOffer | null {
  if (!data || typeof data !== 'object') return null;
  const err = (data as { error?: unknown }).error;
  if (!err || typeof err !== 'object') return null;
  const { code, details } = err as { code?: unknown; details?: unknown };
  if (code !== 'agent_not_running' || !details || typeof details !== 'object') return null;
  const d = details as Record<string, unknown>;
  if (d.canWake !== true) return null;
  return {
    agentId: typeof d.agentId === 'string' ? d.agentId : '',
    agentSlug: typeof d.agentSlug === 'string' ? d.agentSlug : '',
  };
}

/** The error message of a parsed error response body, or the fallback. */
export function errorMessageFromBody(data: unknown, fallback: string): string {
  if (data && typeof data === 'object') {
    const err = (data as { error?: unknown }).error;
    if (err && typeof err === 'object') {
      const message = (err as { message?: unknown }).message;
      if (typeof message === 'string' && message) return message;
    }
    if (typeof err === 'string' && err) return err;
  }
  return fallback;
}

/** Body text of the wake confirmation dialog. */
export function wakeConfirmMessage(offer: WakeOffer): string {
  const name = offer.agentSlug ? `@${offer.agentSlug}` : 'This agent';
  return `${name} is suspended. Wake it and send this message as its first input?`;
}

/**
 * Asks the user whether to wake the agent. Resolves true for "Wake and
 * send", false for Cancel (or Escape).
 */
export function confirmWake(offer: WakeOffer): Promise<boolean> {
  return showConfirm(wakeConfirmMessage(offer), {
    title: 'Agent is suspended',
    confirmText: 'Wake and send',
    cancelText: 'Cancel',
    variant: 'primary',
  });
}

/**
 * Saves a draft that could not go back into the composer because the user
 * switched conversations meanwhile: it becomes the draft of the conversation
 * it was written in, unless that conversation already has another draft.
 * Returns whether it was saved.
 */
export function saveDraftForConversation(conversationKey: string, text: string): boolean {
  if (!conversationKey || !text) return false;
  try {
    const key = chatDraftStorageKey(conversationKey);
    if (localStorage.getItem(key)) return false;
    localStorage.setItem(key, text);
    return true;
  } catch {
    // localStorage may throw in private browsing mode.
    return false;
  }
}

/** Gateway statuses a proxy answers when the hub connection drops. */
const GATEWAY_STATUSES = new Set([502, 503, 504]);

/**
 * Whether a response with this status and parsed body is a gateway drop
 * (no hub error) rather than the hub's own answer. The hub also uses
 * 502/503 for real failures, and those are answers that must not be
 * retried: API errors carry a JSON `error.code` (a wake that failed,
 * dispatch not available), and maintenance mode answers 503 with a
 * top-level string `error` ("system_maintenance").
 */
export function isGatewayDrop(status: number, data: unknown): boolean {
  if (!GATEWAY_STATUSES.has(status)) return false;
  if (!data || typeof data !== 'object') return true;
  const err = (data as { error?: unknown }).error;
  if (typeof err === 'string' && err) return false;
  return !(err && typeof err === 'object' && typeof (err as { code?: unknown }).code === 'string');
}

/** Thrown when a wake send's outcome cannot be confirmed. */
export class WakeOutcomeUnknownError extends Error {
  constructor() {
    super(WAKE_OUTCOME_UNKNOWN_MESSAGE);
    this.name = 'WakeOutcomeUnknownError';
  }
}

/**
 * Whether an error answer to a wake-send retry is about this send, i.e.
 * the hub got as far as the send's idempotency key. Answers the hub gives
 * before that say nothing about an earlier attempt that may have reached
 * it: maintenance mode (503 with a top-level string `error`), a rate limit
 * (429) and re-authentication (401).
 */
export function isAnswerAboutThisSend(status: number, data: unknown): boolean {
  if (status === 401 || status === 429) return false;
  if (data && typeof data === 'object') {
    const err = (data as { error?: unknown }).error;
    if (typeof err === 'string' && err) return false;
  }
  return true;
}

/** Whether a parsed 409 body says a send with the same key is still running. */
export function isSendInProgressBody(data: unknown): boolean {
  if (!data || typeof data !== 'object') return false;
  const err = (data as { error?: unknown }).error;
  return (
    !!err && typeof err === 'object' && (err as { code?: unknown }).code === 'send_in_progress'
  );
}

/** Rebuilds a JSON response whose body was already read. */
export function jsonResponse(data: unknown, status: number): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}
