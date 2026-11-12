// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Choice prompt fixtures for choices.pw.ts (ptone/scion#7673): the states of
 * the approved mocks as hub history rows, with the choice side maps the hub
 * sends while web.chat_choices is on.
 */

import type { Page, Route } from '@playwright/test';
import { GENERAL_THREAD_ID, PROJECT_A } from './mock-api.js';

/** The signed-in user (mock-api.ts `/auth/me`). */
export const SELF_ID = 'fixture-user';
export const BOB_ID = 'user-bob';
export const ASKER_ID = 'agent-release-bot';
export const ASKER_NAME = 'release-bot';

/** 15:41 UTC; the suite runs in UTC, so times read 15:41, 15:42, … */
const BASE = Date.parse('2026-09-01T15:41:00Z');
export const at = (minutes: number): string => new Date(BASE + minutes * 60_000).toISOString();

type Json = Record<string, unknown>;

export interface ChoiceThread {
  messages: Json[];
  messageDecisions?: Record<string, Json[]>;
  messageChoiceIntros?: Record<string, string>;
  messageChoiceAnswerModes?: Record<string, 'single' | 'set'>;
  messageChoiceResponses?: Record<string, Json>;
  messageChoiceResponseSets?: Record<string, Json>;
  canPost?: boolean;
}

interface DecisionOpts {
  messageId: string;
  rowId: string;
  key: string;
  question: string;
  choices: { id: string; label: string; description?: string }[];
  defaultChoiceId?: string;
  version?: number;
  status?: 'pending' | 'decided' | 'replaced';
  decision?: Json;
  replacedBy?: Json;
}

export function decision(o: DecisionOpts): Json {
  return {
    rowId: o.rowId,
    messageId: o.messageId,
    key: o.key,
    ref: o.key,
    version: o.version ?? 1,
    status: o.status ?? 'pending',
    askerAgentId: ASKER_ID,
    askerAgentName: ASKER_NAME,
    question: o.question,
    ...(o.defaultChoiceId ? { defaultChoiceId: o.defaultChoiceId } : {}),
    choices: o.choices,
    ...(o.replacedBy ? { replacedBy: o.replacedBy } : {}),
    ...(o.decision ? { decision: o.decision } : {}),
  };
}

/** A direct answer by `userId` (`name` for display). */
export function answer(
  choiceId: string,
  label: string,
  userId: string,
  name: string,
  minutes: number,
  extra: Json = {}
): Json {
  return {
    choiceId,
    label,
    answeredByUserId: userId,
    answeredByName: name,
    provenance: 'direct',
    outcome: 'answer',
    surface: 'web',
    responseMessageId: `resp-${choiceId}-${minutes}`,
    at: at(minutes),
    ...extra,
  };
}

/** A hub history row. */
export function row(
  id: string,
  minutes: number,
  type: string,
  body: string,
  from: 'asker' | 'self' | 'bob',
  extra: Json = {}
): Json {
  const senderId = from === 'asker' ? ASKER_ID : from === 'self' ? SELF_ID : BOB_ID;
  return {
    id,
    projectId: PROJECT_A.id,
    sender: from === 'asker' ? `agent:${ASKER_NAME}` : `user:${senderId}`,
    senderId,
    recipient: from === 'asker' ? 'thread' : `agent:${ASKER_NAME}`,
    recipientId: GENERAL_THREAD_ID,
    msg: body,
    type,
    agentId: ASKER_ID,
    threadId: GENERAL_THREAD_ID,
    createdAt: at(minutes),
    ...extra,
  };
}

// ---------------------------------------------------------------------------
// Decisions of the mocks
// ---------------------------------------------------------------------------

const YES_NO = [
  { id: 'yes', label: 'Yes' },
  { id: 'no', label: 'No' },
];

/** Mock s1: "Ship `v0.9` today?" with Yes (recommended) / No / Wait for #812. */
export function shipDecision(messageId: string, extra: Partial<DecisionOpts> = {}): Json {
  return decision({
    messageId,
    rowId: `${messageId}-d234`,
    key: 'D234',
    question: 'Ship `v0.9` today?',
    defaultChoiceId: 'yes',
    choices: [...YES_NO, { id: 'wait', label: 'Wait for #812' }],
    ...extra,
  });
}

