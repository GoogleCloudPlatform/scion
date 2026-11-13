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

import { describe, it, expect } from 'vitest';
import {
  agentMentionSlug,
  extractProseMentionTokens,
  resolveReplyAllTargets,
  toMentionSlug,
  userMentionSlug,
  type MentionRoster,
} from './prose-mentions.js';

describe('extractProseMentionTokens', () => {
  it('extracts lowercased tokens in first-appearance order and deduplicates repeats', () => {
    expect(
      extractProseMentionTokens('Hey @Agent-B and @Alice-Smith, cc @agent-b and @Carol')
    ).toEqual(['agent-b', 'alice-smith', 'carol']);
  });

  it('skips backtick and tilde fenced code blocks, including unclosed fences', () => {
    const body = [
      'Before @first',
      '```ts',
      '@in-backtick-fence',
      '``',
      '@still-in-fence',
      '```',
      'Middle @second',
      '~~~',
      '@in-tilde-fence',
      '~~~',
      'After @third',
      '````md',
      '```',
      '@inside-four-backtick-fence',
    ].join('\n');

    expect(extractProseMentionTokens(body)).toEqual(['first', 'second', 'third']);
  });

  it('skips inline code spans while keeping mentions around unmatched backticks', () => {
    const body = [
      'Use `@ignored-one` and `` `@ignored-two` `` with @kept-one.',
      'Stray ` backtick before @kept-two and ```@ignored-inline``` after.',
    ].join('\n');

    expect(extractProseMentionTokens(body)).toEqual(['kept-one', 'kept-two']);
  });

  it('skips blockquote lines starting with >', () => {
    const body = [
      '> @quoted-agent said hello',
      '  > @indented-quote too',
      '@real-recipient please check',
    ].join('\n');

    expect(extractProseMentionTokens(body)).toEqual(['real-recipient']);
  });

  it('handles punctuation boundaries, sentence-ending periods, emails, and URLs', () => {
    const body =
      '(@agent-b, "@carol"!) Ask @alice.smith. Also @dave+dev@example.com. ' +
      'Ignore contact@example.com, @@double, and https://example.com/@urluser.';

    expect(extractProseMentionTokens(body)).toEqual([
      'agent-b',
      'carol',
      'alice.smith',
      'dave+dev@example.com',
    ]);
  });
});

describe('mention slug helpers', () => {
  it('formats display names and falls back to email when display name is empty', () => {
    expect(toMentionSlug('  Preston   Holmes ')).toBe('preston-holmes');
    expect(userMentionSlug({ name: 'Alice Smith', email: 'alice@example.com' })).toBe(
      'alice-smith'
    );
    expect(userMentionSlug({ name: '', email: 'NoName@Example.com' })).toBe('noname@example.com');
  });
});

