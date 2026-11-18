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
  type AskerAnswerability,
} from '../../client/decision-prompts.js';
import { chatConversationPath } from '../../client/chat-routes.js';
import type {
  ProjectDecisionLogEntryView,
  ProjectDecisionPromptView,
} from '../../client/project-decisions.js';
import { effectiveTimeZone, formatInstant, parseWallClock } from '../../utils/time.js';

export interface WaitingOnOthersItem {
  prompt: ProjectDecisionPromptView;
  decision: DecisionView;
  reason: string;
}

export const DEFAULT_DECISIONS_PAGE_SIZE = 50;
export const MAX_DECISIONS_PAGE_SIZE = 200;
export const SEARCH_DEBOUNCE_MS = 300;

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
    (d) =>
      Boolean(p.waitingReasons?.[d.rowId]) ||
      (!p.waitingOnYou &&
        d.status !== 'replaced' &&
        !(d.status === 'decided' && d.decision?.provenance !== 'agent_interpreted'))
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
      } else if (
        !p.waitingOnYou &&
        d.status !== 'replaced' &&
        !(d.status === 'decided' && d.decision?.provenance !== 'agent_interpreted')
      ) {
        items.push({ prompt: p, decision: d, reason: 'other_user_confirm' });
      }
    }
  }
  return items;
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
