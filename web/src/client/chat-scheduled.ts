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
 * Scheduled send in native chat (ptone/scion#3666): the hub API for a
 * user's own scheduled messages in a conversation, and the experiment that
 * gates it. Only the sender ever sees a scheduled message.
 */

import { apiFetch, extractApiError } from './api.js';
import { isFeatureEnabled } from '../utils/feature-flags.js';

/** The experiment gating scheduled send (web and hub). */
export const CHAT_SCHEDULED_SEND_EXPERIMENT = 'web.chat_scheduled_send';

/** Whether scheduled send is offered in this browser session. */
export function scheduledSendEnabled(): boolean {
  return isFeatureEnabled(CHAT_SCHEDULED_SEND_EXPERIMENT);
}

/**
 * Whether a conversation supports scheduled send. Topics only for now;
 * direct messages are refused by the hub.
 */
export function conversationSupportsScheduledSend(conversationKey: string): boolean {
  return conversationKey !== '' && !conversationKey.startsWith('dm:');
}

export type ScheduledMessageStatus = 'pending' | 'sending' | 'sent' | 'cancelled' | 'failed';

/** A scheduled message as the hub returns it. Times are UTC ISO instants. */
export interface ScheduledMessage {
  id: string;
  conversationKey: string;
  content: string;
  replyToId?: string;
  fireAt: string;
  status: ScheduledMessageStatus;
  failureReason?: string;
  messageId?: string;
  createdAt: string;
  updatedAt: string;
}

/** The payload of a `user.<id>.chat.scheduled` SSE event. */
export interface ScheduledMessageEvent {
  action: 'created' | 'cancelled' | 'sent' | 'failed';
  scheduledMessage: ScheduledMessage;
}

/** User-facing text for a failure reason. */
export function scheduledFailureText(reason: string | undefined): string {
  switch (reason) {
    case 'no_access':
      return 'Not sent: you no longer have access to send it here.';
    case 'conversation_gone':
      return 'Not sent: this conversation no longer exists.';
    case 'sender_inactive':
      return 'Not sent: your account is not active.';
    case 'recipient_gone':
      return 'Not sent: the recipient no longer exists.';
    case 'missed':
      return 'Not sent: the scheduled time passed while the hub was unavailable.';
    case 'interrupted':
      return 'Delivery was interrupted. Check the thread before sending it again.';
    default:
      return 'Not sent: delivery failed.';
  }
}

function scheduledPath(conversationKey: string): string {
  return `/api/v1/chat/conversations/${encodeURIComponent(conversationKey)}/scheduled`;
}

/** Thrown by the API helpers with the hub's error message. */
export class ScheduledSendError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
    this.name = 'ScheduledSendError';
  }
}

export interface CreateScheduledMessageInput {
  content: string;
  /** UTC ISO instant. */
  fireAt: string;
  replyToId?: string;
  idempotencyKey: string;
}

/** Schedule a message. */
export async function createScheduledMessage(
  conversationKey: string,
  input: CreateScheduledMessageInput
): Promise<ScheduledMessage> {
  const body: Record<string, string> = {
    content: input.content,
    fire_at: input.fireAt,
    idempotency_key: input.idempotencyKey,
  };
  if (input.replyToId) body.reply_to_id = input.replyToId;
  const res = await apiFetch(scheduledPath(conversationKey), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    throw new ScheduledSendError(
      await extractApiError(res, 'Failed to schedule message'),
      res.status
    );
  }
  return (await res.json()) as ScheduledMessage;
}

/** The caller's pending, sending and failed messages in a conversation. */
export async function listScheduledMessages(conversationKey: string): Promise<ScheduledMessage[]> {
  const res = await apiFetch(scheduledPath(conversationKey));
  if (!res.ok) {
    throw new ScheduledSendError(
      await extractApiError(res, 'Failed to load scheduled messages'),
      res.status
    );
  }
  const body = (await res.json()) as { scheduledMessages?: ScheduledMessage[] };
  return body.scheduledMessages ?? [];
}

/** Cancel a pending scheduled message. */
export async function cancelScheduledMessage(conversationKey: string, id: string): Promise<void> {
  const res = await apiFetch(`${scheduledPath(conversationKey)}/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
  if (!res.ok) {
    const fallback =
      res.status === 409 ? 'This message is already being sent' : 'Failed to cancel message';
    throw new ScheduledSendError(await extractApiError(res, fallback), res.status);
  }
}

/**
 * Applies a scheduled-message change to a list shown for one conversation:
 * pending, sending and failed messages stay (updated in place or added),
 * sent and cancelled ones leave. The result is ordered by fire time.
 */
export function applyScheduledUpdate(
  list: readonly ScheduledMessage[],
  updated: ScheduledMessage
): ScheduledMessage[] {
  const rest = list.filter((m) => m.id !== updated.id);
  if (updated.status === 'sent' || updated.status === 'cancelled') return rest;
  return sortScheduled([...rest, updated]);
}

/** Orders scheduled messages by fire time, then creation time. */
export function sortScheduled(list: readonly ScheduledMessage[]): ScheduledMessage[] {
  return [...list].sort(
    (a, b) =>
      a.fireAt.localeCompare(b.fireAt) ||
      a.createdAt.localeCompare(b.createdAt) ||
      a.id.localeCompare(b.id)
  );
}
