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
 * Choices web cards (ptone/scion#7673): the experiment gate, the answer
 * endpoint, and the merge rules that keep a card's decision views current
 * across history pages, live chat messages and `chat.choice.updated`
 * events.
 */

import { apiFetch, apiErrorMessageFromBody } from './api.js';
import { isFeatureEnabled } from '../utils/feature-flags.js';
import { getDenialMessage } from '../shared/message-mode.js';
import type {
  Agent,
  ChoiceAnswerMode,
  ChoiceResponseSetView,
  ChoiceResponseView,
  DecisionView,
  Message,
} from '../shared/types.js';

/** The experiment gating choice cards (web and hub). */
export const CHAT_CHOICES_EXPERIMENT = 'web.chat_choices';

/** Whether choice cards are shown in this browser session. */
export function chatChoicesEnabled(): boolean {
  return isFeatureEnabled(CHAT_CHOICES_EXPERIMENT);
}

/** A decision whose state can no longer change back to pending. */
export function isTerminalDecision(view: DecisionView): boolean {
  return view.status === 'decided' || view.status === 'replaced';
}

/**
 * How far a decision has gone: pending < decided < replaced. The hub never
 * moves a row back, and replaced is final (a row answered just before an
 * `--update` is then replaced).
 */
function statusRank(view: DecisionView): number {
  if (view.status === 'replaced') return 2;
  if (view.status === 'decided') return 1;
  return 0;
}

/**
 * The view to hold for a decision row once `incoming` arrives: the incoming
 * one, unless it is behind the held one in pending < decided < replaced
 * (an out-of-order event, a page fetched before the change, or the
 * viewer's own POST answering just before a replacement arrived).
 */
export function newerDecisionView(held: DecisionView, incoming: DecisionView): DecisionView {
  if (statusRank(incoming) < statusRank(held)) return held;
  return incoming;
}

/**
 * `decisions` with each row named in `incoming` replaced under the
 * out-of-order guard. Returns the same array when nothing changed, so
 * callers can skip a re-render.
 */
export function patchDecisions(
  decisions: readonly DecisionView[],
  incoming: readonly DecisionView[]
): DecisionView[] {
  const byRow = new Map(incoming.map((d) => [d.rowId, d]));
  let changed = false;
  const next = decisions.map((held) => {
    const update = byRow.get(held.rowId);
    if (!update) return held;
    const chosen = newerDecisionView(held, update);
    if (chosen !== held) changed = true;
    return chosen;
  });
  return changed ? next : (decisions as DecisionView[]);
}

/** The choice side maps of a chat history response. */
export interface ChoiceHistoryMaps {
  messageDecisions?: Record<string, DecisionView[]>;
  messageChoiceIntros?: Record<string, string>;
  messageChoiceAnswerModes?: Record<string, ChoiceAnswerMode>;
  messageChoiceResponses?: Record<string, ChoiceResponseView>;
  messageChoiceResponseSets?: Record<string, ChoiceResponseSetView>;
}

/**
 * Copies the choice side maps of a history response onto its messages.
 * The hub omits the maps while web.chat_choices is off, which leaves the
 * messages untouched.
 */
export function applyChoiceHistoryMaps(items: Message[], data: ChoiceHistoryMaps): void {
  const decisions = data.messageDecisions;
  const intros = data.messageChoiceIntros;
  const modes = data.messageChoiceAnswerModes;
  const responses = data.messageChoiceResponses;
  const responseSets = data.messageChoiceResponseSets;
  if (!decisions && !intros && !modes && !responses && !responseSets) return;
  for (const m of items) {
    const d = decisions?.[m.id];
    if (d) m.decisions = d;
    const intro = intros?.[m.id];
    if (intro !== undefined) m.choiceIntro = intro;
    const mode = modes?.[m.id];
    if (mode) m.answerMode = mode;
    const r = responses?.[m.id];
    if (r) m.choiceResponse = r;
    const rs = responseSets?.[m.id];
    if (rs) m.choiceResponseSet = rs;
  }
}

/**
 * Keeps the choice fields of a message already shown when the same message
 * arrives again: the newer decision views per row (out-of-order guard), and
 * the held fields when the incoming copy carries none.
 */
export function carryChoiceFields(existing: Message | undefined, incoming: Message): void {
  if (!existing?.decisions) return;
  if (!incoming.decisions) {
    incoming.decisions = existing.decisions;
    if (incoming.choiceIntro === undefined && existing.choiceIntro !== undefined) {
      incoming.choiceIntro = existing.choiceIntro;
    }
    if (!incoming.answerMode && existing.answerMode) incoming.answerMode = existing.answerMode;
    return;
  }
  const held = new Map(existing.decisions.map((d) => [d.rowId, d]));
  incoming.decisions = incoming.decisions.map((d) => {
    const prior = held.get(d.rowId);
    return prior ? newerDecisionView(prior, d) : d;
  });
}

/** Detail of the `choice-answered` event a card emits after its own POST. */
export interface ChoiceAnsweredDetail {
  messageId: string;
  decisions: DecisionView[];
  responseMessageId?: string;
}

/**
 * Detail of the `choice-answer-denied` event a card emits when an answer
 * endpoint refused the viewer with 403 (design §4.5 step 3).
 */
export interface ChoiceAnswerDeniedDetail {
  messageId: string;
  askerAgentId: string;
  /** The error code: message_denied, chat_post_denied, or another. */
  code: string;
  /** details.reason, when the hub sent one. */
  reason?: string;
  /** What the card now shows. */
  answerability: AskerAnswerability;
}

/** The outcome of answering one decision. */
export type AnswerDecisionResult =
  | { ok: true; decision: DecisionView; responseMessageId: string }
  | {
      ok: false;
      status: number;
      code: string;
      message: string;
      /** The current view, on 409 replaced / already_answered. */
      decision?: DecisionView;
      /** The newer version, on 409 replaced. */
      replacedBy?: DecisionView;
      /** details.reason, on 403 message_denied / chat_post_denied. */
      reason?: string;
    };

/**
 * POST /api/v1/decisions/{rowId}/answer for a single-decision prompt. A
 * network failure resolves as status 0.
 */
export async function answerDecision(
  rowId: string,
  choiceId: string,
  idempotencyKey: string
): Promise<AnswerDecisionResult> {
  let res: Response;
  try {
    res = await apiFetch(`/api/v1/decisions/${encodeURIComponent(rowId)}/answer`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ choice_id: choiceId, idempotency_key: idempotencyKey }),
    });
  } catch {
    return { ok: false, status: 0, code: '', message: 'Network error: the answer was not sent.' };
  }
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  if (res.ok) {
    const ok = body as { decision?: DecisionView; responseMessageId?: string } | null;
    if (ok?.decision) {
      return { ok: true, decision: ok.decision, responseMessageId: ok.responseMessageId ?? '' };
    }
    return { ok: false, status: res.status, code: '', message: 'Unexpected answer response.' };
  }
  const err = (body as { error?: { code?: string; details?: Record<string, unknown> } } | null)
    ?.error;
  const details = err?.details ?? {};
  return {
    ok: false,
    status: res.status,
    code: typeof err?.code === 'string' ? err.code : '',
    message: apiErrorMessageFromBody(body) ?? `Failed to record the answer (${res.status}).`,
    ...(isDecisionView(details.decision) ? { decision: details.decision } : {}),
    ...(isDecisionView(details.replacedBy) ? { replacedBy: details.replacedBy } : {}),
    ...(typeof details.reason === 'string' ? { reason: details.reason } : {}),
  };
}

