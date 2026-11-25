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
 * Pure formatting, date-bound, avatar, surface, link, and prompt classification
 * helpers shared by `<scion-page-project-decisions>` and
 * `<scion-page-profile-decisions>` (Design §4.3 R7, ptone/scion#7996).
 */

import type { DecisionView } from '../../shared/types.js';
import {
  ANSWERABLE,
  GENERIC_ANSWER_DENIED_REASON,
  actionableReason,
  dismissDialogRows,
  isOpenDecision,
  type AskerAnswerability,
  type DismissDeliveryStatus,
  type DismissDecisionPromptResult,
} from '../../client/decision-prompts.js';
import type { ToastVariant } from '../../utils/toast.js';
import { chatConversationPath } from '../../client/chat-routes.js';
import type {
  ProjectDecisionLogEntryView,
  ProjectDecisionPromptView,
} from '../../client/project-decisions.js';
import { effectiveTimeZone, formatInstant, parseWallClock } from '../../utils/time.js';

/**
 * The toast after a successful dismiss call: what was dismissed, and only a
 * truthful claim about whether the asking agent was told (deliveryStatus).
 */
export function dismissResultToast(
  deliveryStatus: DismissDeliveryStatus,
  dismissedCount: number,
  alreadyDismissedCount: number,
  askerName: string
): { message: string; variant: ToastVariant } {
  const name = askerName || 'The agent';
  const midName = askerName || 'the agent';
  const done =
    dismissedCount === 1 ? 'Decision dismissed.' : `Dismissed ${dismissedCount} decisions.`;
  switch (deliveryStatus) {
    case 'none':
      return {
        message:
          alreadyDismissedCount === 1
            ? 'Already dismissed.'
            : `${alreadyDismissedCount} decisions were already dismissed.`,
        variant: 'neutral',
      };
    case 'delivered':
      return { message: done, variant: 'success' };
    case 'held':
      return {
        message: `${done} ${midName} will be told when it can receive messages.`,
        variant: 'success',
      };
    case 'gone':
      return { message: `${name} is gone; nothing was sent.`, variant: 'neutral' };
    case 'unreachable':
      return { message: `${name} is suspended or stopped; it was not told.`, variant: 'warning' };
    case 'failed':
    default:
      return { message: `${done} Could not confirm that ${midName} was told.`, variant: 'warning' };
  }
}

export interface WaitingOnOthersItem {
  prompt: ProjectDecisionPromptView;
  decision: DecisionView;
  reason: string;
}

export const DEFAULT_DECISIONS_PAGE_SIZE = 50;
export const MAX_DECISIONS_PAGE_SIZE = 200;
export const SEARCH_DEBOUNCE_MS = 300;
export const MAX_BULK_DISMISS_SELECTION = 50;
export const MAX_BULK_DISMISS_CONCURRENCY = 4;
export const MAX_DISMISS_REASON_RUNES = 500;

export function clampDismissReason(raw: string): string {
  const runes = [...raw];
  return runes.length > MAX_DISMISS_REASON_RUNES
    ? runes.slice(0, MAX_DISMISS_REASON_RUNES).join('')
    : raw;
}

export interface BulkDismissCardOutcome {
  prompt: ProjectDecisionPromptView;
  result: DismissDecisionPromptResult;
}

/**
 * Whether a failed dismiss result failed solely because every listed row is
 * still inside the 5-minute grace period (`409 too_recent` or
 * `409 dismiss_changed` where every reason is `'too_recent'`).
 */
export function isOnlyTooRecentDismissError(res: DismissDecisionPromptResult): boolean {
  if (res.ok) return false;
  if (res.code === 'too_recent') return true;
  return (
    res.code === 'dismiss_changed' &&
    Boolean(res.reasons) &&
    Object.keys(res.reasons!).length > 0 &&
    Object.values(res.reasons!).every((r) => r === 'too_recent')
  );
}

/**
 * Runs `fn` over `items` with at most `limit` promises in flight at any time,
 * resolving to results in the original `items` order.
 */
export async function runWithConcurrencyLimit<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T, index: number) => Promise<R>
): Promise<R[]> {
  if (items.length === 0) return [];
  const cap = Math.max(1, Math.trunc(limit));
  const out = new Array<R>(items.length);
  let nextIndex = 0;

  const worker = async (): Promise<void> => {
    while (nextIndex < items.length) {
      const idx = nextIndex++;
      out[idx] = await fn(items[idx], idx);
    }
  };

  const workerCount = Math.min(cap, items.length);
  await Promise.all(Array.from({ length: workerCount }, () => worker()));
  return out;
}