describe('resolveReplyAllTargets', () => {
  const roster: MentionRoster = {
    members: [
      { id: 'u-me', name: 'Me User', email: 'me@example.com', kind: 'user' },
      { id: 'u-alice', name: 'Alice Smith', email: 'asmith@example.com', kind: 'user' },
      { id: 'u-carol', name: 'Carol', email: 'carol@example.com', kind: 'user' },
      { id: 'u-noname', name: '', email: 'noname@example.com', kind: 'user' },
      { id: 'u-alex-1', name: 'Alex One', email: 'alex@corp.com', kind: 'user' },
      { id: 'u-alex-2', name: 'Alex Two', email: 'alex@other.org', kind: 'user' },
      { id: 'a-writer', name: 'Writer Bot', email: '', slug: 'writer', kind: 'agent' },
    ],
    agentMembers: [
      { id: 'a-sender', slug: 'agent-a', displayName: 'Agent A' },
      { id: 'a-peer', slug: 'agent-b', displayName: 'Agent B' },
    ],
  };

  it('resolves agents and users in first-appearance order and ignores non-members', () => {
    const targets = resolveReplyAllTargets(
      {
        sender: 'agent:agent-a',
        senderId: 'a-sender',
        msg: 'See `@Override` — @agent-b @alice-smith @departed @writer thoughts?',
      },
      roster,
      'u-me'
    );

    expect(targets).toEqual([
      { kind: 'agent', id: 'a-peer', insertSlug: 'agent-b' },
      { kind: 'user', id: 'u-alice', insertSlug: 'alice-smith' },
      { kind: 'agent', id: 'a-writer', insertSlug: 'writer' },
    ]);
  });

  it('matches users by slug, single-word name, full email, and unique email local-part', () => {
    const targets = resolveReplyAllTargets(
      {
        sender: 'agent:agent-a',
        senderId: 'a-sender',
        msg: '@asmith @Carol @noname@example.com',
      },
      roster,
      'u-me'
    );

    expect(targets).toEqual([
      { kind: 'user', id: 'u-alice', insertSlug: 'alice-smith' },
      { kind: 'user', id: 'u-carol', insertSlug: 'carol' },
      { kind: 'user', id: 'u-noname', insertSlug: 'noname@example.com' },
    ]);
  });

  it('ignores ambiguous email local-parts while allowing exact slug or full email', () => {
    expect(
      resolveReplyAllTargets(
        { sender: 'agent:agent-a', senderId: 'a-sender', msg: 'Hey @alex check this' },
        roster,
        'u-me'
      )
    ).toEqual([]);

    expect(
      resolveReplyAllTargets(
        {
          sender: 'agent:agent-a',
          senderId: 'a-sender',
          msg: 'Hey @alex@corp.com and @alex-two check this',
        },
        roster,
        'u-me'
      )
    ).toEqual([
      { kind: 'user', id: 'u-alex-1', insertSlug: 'alex-one' },
      { kind: 'user', id: 'u-alex-2', insertSlug: 'alex-two' },
    ]);
  });

  it('deduplicates the same participant mentioned via multiple forms', () => {
    const targets = resolveReplyAllTargets(
      {
        sender: 'agent:agent-a',
        senderId: 'a-sender',
        msg: '@alice-smith and @asmith@example.com and @asmith and @Agent-B and @agent-b',
      },
      roster,
      'u-me'
    );

    expect(targets).toEqual([
      { kind: 'user', id: 'u-alice', insertSlug: 'alice-smith' },
      { kind: 'agent', id: 'a-peer', insertSlug: 'agent-b' },
    ]);
  });

  it('excludes both current user (self) and original sender (by senderId or sender ref)', () => {
    expect(
      resolveReplyAllTargets(
        {
          sender: 'agent:agent-a',
          senderId: 'a-sender',
          msg: '@me-user @agent-a only us two',
        },
        roster,
        'u-me'
      )
    ).toEqual([]);

    // Fallback when senderId is absent on historical message
    expect(
      resolveReplyAllTargets(
        {
          sender: 'user:asmith@example.com',
          msg: '@alice-smith @me@example.com',
        },
        roster,
        'u-me'
      )
    ).toEqual([]);
  });

  it('includes other mentioned participants on the current user own message', () => {
    const targets = resolveReplyAllTargets(
      {
        sender: 'user:me@example.com',
        senderId: 'u-me',
        msg: '@me-user @agent-b @carol following up',
      },
      roster,
      'u-me'
    );

    expect(targets).toEqual([
      { kind: 'agent', id: 'a-peer', insertSlug: 'agent-b' },
      { kind: 'user', id: 'u-carol', insertSlug: 'carol' },
    ]);
  });

  it('normalizes empty-slug agent fallback to a single mention token while keeping agentMentionSlug slug || name', () => {
    expect(agentMentionSlug({ slug: 'agent-b', name: 'Agent B' })).toBe('agent-b');
    expect(agentMentionSlug({ slug: '', name: 'Bot Name' })).toBe('Bot Name');

    const emptySlugRoster: MentionRoster = {
      agentMembers: [{ id: 'a-noslug', slug: '', displayName: 'Bot Name' }],
      members: [
        { id: 'u-me', name: 'Me User', email: 'me@example.com', kind: 'user' },
        { id: 'a-noslug', name: 'Bot Name', email: '', slug: '', kind: 'agent' },
      ],
    };

    const targets = resolveReplyAllTargets(
      {
        sender: 'agent:agent-a',
        senderId: 'a-sender',
        msg: 'Check with @bot-name please',
      },
      emptySlugRoster,
      'u-me'
    );

    expect(targets).toEqual([{ kind: 'agent', id: 'a-noslug', insertSlug: 'bot-name' }]);
  });
});