function isDecisionView(v: unknown): v is DecisionView {
  return (
    !!v &&
    typeof v === 'object' &&
    typeof (v as DecisionView).rowId === 'string' &&
    typeof (v as DecisionView).status === 'string'
  );
}

/** One answer of a set submit. */
export interface SetAnswer {
  rowId: string;
  choiceId: string;
}

/** The outcome of answering a set prompt. */
export type AnswerDecisionSetResult =
  | { ok: true; decisions: DecisionView[]; responseMessageId: string; batchId: string }
  | {
      ok: false;
      status: number;
      code: string;
      message: string;
      /** Every current view of the prompt, on 409 answer_set_changed. */
      decisions?: DecisionView[];
      /** Why each changed row differs, by row id, on 409 answer_set_changed. */
      reasons?: Record<string, string>;
      /** details.reason, on 403 message_denied / chat_post_denied. */
      reason?: string;
    };

/**
 * POST /api/v1/choice-prompts/{messageId}/answers for a set prompt
 * (design §4.3). A network failure resolves as status 0.
 */
export async function answerDecisionSet(
  messageId: string,
  answers: readonly SetAnswer[],
  idempotencyKey: string
): Promise<AnswerDecisionSetResult> {
  let res: Response;
  try {
    res = await apiFetch(`/api/v1/choice-prompts/${encodeURIComponent(messageId)}/answers`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        idempotency_key: idempotencyKey,
        answers: answers.map((a) => ({ decision_row_id: a.rowId, choice_id: a.choiceId })),
      }),
    });
  } catch {
    return { ok: false, status: 0, code: '', message: 'Network error: the answers were not sent.' };
  }
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  if (res.ok) {
    const ok = body as {
      decisions?: unknown;
      responseMessageId?: string;
      batchId?: string;
    } | null;
    const decisions = decisionViews(ok?.decisions);
    if (decisions) {
      return {
        ok: true,
        decisions,
        responseMessageId: ok?.responseMessageId ?? '',
        batchId: ok?.batchId ?? '',
      };
    }
    return { ok: false, status: res.status, code: '', message: 'Unexpected answer response.' };
  }
  const err = (body as { error?: { code?: string; details?: Record<string, unknown> } } | null)
    ?.error;
  const details = err?.details ?? {};
  const decisions = decisionViews(details.decisions);
  const reasons = stringMap(details.reasons);
  return {
    ok: false,
    status: res.status,
    code: typeof err?.code === 'string' ? err.code : '',
    message: apiErrorMessageFromBody(body) ?? `Failed to record the answers (${res.status}).`,
    ...(decisions ? { decisions } : {}),
    ...(reasons ? { reasons } : {}),
    ...(typeof details.reason === 'string' ? { reason: details.reason } : {}),
  };
}