function formatCardFailureTarget(p: ProjectDecisionPromptView): string {
  const agent = p.askerAgentName || 'Agent';
  const where = conversationDisplayLabel(p.chatKey, p.conversationLabel);
  return `${agent} · ${where}`;
}

function formatCappedList(items: readonly string[], maxShown = 3): string {
  if (items.length <= maxShown) return items.join(', ');
  return `${items.slice(0, maxShown).join(', ')}, +${items.length - maxShown} more`;
}

/**
 * Aggregates multi-card dismiss outcomes into one truthful summary toast:
 * reports partial failures per card and summarizes `deliveryStatus` without
 * claiming delivery for gone, suspended/unreachable, or unconfirmed dispatches.
 */
export function bulkDismissResultToast(outcomes: readonly BulkDismissCardOutcome[]): {
  message: string;
  variant: ToastVariant;
} {
  if (outcomes.length === 1 && outcomes[0].result.ok) {
    const { prompt, result } = outcomes[0];
    return dismissResultToast(
      result.deliveryStatus,
      result.dismissed.length,
      result.alreadyDismissed.length,
      prompt.askerAgentName
    );
  }

  const total = outcomes.length;
  let succeededCount = 0;
  let failedCount = 0;
  let newlyDismissedCount = 0;
  const tally: Record<DismissDeliveryStatus, number> = {
    none: 0,
    delivered: 0,
    held: 0,
    gone: 0,
    unreachable: 0,
    failed: 0,
  };
  const newlyDismissedAskers = new Set<string>();
  const changedTargets: string[] = [];
  const tooRecentTargets: string[] = [];
  const errorItems: string[] = [];

  for (const { prompt, result } of outcomes) {
    if (result.ok) {
      succeededCount++;
      const status: DismissDeliveryStatus =
        result.deliveryStatus in tally ? result.deliveryStatus : 'failed';
      tally[status]++;
      if (status !== 'none') {
        newlyDismissedCount++;
        if (prompt.askerAgentName) newlyDismissedAskers.add(prompt.askerAgentName);
      }
      continue;
    }
    failedCount++;
    const target = formatCardFailureTarget(prompt);
    if (isOnlyTooRecentDismissError(result)) {
      tooRecentTargets.push(target);
    } else if (result.code === 'dismiss_changed') {
      changedTargets.push(target);
    } else {
      errorItems.push(`${target}: ${result.message || 'failed'}`);
    }
  }

  const failureClause = [
    changedTargets.length > 0 &&
      `${changedTargets.length} had changed (${formatCappedList(changedTargets)})`,
    tooRecentTargets.length > 0 &&
      `${tooRecentTargets.length} too recent (${formatCappedList(tooRecentTargets)})`,
    errorItems.length > 0 && `${errorItems.length} failed (${formatCappedList(errorItems)})`,
  ]
    .filter(Boolean)
    .join('; ');

  if (succeededCount > 0 && newlyDismissedCount === 0) {
    return failedCount === 0
      ? { message: `${succeededCount} cards were already dismissed.`, variant: 'neutral' }
      : {
          message: `${succeededCount} of ${total} cards already dismissed; ${failureClause}.`,
          variant: 'warning',
        };
  }

  const lead =
    failedCount === 0
      ? `Dismissed ${succeededCount} ${succeededCount === 1 ? 'card' : 'cards'}.`
      : `Dismissed ${succeededCount} of ${total} ${total === 1 ? 'card' : 'cards'}; ${failureClause}.`;

  let deliveryTail = '';
  if (newlyDismissedCount > 0) {
    if (tally.gone === newlyDismissedCount) {
      const onlyAsker = newlyDismissedAskers.size === 1 ? [...newlyDismissedAskers][0] : '';
      deliveryTail = onlyAsker
        ? `${onlyAsker} is gone; nothing was sent.`
        : 'Asking agents are gone; nothing was sent.';
    } else {
      const notes = [
        tally.held > 0 &&
          `${tally.held === 1 ? '1 response' : `${tally.held} responses`} will be sent when the agent can receive messages`,
        tally.gone > 0 &&
          `${tally.gone} asking ${tally.gone === 1 ? 'agent is' : 'agents are'} gone (nothing sent)`,
        tally.unreachable > 0 &&
          `${tally.unreachable} ${tally.unreachable === 1 ? 'agent is' : 'agents are'} suspended or stopped; not told`,
        tally.failed > 0 && `could not confirm delivery for ${tally.failed}`,
      ].filter((s): s is string => Boolean(s));
      if (notes.length > 0) {
        const joined = notes.join('; ');
        deliveryTail = `${joined.charAt(0).toUpperCase()}${joined.slice(1)}.`;
      }
    }
  }

  const message = deliveryTail ? `${lead} ${deliveryTail}` : lead;
  const variant: ToastVariant =
    succeededCount === 0 && errorItems.length > 0
      ? 'danger'
      : failedCount > 0 || tally.unreachable > 0 || tally.failed > 0
        ? 'warning'
        : tally.delivered === 0 && tally.held === 0
          ? 'neutral'
          : 'success';
  return { message, variant };
}

