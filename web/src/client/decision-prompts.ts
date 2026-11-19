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
 * Choices web cards (ptone/scion#7673): the answer endpoint and the merge
 * rules that keep a card's decision views current across history pages, live
 * chat messages and `chat.decision.updated` events.
 */

import { apiFetch, apiErrorMessageFromBody } from './api.js';
import { getDenialMessage } from '../shared/message-mode.js';
import type {
  Agent,
  DecisionAnswerMode,
  DecisionResponseSetView,
  DecisionResponseView,
  DecisionView,
  Message,
} from '../shared/types.js';

/** Web surface submitting a decision answer (`native` in chat, `decisions_page` on `/projects/:id/decisions`). */
export type DecisionAnswerSurface = 'native' | 'decisions_page';

/** Minimum version age before a decision may be dismissed (`store.DecisionDismissMinAge`, 5 minutes). */
export const DISMISS_MIN_AGE_MS = 5 * 60 * 1000;

/**
 * Whether `d` is still open (pending without an effective answer, or decided
 * only by an agent-interpreted answer awaiting confirmation). Unknown future
 * statuses return false so they fail closed rather than rendering answerable.
 */
export function isOpenDecision(d: DecisionView): boolean {
  if (d.status === 'pending' && !d.decision) return true;
  if (d.status === 'decided' && d.decision?.provenance === 'agent_interpreted') return true;
  return false;
}

/**
 * Whether `d` is closed without an effective answer (`replaced`, `withdrawn`,
 * `dismissed`, or any unknown future status).
 */
export function isClosedUnanswered(d: DecisionView): boolean {
  if (isOpenDecision(d)) return false;
  if (d.status === 'decided') return false;
  return true;
}

/** A decision whose state can no longer change back to pending. */
export function isTerminalDecision(view: DecisionView): boolean {
  return (
    view.status === 'decided' ||
    view.status === 'replaced' ||
    view.status === 'withdrawn' ||
    view.status === 'dismissed'
  );
}

/**
 * Why `d` cannot be answered (or confirmed/changed) by `userId`, or `''` when
 * it is actionable. Mirrors `store.DecisionActionableReason`
 * (`pkg/store/decision.go`, pinned against `decision_actionable_cases.json`).
 */
export type ActionableReason =
  | ''
  | 'replaced'
  | 'withdrawn'
  | 'dismissed'
  | 'decided'
  | 'not_recorded_answerer'
  | 'not_actionable';

export function actionableReason(d: DecisionView, userId: string | undefined): ActionableReason {
  if (d.status === 'replaced') return 'replaced';
  if (d.status === 'withdrawn' || d.withdrawn) return 'withdrawn';
  if (d.status === 'dismissed' || d.dismissed) return 'dismissed';
  if (d.replacedBy) return 'replaced';
  if (d.status === 'decided' && d.decision) {
    if (d.decision.provenance !== 'agent_interpreted') return 'decided';
    if (!userId || !d.decision.answeredByUserId || d.decision.answeredByUserId !== userId) {
      return 'not_recorded_answerer';
    }
    return '';
  }
  if (d.status === 'pending' && !d.decision) return '';
  return 'not_actionable';
}

/**
 * How far a decision has gone:
 * pending (0) < decided-interpreted (1) < decided-direct (2) < terminal closed (3).
 * The hub never moves a row back: a direct answer (including confirm/override)
 * supersedes an interpreted answer even when their timestamps tie, and replaced,
 * withdrawn, or dismissed are final.
 */
function statusRank(view: DecisionView): number {
  if (
    view.status === 'replaced' ||
    view.replacedBy ||
    view.status === 'withdrawn' ||
    view.withdrawn ||
    view.status === 'dismissed' ||
    view.dismissed
  ) {
    return 3;
  }
  if (view.status === 'decided') {
    return view.decision?.provenance === 'agent_interpreted' ? 1 : 2;
  }
  return 0;
}

/**
 * The view to hold for a decision row once `incoming` arrives: the incoming
 * one, unless it is behind the held one in the 4-tier `statusRank` ordering
 * (`pending (0) < decided-interpreted (1) < decided-direct (2) < replaced (3)`)
 * — an out-of-order event, a page fetched before the change, or the viewer's
 * own POST answering just before a replacement arrived.
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

/** The decision side maps of a chat history response. */
export interface DecisionHistoryMaps {
  messageDecisions?: Record<string, DecisionView[]>;
  messageDecisionIntros?: Record<string, string>;
  messageDecisionAnswerModes?: Record<string, DecisionAnswerMode>;
  messageDecisionResponses?: Record<string, DecisionResponseView>;
  messageDecisionResponseSets?: Record<string, DecisionResponseSetView>;
}

/**
 * Copies the decision side maps of a history response onto its messages.
 * The hub omits the maps while web.decisions is off, which leaves the
 * messages untouched.
 */
export function applyDecisionHistoryMaps(items: Message[], data: DecisionHistoryMaps): void {
  const decisions = data.messageDecisions;
  const intros = data.messageDecisionIntros;
  const modes = data.messageDecisionAnswerModes;
  const responses = data.messageDecisionResponses;
  const responseSets = data.messageDecisionResponseSets;
  if (!decisions && !intros && !modes && !responses && !responseSets) return;
  for (const m of items) {
    const d = decisions?.[m.id];
    if (d) m.decisions = d;
    const intro = intros?.[m.id];
    if (intro !== undefined) m.decisionIntro = intro;
    const mode = modes?.[m.id];
    if (mode) m.answerMode = mode;
    const r = responses?.[m.id];
    if (r) m.decisionResponse = r;
    const rs = responseSets?.[m.id];
    if (rs) m.decisionResponseSet = rs;
  }
}

/**
 * Keeps the decision fields of a message already shown when the same message
 * arrives again: the newer decision views per row (out-of-order guard), and
 * the held fields when the incoming copy carries none.
 */
export function carryDecisionFields(existing: Message | undefined, incoming: Message): void {
  if (!existing?.decisions) return;
  if (!incoming.decisions) {
    incoming.decisions = existing.decisions;
    if (incoming.decisionIntro === undefined && existing.decisionIntro !== undefined) {
      incoming.decisionIntro = existing.decisionIntro;
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

/** Detail of the `decision-answered` event a card emits after its own POST. */
export interface DecisionAnsweredDetail {
  messageId: string;
  decisions: DecisionView[];
  responseMessageId?: string;
}

/**
 * Detail of the `decision-answer-denied` event a card emits when an answer
 * endpoint refused the viewer with 403 (design §4.5 step 3).
 */
export interface DecisionAnswerDeniedDetail {
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
  idempotencyKey: string,
  surface?: DecisionAnswerSurface
): Promise<AnswerDecisionResult> {
  let res: Response;
  try {
    res = await apiFetch(`/api/v1/decisions/${encodeURIComponent(rowId)}/answer`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        choice_id: choiceId,
        idempotency_key: idempotencyKey,
        ...(surface === 'decisions_page' ? { surface: 'decisions_page' } : {}),
      }),
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
 * POST /api/v1/decision-prompts/{messageId}/answers for a set prompt
 * (design §4.3). A network failure resolves as status 0.
 */
export async function answerDecisionSet(
  messageId: string,
  answers: readonly SetAnswer[],
  idempotencyKey: string,
  surface?: DecisionAnswerSurface
): Promise<AnswerDecisionSetResult> {
  let res: Response;
  try {
    res = await apiFetch(`/api/v1/decision-prompts/${encodeURIComponent(messageId)}/answers`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        idempotency_key: idempotencyKey,
        answers: answers.map((a) => ({ decision_row_id: a.rowId, choice_id: a.choiceId })),
        ...(surface === 'decisions_page' ? { surface: 'decisions_page' } : {}),
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

/**
 * Epoch milliseconds at which a prompt created at `createdAt` reaches the
 * 5-minute dismissal threshold, or `0` when `createdAt` cannot be parsed.
 */
export function dismissAvailableAtMs(createdAt: string): number {
  const parsed = Date.parse(createdAt);
  if (!Number.isFinite(parsed)) return 0;
  return parsed + DISMISS_MIN_AGE_MS;
}

/**
 * Remaining whole minutes (rounded up, minimum 1 while before threshold)
 * until `createdAt + 5 min`, or `0` once dismiss is available.
 */
export function dismissRemainingMinutes(createdAt: string, nowMs = Date.now()): number {
  const availMs = dismissAvailableAtMs(createdAt);
  if (availMs <= 0 || nowMs >= availMs) return 0;
  return Math.max(1, Math.ceil((availMs - nowMs) / 60000));
}

/**
 * What happened to the asking agent's decision-response after a dismissal
 * (`DismissDecisionPromptResponse.deliveryStatus`): `delivered`, `held` (sent
 * at the agent's next session), `gone` (agent deleted), `unreachable`
 * (suspended or stopped: not sent), `failed`, or `none` (nothing new was
 * dismissed, so nothing was sent).
 */
export type DismissDeliveryStatus =
  | 'delivered'
  | 'held'
  | 'gone'
  | 'unreachable'
  | 'failed'
  | 'none';

const DISMISS_DELIVERY_STATUSES: readonly string[] = [
  'delivered',
  'held',
  'gone',
  'unreachable',
  'failed',
  'none',
];

/** The outcome of dismissing open decisions of a prompt. */
export type DismissDecisionPromptResult =
  | {
      ok: true;
      dismissed: DecisionView[];
      alreadyDismissed: DecisionView[];
      responseMessageId: string;
      delivered: boolean;
      deliveryStatus: DismissDeliveryStatus;
    }
  | {
      ok: false;
      status: number;
      code: string;
      message: string;
      availableAt?: string;
      decisions?: DecisionView[];
      reasons?: Record<string, string>;
      reason?: string;
    };

/**
 * Rows the Decisions-page dismiss dialog lists and dismisses: pending rows
 * without an effective answer. The dialog sends exactly these row ids as
 * `decisionIds`, so the hub never dismisses a row the dialog did not show
 * (such as an interpreted answer awaiting the viewer's confirmation).
 */
export function dismissDialogRows(decisions: readonly DecisionView[]): DecisionView[] {
  return decisions.filter((d) => d.status === 'pending' && !d.decision);
}

/**
 * Normalizes the hub's deliveryStatus. An unknown or missing value claims no
 * delivery: `none` when nothing new was dismissed, else `failed`.
 */
function dismissDeliveryStatus(
  raw: string | undefined,
  anyDismissed: boolean
): DismissDeliveryStatus {
  if (raw && DISMISS_DELIVERY_STATUSES.includes(raw)) return raw as DismissDeliveryStatus;
  return anyDismissed ? 'failed' : 'none';
}

/**
 * POST /api/v1/decision-prompts/{messageId}/dismiss (design 8194, §4.3).
 *
 * With `decisionIds`, the hub dismisses exactly those rows or nothing
 * (`409 dismiss_changed` when one is no longer dismissable).
 */
export async function dismissDecisionPrompt(
  messageId: string,
  opts: { reason?: string; surface?: DecisionAnswerSurface; decisionIds?: readonly string[] } = {}
): Promise<DismissDecisionPromptResult> {
  const trimmedReason = opts.reason?.trim() ?? '';
  let res: Response;
  try {
    res = await apiFetch(`/api/v1/decision-prompts/${encodeURIComponent(messageId)}/dismiss`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        ...(opts.decisionIds && opts.decisionIds.length > 0
          ? { decisionIds: [...opts.decisionIds] }
          : {}),
        ...(trimmedReason ? { reason: trimmedReason } : {}),
        ...(opts.surface ? { surface: opts.surface } : {}),
      }),
    });
  } catch {
    return {
      ok: false,
      status: 0,
      code: '',
      message: 'Network error: the dismissal was not sent.',
    };
  }
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  if (res.ok) {
    const ok = body as {
      dismissed?: unknown;
      alreadyDismissed?: unknown;
      responseMessageId?: string;
      delivered?: boolean;
      deliveryStatus?: string;
    } | null;
    const dismissed = decisionViews(ok?.dismissed);
    const alreadyDismissed = decisionViews(ok?.alreadyDismissed);
    if (dismissed && alreadyDismissed) {
      return {
        ok: true,
        dismissed,
        alreadyDismissed,
        responseMessageId: ok?.responseMessageId ?? '',
        delivered: Boolean(ok?.delivered),
        deliveryStatus: dismissDeliveryStatus(ok?.deliveryStatus, dismissed.length > 0),
      };
    }
    return { ok: false, status: res.status, code: '', message: 'Unexpected dismiss response.' };
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
    message: apiErrorMessageFromBody(body) ?? `Failed to dismiss decisions (${res.status}).`,
    ...(typeof details.availableAt === 'string' ? { availableAt: details.availableAt } : {}),
    ...(decisions ? { decisions } : {}),
    ...(reasons ? { reasons } : {}),
    ...(typeof details.reason === 'string' ? { reason: details.reason } : {}),
  };
}
