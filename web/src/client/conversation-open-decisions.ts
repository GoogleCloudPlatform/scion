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
 * Open decisions in a single chat conversation (ptone/scion#8594):
 * API client for `GET /api/v1/chat/conversations/{key}/decisions`.
 */

import { apiFetch, extractApiError } from './api.js';
import type { DecisionAnswerMode } from '../shared/types.js';

/** Per-viewer prompt counts for a single conversation (prompts, not rows). */
export interface ConversationDecisionCounts {
  waitingOnYou: number;
  waitingOnOthers: number;
}

/** One open decision prompt returned by the conversation decisions endpoint. */
export interface ConversationOpenPromptItem {
  promptMessageId: string;
  promptAvailable: boolean;
  createdAt: string;
  askerAgentId: string;
  askerAgentName: string;
  answerMode: DecisionAnswerMode;
  openCount: number;
  totalCount: number;
  question: string;
  waitingOnYou: boolean;
  awaitingConfirmation: boolean;
  disabledReason?: string;
}

/** Response of `GET /api/v1/chat/conversations/{key}/decisions`. */
export interface ConversationOpenDecisionsResponse {
  counts: ConversationDecisionCounts;
  prompts: ConversationOpenPromptItem[];
  truncated: boolean;
}

/** Thrown by `fetchConversationOpenDecisions` with HTTP status. */
export class ConversationDecisionsError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
    this.name = 'ConversationDecisionsError';
  }
}

/**
 * Fetches open decision prompts for `conversationKey`.
 * Returns `null` on 404 (experiment off, older hub, or conversation no longer
 * readable) so the toolbar component can hide quietly.
 */
export async function fetchConversationOpenDecisions(
  conversationKey: string,
  signal?: AbortSignal
): Promise<ConversationOpenDecisionsResponse | null> {
  const path = `/api/v1/chat/conversations/${encodeURIComponent(conversationKey)}/decisions`;
  const res = await apiFetch(path, signal ? { signal } : undefined);
  if (res.status === 404) return null;
  if (!res.ok) {
    throw new ConversationDecisionsError(
      await extractApiError(res, 'Could not load open decisions'),
      res.status
    );
  }
  const body = (await res.json()) as Partial<ConversationOpenDecisionsResponse> | null;
  return {
    counts: {
      waitingOnYou: body?.counts?.waitingOnYou ?? 0,
      waitingOnOthers: body?.counts?.waitingOnOthers ?? 0,
    },
    prompts: Array.isArray(body?.prompts) ? body.prompts : [],
    truncated: Boolean(body?.truncated),
  };
}