const AVATAR_PALETTE = ['#7c3aed', '#059669', '#0284c7', '#d97706', '#e11d48', '#4f46e5'] as const;

const EDITABLE_TAG_NAMES = new Set([
  'input',
  'textarea',
  'select',
  'sl-input',
  'sl-textarea',
  'sl-select',
]);

export function isEditableTarget(el: EventTarget | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  if (el.isContentEditable) return true;
  return EDITABLE_TAG_NAMES.has(el.tagName.toLowerCase());
}

export function nextCalendarDate(ymd: string): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(ymd);
  if (!match) return '';
  const y = Number(match[1]);
  const m = Number(match[2]);
  const day = Number(match[3]);
  const d = new Date(0);
  d.setUTCFullYear(y, m - 1, day + 1);
  d.setUTCHours(0, 0, 0, 0);
  const ny = String(d.getUTCFullYear()).padStart(4, '0');
  const nm = String(d.getUTCMonth() + 1).padStart(2, '0');
  const nd = String(d.getUTCDate()).padStart(2, '0');
  return `${ny}-${nm}-${nd}`;
}

export function dateBoundToIso(ymd: string, endOfDay: boolean): string {
  if (!ymd) return '';
  const zone = effectiveTimeZone();
  if (!endOfDay) {
    return parseWallClock(`${ymd}T00:00`, zone) || ymd;
  }
  const nextYmd = nextCalendarDate(ymd);
  const nextStartIso = nextYmd ? parseWallClock(`${nextYmd}T00:00`, zone) : '';
  if (nextStartIso) {
    const ms = Date.parse(nextStartIso);
    if (Number.isFinite(ms)) {
      return new Date(ms - 1).toISOString();
    }
  }
  return ymd;
}

export function avatarColor(seed: string | undefined | null): string {
  const s = seed ?? '';
  let hash = 0;
  for (let i = 0; i < s.length; i++) {
    hash = (hash * 31 + s.charCodeAt(i)) | 0;
  }
  const idx = Math.abs(hash) % AVATAR_PALETTE.length;
  return AVATAR_PALETTE[idx] ?? AVATAR_PALETTE[0];
}

export function avatarInitial(name: string | undefined | null): string {
  const ch = (name || '?').trim().charAt(0).toUpperCase();
  return ch || '?';
}