function decisionViews(v: unknown): DecisionView[] | undefined {
  if (!Array.isArray(v)) return undefined;
  return v.every(isDecisionView) ? v : undefined;
}

function stringMap(v: unknown): Record<string, string> | undefined {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return undefined;
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
    if (typeof val === 'string') out[k] = val;
  }
  return out;
}

// ---------------------------------------------------------------------------
// Answerability (design §4.5): can the viewer answer a prompt of this asker?
// ---------------------------------------------------------------------------

/**
 * Whether the viewer can answer an asker's decisions, as a card shows it:
 * `blocked` disables every row (and a set's Submit) with `reason` shown;
 * `gone` stays answerable with a note that the answer is not delivered;
 * `unknown` (the lookup failed) stays answerable, the hub's 403 decides.
 */
export interface AskerAnswerability {
  state: 'ok' | 'blocked' | 'gone' | 'unknown';
  /** Why the viewer cannot answer (`blocked`), a full sentence. */
  reason?: string;
  /** Blocked by the asker's messaging rules: the reason leads with this name. */
  askerName?: string;
  /** Blocked because the viewer may read the thread but not post in it. */
  readOnly?: boolean;
}

/** Nothing known against answering. */
export const ANSWERABLE: AskerAnswerability = { state: 'ok' };

/** Text of a read-only thread's reason (design §4.5 step 1). */
export const READ_ONLY_ANSWER_REASON =
  "You can read this thread but can't post in it, so you can't answer.";

/** Text of a 403 that is neither message_denied nor chat_post_denied. */
export const GENERIC_ANSWER_DENIED_REASON = "You can't answer this decision.";

/** The answerability of a viewer who may read the thread but not post. */
export function readOnlyAnswerability(): AskerAnswerability {
  return { state: 'blocked', readOnly: true, reason: READ_ONLY_ANSWER_REASON };
}

/** Blocked by the asker's messaging rules (`reason` is a denial code). */
export function deniedAnswerability(
  reason: string | undefined,
  askerName: string
): AskerAnswerability {
  return {
    state: 'blocked',
    askerName,
    reason: getDenialMessage(reason, askerName),
  };
}

/**
 * The answerability a 403 from an answer endpoint means (design §4.5 step
 * 3, authoritative): message_denied names the asker's rule, chat_post_denied
 * is a read-only thread, any other 403 a generic refusal.
 */
export function answerabilityFromDenial(
  code: string,
  reason: string | undefined,
  askerName: string
): AskerAnswerability {
  if (code === 'message_denied') return deniedAnswerability(reason, askerName);
  if (code === 'chat_post_denied') return readOnlyAnswerability();
  return { state: 'blocked', reason: GENERIC_ANSWER_DENIED_REASON };
}

/**
 * Looks up whether the viewer can answer `askerAgentId` (design §4.5 step
 * 2) through the agent read, whose `_messageability` is computed by the
 * same rule the answer endpoints apply. 404 is a gone asker; a failed or
 * unexpected read is `unknown` (answerable; the 403 backstop decides).
 */
export async function fetchAskerAnswerability(
  askerAgentId: string,
  askerName: string
): Promise<AskerAnswerability> {
  let res: Response;
  try {
    res = await apiFetch(`/api/v1/agents/${encodeURIComponent(askerAgentId)}`);
  } catch {
    return { state: 'unknown' };
  }
  if (res.status === 404) return { state: 'gone' };
  if (!res.ok) return { state: 'unknown' };
  let agent: Agent | null;
  try {
    agent = (await res.json()) as Agent | null;
  } catch {
    return { state: 'unknown' };
  }
  const m = agent?._messageability;
  if (m && m.canMessage === false) {
    return deniedAnswerability(m.reason, askerName || agent?.name || 'This agent');
  }
  return ANSWERABLE;
}
