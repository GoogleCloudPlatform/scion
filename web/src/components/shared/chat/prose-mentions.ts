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
 * Client-side prose @-mention extraction and participant roster resolution
 * for "Reply all" (ptone/scion#7817).
 *
 * Mirrors the hub's `ExtractProseMentions` (`pkg/messages/mentions.go`) for
 * skipping fenced code blocks, inline code spans, and `>` blockquote lines,
 * and `chat-notifications.ts` / `handlers_chat_v2.go` (`mentionedHumanIDs`)
 * for token boundaries and member matching.
 */

/** Resolved participant targeted by "Reply all". */
export interface Participant {
  kind: 'user' | 'agent';
  id: string;
  insertSlug: string;
}

/** Member entry from `ScionChatThread.members`. */
export interface MentionRosterMember {
  id: string;
  name: string;
  email: string;
  kind: 'user' | 'agent';
  slug?: string;
}

/** Agent entry from `ScionChatThread.agentMembers`. */
export interface MentionRosterAgent {
  id: string;
  slug?: string;
  displayName?: string;
  name?: string;
}

/** Combined participant roster available on a chat thread. */
export interface MentionRoster {
  members?: readonly MentionRosterMember[];
  agentMembers?: readonly MentionRosterAgent[];
}

/** Minimal message fields needed to resolve Reply-all targets. */
export interface MentionSourceMessage {
  msg: string;
  sender: string;
  senderId?: string;
}

/**
 * Fold a display name or slug onto the lowercase hyphenated form inserted by
 * `<scion-mention-autocomplete>`.
 */
export function toMentionSlug(value: string): string {
  return value.trim().toLowerCase().replace(/\s+/g, '-');
}

/**
 * Canonical composer mention slug for a human member: display-name slug when
 * present, falling back to lowercased email when display name is empty.
 */
export function userMentionSlug(member: { name?: string; email?: string }): string {
  return toMentionSlug(member.name?.trim() || member.email?.trim() || '');
}

/** Canonical composer mention slug for an agent (`slug || name`). */
export function agentMentionSlug(agent: { slug?: string; name?: string }): string {
  return (agent.slug || agent.name || '').trim();
}

/** Characters that cannot immediately precede a standalone `@mention`. */
const PRECEDING_NON_BOUNDARY_RE = /[\w.@+/-]/;

/** Characters that may continue a mention token (matches `chat-notifications.ts`). */
function isMentionNameChar(ch: string): boolean {
  return /[\w.@+-]/.test(ch);
}

/**
 * Whether `text` already contains `@slug` as a standalone mention token
 * (case-insensitive), using the same preceding/trailing boundary rules as
 * prose mention extraction and `chat-notifications.ts`.
 */
export function hasStandaloneMentionToken(text: string, slug: string): boolean {
  const trimmedSlug = slug.trim().toLowerCase();
  if (!trimmedSlug || !text) return false;
  const lower = text.toLowerCase();
  const needle = `@${trimmedSlug}`;
  let from = 0;
  for (;;) {
    const at = lower.indexOf(needle, from);
    if (at < 0) return false;
    const prev = at > 0 ? lower.charAt(at - 1) : '';
    const next = lower.charAt(at + needle.length);
    const nextNext = lower.charAt(at + needle.length + 1);
    const validStart = !prev || !PRECEDING_NON_BOUNDARY_RE.test(prev);
    const validEnd =
      !next ||
      !isMentionNameChar(next) ||
      (next === '.' && !(nextNext && isMentionNameChar(nextNext)));
    if (validStart && validEnd) {
      return true;
    }
    from = at + 1;
  }
}

/** CommonMark leading indentation width in columns (tab stops of 4). */
function fenceIndentWidth(line: string): number {
  let width = 0;
  for (let i = 0; i < line.length; i++) {
    const ch = line.charAt(i);
    if (ch === ' ') {
      width++;
    } else if (ch === '\t') {
      width += 4 - (width % 4);
    } else {
      break;
    }
  }
  return width;
}

function runLength(s: string, ch: string): number {
  let n = 0;
  while (n < s.length && s.charAt(n) === ch) n++;
  return n;
}

function openingFence(trimmed: string): { ch: '`' | '~'; length: number } | null {
  if (!trimmed) return null;
  const first = trimmed.charAt(0);
  if (first !== '`' && first !== '~') return null;
  const length = runLength(trimmed, first);
  if (length < 3) return null;
  // CommonMark: backtick fence info strings cannot contain backticks
  // (distinguishes ` ```code``` ` inline spans from fenced blocks).
  if (first === '`' && trimmed.slice(length).includes('`')) return null;
  return { ch: first, length };
}

function closesFence(trimmed: string, fenceChar: '`' | '~', fenceLen: number): boolean {
  if (!trimmed || trimmed.charAt(0) !== fenceChar) return false;
  const n = runLength(trimmed, fenceChar);
  if (n < fenceLen) return false;
  return trimmed.slice(n).trim() === '';
}

/**
 * Blank CommonMark inline code spans within a single prose line using spaces
 * so surrounding token boundaries are preserved; unmatched backtick runs stay
 * literal text.
 */
function stripInlineBackticks(line: string): string {
  if (!line.includes('`')) return line;
  const chars = [...line];
  let i = 0;
  while (i < chars.length) {
    if (chars[i] !== '`') {
      i++;
      continue;
    }
    const openStart = i;
    let openLen = 0;
    while (i < chars.length && chars[i] === '`') {
      openLen++;
      i++;
    }
    let closeStart = -1;
    let closeEnd = -1;
    let scan = i;
    while (scan < chars.length) {
      if (chars[scan] !== '`') {
        scan++;
        continue;
      }
      const runStart = scan;
      let runLen = 0;
      while (scan < chars.length && chars[scan] === '`') {
        runLen++;
        scan++;
      }
      if (runLen === openLen) {
        closeStart = runStart;
        closeEnd = scan;
        break;
      }
    }
    if (closeStart === -1) {
      continue;
    }
    for (let k = openStart; k < closeEnd; k++) {
      chars[k] = ' ';
    }
    i = closeEnd;
  }
  return chars.join('');
}

const MENTION_TOKEN_RE =
  /@([a-zA-Z0-9_](?:[\w.+-]*[a-zA-Z0-9_-])?(?:@[a-zA-Z0-9_](?:[\w.-]*[a-zA-Z0-9_-])?)?)/g;

/**
 * Extract lowercased `@mention` tokens from prose in `text` (without the
 * leading `@`), skipping fenced code blocks, inline code spans, and `>`
 * blockquote lines. Preserves first-appearance order and deduplicates
 * identical tokens case-insensitively.
 */
export function extractProseMentionTokens(text: string): string[] {
  if (!text) return [];
  const rawLines = text.split(/\r?\n/);
  const tokens: string[] = [];
  const seen = new Set<string>();

  let fenceChar: '`' | '~' | null = null;
  let fenceLen = 0;

  for (const line of rawLines) {
    const trimmed = line.replace(/^[ \t]+/, '');
    if (fenceChar !== null) {
      if (fenceIndentWidth(line) <= 3 && closesFence(trimmed, fenceChar, fenceLen)) {
        fenceChar = null;
        fenceLen = 0;
      }
      continue;
    }
    const opener = openingFence(trimmed);
    if (opener && fenceIndentWidth(line) <= 3) {
      fenceChar = opener.ch;
      fenceLen = opener.length;
      continue;
    }
    if (trimmed.startsWith('>')) {
      continue;
    }

    const prose = stripInlineBackticks(line);
    MENTION_TOKEN_RE.lastIndex = 0;
    let match: RegExpExecArray | null;
    while ((match = MENTION_TOKEN_RE.exec(prose)) !== null) {
      const atIdx = match.index;
      const prev = atIdx > 0 ? prose.charAt(atIdx - 1) : '';
      if (prev && PRECEDING_NON_BOUNDARY_RE.test(prev)) continue;

      const afterIdx = atIdx + match[0].length;
      const next = prose.charAt(afterIdx);
      if (next === '@') continue;

      const lower = match[1].toLowerCase();
      if (!seen.has(lower)) {
        seen.add(lower);
        tokens.push(lower);
      }
    }
  }

  return tokens;
}

interface IndexedAgent {
  kind: 'agent';
  id: string;
  slug: string;
  name: string;
  insertSlug: string;
}

interface IndexedUser {
  kind: 'user';
  id: string;
  name: string;
  email: string;
  insertSlug: string;
}

function isOriginalSender(
  participant: IndexedAgent | IndexedUser,
  senderId: string,
  senderRef: string
): boolean {
  if (senderId && participant.id && participant.id === senderId) {
    return true;
  }
  if (!senderRef) return false;
  const lowerSender = senderRef.toLowerCase();
  if (participant.kind === 'agent') {
    if (!lowerSender.startsWith('agent:')) return false;
    const senderSlug = lowerSender.slice('agent:'.length).trim();
    return (
      senderSlug !== '' &&
      (participant.slug.toLowerCase() === senderSlug ||
        participant.insertSlug.toLowerCase() === senderSlug ||
        participant.id.toLowerCase() === senderSlug)
    );
  }
  const userRef = lowerSender.startsWith('user:')
    ? lowerSender.slice('user:'.length).trim()
    : lowerSender;
  if (!userRef) return false;
  return (
    (participant.email !== '' && participant.email.toLowerCase() === userRef) ||
    participant.id.toLowerCase() === userRef
  );
}

/**
 * Resolve prose `@`-mentions in `msg` against `roster` (`members` +
 * `agentMembers`), deduplicating by `(kind, id)` in first-appearance order
 * and excluding both `selfId` and `msg`'s original sender.
 */
export function resolveReplyAllTargets(
  msg: MentionSourceMessage,
  roster: MentionRoster,
  selfId: string
): Participant[] {
  const tokens = extractProseMentionTokens(msg.msg);
  if (tokens.length === 0) return [];

  const agentsByKey = new Map<string, IndexedAgent>();
  for (const a of roster.agentMembers ?? []) {
    const slug = (a.slug || '').trim();
    const name = (a.name || a.displayName || '').trim();
    const insertSlug = slug || toMentionSlug(name);
    const id = (a.id || slug).trim();
    if (!id || !insertSlug) continue;
    agentsByKey.set(id, { kind: 'agent', id, slug, name, insertSlug });
  }
  for (const m of roster.members ?? []) {
    if (m.kind !== 'agent') continue;
    const slug = (m.slug || '').trim();
    const name = (m.name || '').trim();
    const insertSlug = slug || toMentionSlug(name);
    const id = (m.id || slug || name).trim();
    if (!id || !insertSlug) continue;
    const existing = agentsByKey.get(id);
    if (existing) {
      if (!existing.name && name) existing.name = name;
      if (!existing.slug && slug) existing.slug = slug;
      existing.insertSlug = existing.slug || toMentionSlug(existing.name);
    } else {
      agentsByKey.set(id, { kind: 'agent', id, slug, name, insertSlug });
    }
  }

  const usersByKey = new Map<string, IndexedUser>();
  for (const m of roster.members ?? []) {
    if (m.kind !== 'user') continue;
    const name = (m.name || '').trim();
    const email = (m.email || '').trim();
    const insertSlug = userMentionSlug(m);
    const id = (m.id || email.toLowerCase()).trim();
    if (!id || !insertSlug) continue;
    if (!usersByKey.has(id)) {
      usersByKey.set(id, { kind: 'user', id, name, email, insertSlug });
    }
  }

  // Build lookup maps: exact agent keys, exact user keys, and unique user local-parts.
  const agentLookup = new Map<string, IndexedAgent>();
  for (const agent of agentsByKey.values()) {
    const keys = [
      agent.slug.toLowerCase(),
      toMentionSlug(agent.slug),
      agent.name.toLowerCase(),
      toMentionSlug(agent.name),
    ];
    for (const k of keys) {
      if (k && !agentLookup.has(k)) agentLookup.set(k, agent);
    }
  }

  const userExactLookup = new Map<string, IndexedUser>();
  const localPartMatches = new Map<string, IndexedUser | 'ambiguous'>();
  for (const user of usersByKey.values()) {
    if (user.name) {
      const slug = toMentionSlug(user.name);
      if (slug && !userExactLookup.has(slug)) userExactLookup.set(slug, user);
      const lowerName = user.name.toLowerCase();
      if (lowerName && !userExactLookup.has(lowerName)) userExactLookup.set(lowerName, user);
    }
    if (user.email) {
      const lowerEmail = user.email.toLowerCase();
      if (!userExactLookup.has(lowerEmail)) userExactLookup.set(lowerEmail, user);
      const at = lowerEmail.indexOf('@');
      if (at > 0) {
        const local = lowerEmail.slice(0, at);
        const prev = localPartMatches.get(local);
        if (prev === undefined) {
          localPartMatches.set(local, user);
        } else if (prev !== 'ambiguous' && prev.id !== user.id) {
          localPartMatches.set(local, 'ambiguous');
        }
      }
    }
  }

  const normalizedSelfId = selfId.trim();
  const normalizedSenderId = (msg.senderId ?? '').trim();
  const normalizedSenderRef = (msg.sender ?? '').trim();

  const out: Participant[] = [];
  const seenParticipants = new Set<string>();

  for (const token of tokens) {
    const localCandidate = localPartMatches.get(token);
    const matched: IndexedAgent | IndexedUser | undefined =
      agentLookup.get(token) ??
      userExactLookup.get(token) ??
      (localCandidate && localCandidate !== 'ambiguous' ? localCandidate : undefined);

    if (!matched) continue;
    if (matched.kind === 'user' && normalizedSelfId !== '' && matched.id === normalizedSelfId) {
      continue;
    }
    if (isOriginalSender(matched, normalizedSenderId, normalizedSenderRef)) {
      continue;
    }

    const canonicalKey = `${matched.kind}:${matched.id}`;
    if (seenParticipants.has(canonicalKey)) continue;
    seenParticipants.add(canonicalKey);
    out.push({
      kind: matched.kind,
      id: matched.id,
      insertSlug: matched.insertSlug,
    });
  }

  return out;
}