export function surfaceDisplay(surface: string): { icon: string; label: string } {
  switch (surface) {
    case 'decisions_page':
      return { icon: 'window', label: 'Decisions page' };
    case 'inbox':
      return { icon: 'inbox', label: 'Inbox' };
    case 'native':
    case 'web':
      return { icon: 'chat-dots', label: 'Chat' };
    case 'slack':
      return { icon: 'slack', label: 'Slack' };
    case 'google_chat':
      return { icon: 'chat-dots', label: 'Google Chat' };
    case 'discord':
      return { icon: 'discord', label: 'Discord' };
    case 'telegram':
      return { icon: 'telegram', label: 'Telegram' };
    case 'teams':
      return { icon: 'microsoft-teams', label: 'Teams' };
    case 'other':
    case '':
      return { icon: '', label: 'Other' };
    default:
      return { icon: '', label: surface };
  }
}

export function formatDecisionLogWhen(iso: string): string {
  const today = formatInstant(new Date().toISOString(), 'date');
  const rowDate = formatInstant(iso, 'date');
  if (today && rowDate === today) {
    return formatInstant(iso, 'time');
  }
  return formatInstant(iso, 'datetime');
}

export function conversationDisplayLabel(chatKey: string, conversationLabel: string): string {
  if (chatKey.startsWith('dm:')) return 'your DM';
  return conversationLabel || 'chat';
}

export function conversationHref(
  chatKey: string,
  available: boolean,
  projectId: string | undefined
): string | null {
  if (!available || !projectId) return null;
  return chatConversationPath({ conversationKey: chatKey, projectId });
}

export function replyHrefForEntry(
  e: ProjectDecisionLogEntryView,
  projectId: string | undefined
): string | null {
  if (!e.sourceMessageId || !e.sourceMessageAvailable || !e.conversationAvailable) {
    return null;
  }
  const convHref = conversationHref(e.chatKey, e.conversationAvailable, projectId);
  if (!convHref) return null;
  return `${convHref}#msg-${encodeURIComponent(e.sourceMessageId)}`;
}

export function promptAnswerability(p: ProjectDecisionPromptView): AskerAnswerability {
  if (p.canAnswer) return ANSWERABLE;
  return {
    state: 'blocked',
    reason: p.disabledReason || GENERIC_ANSWER_DENIED_REASON,
  };
}

export function promptHasWaitingOnOthers(p: ProjectDecisionPromptView): boolean {
  return p.decisions.some(
    (d) => Boolean(p.waitingReasons?.[d.rowId]) || (!p.waitingOnYou && isOpenDecision(d))
  );
}

export function collectWaitingOnOthersItems(
  prompts: readonly ProjectDecisionPromptView[]
): WaitingOnOthersItem[] {
  const items: WaitingOnOthersItem[] = [];
  for (const p of prompts) {
    for (const d of p.decisions) {
      const reason = p.waitingReasons?.[d.rowId];
      if (reason) {
        items.push({ prompt: p, decision: d, reason });
      } else if (!p.waitingOnYou && isOpenDecision(d)) {
        items.push({ prompt: p, decision: d, reason: 'other_user_confirm' });
      }
    }
  }
  return items;
}

/**
 * Decision rows of `p` that wait on the viewer and may be dismissed by the
 * viewer on Waiting on you (pending rows without a block reason, plus D6
 * interpreted answers awaiting the viewer's confirmation).
 */
export function promptWaitingOnViewerRows(
  p: ProjectDecisionPromptView,
  currentUserId: string
): DecisionView[] {
  if (!p.waitingOnYou || !p.canAnswer) return [];
  return dismissDialogRows(p.decisions, currentUserId || undefined, p.waitingReasons);
}

export function countActionableInPrompt(
  p: ProjectDecisionPromptView,
  currentUserId: string
): number {
  if (!p.waitingOnYou) return 0;
  const n = p.decisions.filter(
    (d) => !p.waitingReasons?.[d.rowId] && actionableReason(d, currentUserId) === ''
  ).length;
  return n > 0 ? n : Math.max(1, p.decisions.length);
}