/** Mock s4: the three decisions of a set; 17 has no recommendation. */
export function setDecisions(
  messageId: string,
  opts: { descriptions?: boolean; overrides?: Record<string, Partial<DecisionOpts>> } = {}
): Json[] {
  const o = opts.overrides ?? {};
  return [
    decision({
      messageId,
      rowId: `${messageId}-d234`,
      key: 'D234',
      question: 'Ship `v0.9` today?',
      defaultChoiceId: 'yes',
      choices: YES_NO,
      ...o.D234,
    }),
    decision({
      messageId,
      rowId: `${messageId}-17`,
      key: '17',
      question: 'Which approach?',
      choices: opts.descriptions
        ? [
            {
              id: 'a',
              label: 'Extend input-needed',
              description: 'Add a `timeout` field. ~1 day.',
            },
            {
              id: 'b',
              label: 'New waiting state',
              description: 'New state machine entry. ~3 days.',
            },
          ]
        : [
            { id: 'a', label: 'a · Extend input-needed' },
            { id: 'b', label: 'b · New waiting state' },
          ],
      ...o['17'],
    }),
    decision({
      messageId,
      rowId: `${messageId}-ship-now`,
      key: 'ship-now',
      question: 'Deploy to prod after tagging?',
      defaultChoiceId: 'no',
      choices: YES_NO,
      ...o['ship-now'],
    }),
  ];
}

// ---------------------------------------------------------------------------
// Threads
// ---------------------------------------------------------------------------

/** One single-decision prompt. */
export function singleThread(
  d: Json,
  intro = 'The release branch is green. One call before I tag it.'
): ChoiceThread {
  const id = d.messageId as string;
  return {
    messages: [
      row(id, 0, 'choice-prompt', `${intro}\n\n**D234** Ship v0.9 today? (fallback)`, 'asker'),
    ],
    messageDecisions: { [id]: [d] },
    messageChoiceIntros: { [id]: intro },
    messageChoiceAnswerModes: { [id]: 'single' },
  };
}

/** One set prompt. */
export function setThread(
  id: string,
  decisions: Json[],
  intro = 'Three calls before the release. Answer them together.'
): ChoiceThread {
  return {
    messages: [row(id, 0, 'choice-prompt', `${intro}\n\n(fallback)`, 'asker')],
    messageDecisions: { [id]: decisions },
    messageChoiceIntros: { [id]: intro },
    messageChoiceAnswerModes: { [id]: 'set' },
  };
}

/** Mock s9: v1 replaced by v2, both in the thread. */
export function replacedThread(): ChoiceThread {
  const choices = [
    { id: 'extend', label: 'Extend input-needed' },
    { id: 'waiting', label: 'New waiting state' },
  ];
  const v1 = decision({
    messageId: 'prompt-v1',
    rowId: 'row-17-v1',
    key: '17',
    question: 'Which approach?',
    choices,
    status: 'replaced',
    replacedBy: { rowId: 'row-17-v2', messageId: 'prompt-v2', version: 2, at: at(2) },
  });
  const v2 = decision({
    messageId: 'prompt-v2',
    rowId: 'row-17-v2',
    key: '17',
    question: 'Which approach?',
    version: 2,
    defaultChoiceId: 'hybrid',
    choices: [...choices, { id: 'hybrid', label: 'Hybrid' }],
  });
  return {
    messages: [
      row('prompt-v1', 0, 'choice-prompt', 'Which approach? (fallback)', 'asker'),
      row(
        'prompt-v2',
        2,
        'choice-prompt',
        'Updated: added the hybrid option.\n\n(fallback)',
        'asker'
      ),
    ],
    messageDecisions: { 'prompt-v1': [v1], 'prompt-v2': [v2] },
    messageChoiceIntros: { 'prompt-v1': '', 'prompt-v2': 'Updated: added the hybrid option.' },
    messageChoiceAnswerModes: { 'prompt-v1': 'single', 'prompt-v2': 'single' },
  };
}

