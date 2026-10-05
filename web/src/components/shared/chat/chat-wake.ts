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

/** Client-only dispatch state shown on the optimistic bubble while waking. */
export const WAKING_DISPATCH_STATE = 'waking';

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
