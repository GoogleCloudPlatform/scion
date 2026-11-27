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
 * Presentation eligibility and helper functions for Markdown Execute on fenced
 * shell code blocks (ptone/scion#9286, design §4.14).
 *
 * Every eligibility rule here is presentation-only; the Hub re-enforces all
 * rules authoritatively on `POST /api/v1/command-requests/from-message`.
 */

import {
  validateCommandRequestScript,
  type CommandRequestExecContainerView,
} from '../../../client/command-requests.js';

/** Fenced code block info-string languages eligible for Markdown Execute (§4.14). */
export const SHELL_EXECUTE_LANGS: ReadonlySet<string> = new Set(['bash', 'sh', 'shell', 'console']);

/** Maximum message age for Markdown Execute (24 h, D4 / §4.14). */
export const MARKDOWN_EXECUTE_MAX_AGE_MS = 24 * 60 * 60 * 1000;

export interface MarkdownExecuteMemberInfo {
  id?: string;
  kind?: string;
  email?: string;
  displayName?: string;
  name?: string;
  slug?: string;
}

/** Reports whether `timestamp` is a valid timestamp strictly younger than 24 h. */
export function isMessageWithinExecuteWindow(
  timestamp: string,
  nowMs: number = Date.now()
): boolean {
  if (!timestamp) return false;
  const parsed = Date.parse(timestamp);
  if (!Number.isFinite(parsed)) return false;
  return nowMs - parsed < MARKDOWN_EXECUTE_MAX_AGE_MS;
}

/**
 * Strips top-level fenced code blocks, `>` blockquotes, and inline code spans
 * so `@`-mention extraction only inspects prose (mirroring Go's
 * `messages.ExtractProseMentions`).
 */
function extractProseMentions(body: string): string[] {
  const lines = body.replace(/\r\n/g, '\n').split('\n');
  const proseLines: string[] = [];
  let inFence = false;
  let fenceChar = '';
  let fenceLen = 0;

  for (const line of lines) {
    if (!inFence) {
      const m = line.match(/^(`{3,}|~{3,})/);
      if (m) {
        inFence = true;
        fenceChar = m[1][0];
        fenceLen = m[1].length;
        continue;
      }
      if (/^\s*>/.test(line)) {
        continue;
      }
      // Strip inline code spans.
      proseLines.push(line.replace(/`[^`]*`/g, ' '));
    } else {
      const closeRe = new RegExp(`^${fenceChar === '`' ? '`' : '~'}{${fenceLen},}[ \\t]*$`);
      if (closeRe.test(line)) {
        inFence = false;
        fenceChar = '';
        fenceLen = 0;
      }
    }
  }

  const prose = proseLines.join('\n');
  const found = new Set<string>();
  const mentionRe =
    /(?:^|[\s([{"',;:])@([A-Za-z0-9][A-Za-z0-9._+-]*(?:@[A-Za-z0-9.-]+\.[A-Za-z]{2,})?)/g;
  let match: RegExpExecArray | null;
  while ((match = mentionRe.exec(prose)) !== null) {
    const token = match[1].replace(/[.,;:!?)]+$/, '').toLowerCase();
    if (token) found.add(token);
  }
  return Array.from(found);
}

/**
 * Enforces the §4.14 / D6 recipient presentation rule:
 * - In a DM, the viewer is the DM human recipient.
 * - In a group conversation or thread, the message must be addressed to
 *   exactly one human (via `recipient`/`recipientId` or a single human
 *   `@-mention` in prose), and that human must be `currentUserId`.
 */
export function isViewerSingleAddressedHuman(opts: {
  isDirectConversation: boolean;
  currentUserId: string;
  recipient?: string | undefined;
  recipientId?: string | undefined;
  body: string;
  members?: readonly MarkdownExecuteMemberInfo[] | undefined;
  knownUsersByEmail?: ReadonlyMap<string, string> | null | undefined;
}): boolean {
  const { currentUserId } = opts;
  if (!currentUserId) return false;
  if (opts.isDirectConversation) {
    return true;
  }

  const addressedHumans = new Set<string>();
  if (opts.recipient?.startsWith('user:') && opts.recipientId) {
    addressedHumans.add(opts.recipientId);
  }

  const humanByMention = new Map<string, string>();
  if (opts.members) {
    for (const m of opts.members) {
      if (m.kind && m.kind !== 'user') continue;
      const memberKey = m.id || m.email?.trim().toLowerCase() || m.slug?.trim().toLowerCase() || '';
      if (!memberKey) continue;
      if (m.email) {
        humanByMention.set(m.email.trim().toLowerCase(), memberKey);
      }
      const display = m.displayName || m.name;
      if (display) {
        const slug = display.trim().toLowerCase().replace(/\s+/g, '-');
        if (slug) humanByMention.set(slug, memberKey);
      }
      if (m.slug) {
        humanByMention.set(m.slug.trim().toLowerCase(), memberKey);
      }
    }
  }

  const mentions = extractProseMentions(opts.body);
  for (const m of mentions) {
    const uid = humanByMention.get(m);
    if (uid) {
      addressedHumans.add(uid);
    } else if (m.includes('@')) {
      // Fail closed on an unrecognized email mention (another human).
      addressedHumans.add(`unknown:${m}`);
    }
  }

  return addressedHumans.size === 1 && addressedHumans.has(currentUserId);
}

export type MarkdownBlockExecuteState =
  | { mode: 'hidden' }
  | { mode: 'setup-link' }
  | { mode: 'disabled'; reason: string }
  | { mode: 'enabled' };

/**
 * Evaluates whether a fenced shell code block in a chat message should show
 * Execute, a disabled Execute with tooltip, a Set-up link, or Copy only (§4.14).
 */
export function evaluateMarkdownBlockExecuteState(opts: {
  experimentEnabled: boolean;
  senderIsAgent: boolean;
  messageType: string;
  messageId: string;
  timestamp: string;
  langTag: string;
  noExecute: boolean;
  rawScript: string;
  isDirectConversation: boolean;
  currentUserId: string;
  recipient?: string;
  recipientId?: string;
  body: string;
  members?: readonly MarkdownExecuteMemberInfo[];
  knownUsersByEmail?: ReadonlyMap<string, string> | null;
  execContainer: CommandRequestExecContainerView | null;
  nowMs?: number;
}): MarkdownBlockExecuteState {
  if (!opts.experimentEnabled) return { mode: 'hidden' };
  if (!opts.senderIsAgent) return { mode: 'hidden' };
  if (opts.messageType === 'command-request') return { mode: 'hidden' };
  if (!opts.messageId) return { mode: 'hidden' };
  if (!SHELL_EXECUTE_LANGS.has(opts.langTag.toLowerCase())) return { mode: 'hidden' };
  if (opts.noExecute) return { mode: 'hidden' };
  if (!isMessageWithinExecuteWindow(opts.timestamp, opts.nowMs)) return { mode: 'hidden' };
  if (
    !isViewerSingleAddressedHuman({
      isDirectConversation: opts.isDirectConversation,
      currentUserId: opts.currentUserId,
      recipient: opts.recipient,
      recipientId: opts.recipientId,
      body: opts.body,
      members: opts.members,
      knownUsersByEmail: opts.knownUsersByEmail,
    })
  ) {
    return { mode: 'hidden' };
  }

  const ec = opts.execContainer;
  if (!ec) return { mode: 'hidden' };
  if (ec.state === 'unconfigured') {
    return { mode: 'setup-link' };
  }

  const scriptErr = validateCommandRequestScript(opts.rawScript);
  if (scriptErr) {
    return { mode: 'disabled', reason: scriptErr };
  }

  if (ec.state === 'stopped') {
    return { mode: 'disabled', reason: 'Start your execution container' };
  }
  if (ec.state === 'unavailable') {
    return { mode: 'disabled', reason: 'Your execution container is unavailable' };
  }
  if (ec.state === 'suspended' && ec.canWake === false) {
    return { mode: 'disabled', reason: 'Resume your execution container' };
  }

  return { mode: 'enabled' };
}

export function markdownRunStatusLabel(status: string): string {
  switch (status) {
    case 'queued':
      return 'Queued';
    case 'waking':
      return 'Waking container…';
    case 'dispatching':
      return 'Starting…';
    case 'running':
      return 'Running';
    case 'succeeded':
      return 'Succeeded';
    case 'failed':
      return 'Failed';
    case 'unknown':
      return 'Unknown — may have run';
    case 'not_started':
      return 'Not started';
    default:
      return status;
  }
}

export function markdownRunStatusTone(status: string): string {
  switch (status) {
    case 'succeeded':
      return 'success';
    case 'failed':
      return 'danger';
    case 'unknown':
      return 'warning';
    case 'not_started':
      return 'neutral';
    default:
      return 'info';
  }
}