/** Mock s12: single (own, Bob's), set (own), and an undelivered single. */
export function responsesThread(): ChoiceThread {
  const single = (id: string, label: string, key: string, q: string, choiceId: string) => ({
    rowId: `${id}-row`,
    ref: key,
    key,
    version: 1,
    question: q,
    choiceId,
    label,
    provenance: 'direct',
    promptMessageId: 'prompt-resp',
  });
  const set = {
    promptMessageId: 'prompt-resp',
    batchId: 'batch-1',
    answers: [
      { key: 'D234', label: 'Yes', matchedDefault: true, choiceId: 'yes' },
      { key: '17', label: 'a · Extend input-needed', matchedDefault: false, choiceId: 'a' },
      { key: 'ship-now', label: 'Yes', matchedDefault: false, choiceId: 'yes' },
    ].map((a, i) => ({
      rowId: `set-row-${i}`,
      ref: a.key,
      key: a.key,
      version: 1,
      question: 'q',
      choiceId: a.choiceId,
      label: a.label,
      provenance: 'direct',
      outcome: 'answer',
      matchedDefault: a.matchedDefault,
    })),
  };
  const prompt = shipDecision('prompt-resp', {
    status: 'decided',
    decision: answer('yes', 'Yes', SELF_ID, 'Fixture User', 1),
  });
  return {
    messages: [
      row('prompt-resp', 0, 'choice-prompt', 'One call before I tag it.\n\n(fallback)', 'asker'),
      row('resp-own', 1, 'choice-response', 'I chose Yes for D234.', 'self'),
      row('resp-bob', 2, 'choice-response', 'Bob chose New waiting state for 17.', 'bob'),
      row('resp-set', 3, 'choice-response', 'I answered 3 decisions.', 'self'),
      row('resp-failed', 29, 'choice-response', 'I chose Yes for D234.', 'self', {
        dispatchState: 'failed',
        dispatchFailureReason: 'agent gone',
      }),
    ],
    messageDecisions: { 'prompt-resp': [prompt] },
    messageChoiceIntros: { 'prompt-resp': 'One call before I tag it.' },
    messageChoiceAnswerModes: { 'prompt-resp': 'single' },
    messageChoiceResponses: {
      'resp-own': single('own', 'Yes', 'D234', 'Ship v0.9 today?', 'yes'),
      'resp-bob': single('bob', 'New waiting state', '17', 'Which approach?', 'b'),
      'resp-failed': single('failed', 'Yes', 'D234', '', 'yes'),
    },
    messageChoiceResponseSets: { 'resp-set': set },
  };
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

/** How the asker lookup (GET /api/v1/agents/{id}) answers. */
export type AskerLookup = 'ok' | 'denied' | 'gone';

/**
 * Serves `thread` as the #general history, the asker and Bob as members,
 * and the asker lookup. Register after setupChatMobileMocks so these win.
 */
export async function routeChoiceThread(
  page: Page,
  thread: ChoiceThread,
  asker: AskerLookup = 'ok'
): Promise<void> {
  await page.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/members/, (route) =>
    route.fulfill({
      json: {
        humans: [
          { id: SELF_ID, kind: 'user', displayName: 'Fixture User', email: 'fixture@example.test' },
          { id: BOB_ID, kind: 'user', displayName: 'Bob', email: 'bob@example.test' },
        ],
        agents: [{ id: ASKER_ID, kind: 'agent', displayName: ASKER_NAME, slug: ASKER_NAME }],
      },
    })
  );
  await page.route(/\/api\/v1\/chat\/conversations\/([^/?]+)\/messages/, (route) => {
    const url = route.request().url();
    if (route.request().method() !== 'GET' || !url.includes(`/${GENERAL_THREAD_ID}/`)) {
      return route.fulfill({ json: { items: [], messages: [] } });
    }
    const { messages, ...maps } = thread;
    return route.fulfill({ json: { items: messages, messages, canPost: true, ...maps } });
  });
  await page.route(new RegExp(`/api/v1/agents/${ASKER_ID}(\\?|$)`), (route: Route) => {
    if (asker === 'gone') {
      return route.fulfill({
        status: 404,
        json: { error: { code: 'not_found', message: 'agent not found' } },
      });
    }
    return route.fulfill({
      json: {
        id: ASKER_ID,
        name: ASKER_NAME,
        slug: ASKER_NAME,
        projectId: PROJECT_A.id,
        _messageability:
          asker === 'denied'
            ? { canMessage: false, reason: 'mode_lineage_no_ancestry' }
            : { canMessage: true },
      },
    });
  });
}
