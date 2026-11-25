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
import type { DecisionView } from '../../shared/types.js';
import type { ProjectDecisionPromptView } from '../../client/project-decisions.js';
import {
  collectWaitingOnOthersItems,
  countActionableInPrompt,
  promptHasWaitingOnOthers,
  surfaceDisplay,
} from './decisions-format.js';

function makeDecision(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'r1',
    messageId: 'm1',
    key: 'D1',
    ref: 'D1',
    version: 1,
    status: 'pending',
    askerAgentId: 'a1',
    askerAgentName: 'release-bot',
    question: 'Ship today?',
    choices: [
      { id: 'y', label: 'Yes' },
      { id: 'n', label: 'No' },
    ],
    ...over,
  };
}

function makePrompt(over: Partial<ProjectDecisionPromptView> = {}): ProjectDecisionPromptView {
  return {
    promptMessageId: 'm1',
    promptAvailable: true,
    chatKey: 'topic-1',
    conversationLabel: '#release',
    conversationAvailable: true,
    askerAgentId: 'a1',
    askerAgentName: 'release-bot',
    createdAt: '2026-11-16T09:00:00Z',
    answerMode: 'single',
    waitingOnYou: false,
    canAnswer: true,
    decisions: [makeDecision()],
    ...over,
  };
}

describe('decisions-format positive open checks', () => {
  it('excludes withdrawn, replaced, direct-decided, and unknown future statuses from Waiting on others', () => {
    const withdrawn = makeDecision({
      rowId: 'r-wd',
      status: 'withdrawn',
      withdrawn: { at: '2026-11-16T09:10:00Z', byName: 'release-bot' },
    });
    const replaced = makeDecision({ rowId: 'r-rep', status: 'replaced' });
    const directDecided = makeDecision({
      rowId: 'r-dec',
      status: 'decided',
      decision: {
        choiceId: 'y',
        label: 'Yes',
        provenance: 'direct',
        outcome: 'answer',
        answeredByUserId: 'u2',
        surface: 'native',
        responseMessageId: 'resp-1',
        at: '2026-11-16T09:05:00Z',
      },
    });
    const unknownClosed = makeDecision({ rowId: 'r-exp', status: 'expired_v99' });

    const closedPrompt = makePrompt({
      waitingOnYou: false,
      decisions: [withdrawn, replaced, directDecided, unknownClosed],
    });

    expect(promptHasWaitingOnOthers(closedPrompt)).toBe(false);
    expect(collectWaitingOnOthersItems([closedPrompt])).toEqual([]);
  });

  it('includes unconfirmed agent_interpreted rows for another user in Waiting on others', () => {
    const otherInterp = makeDecision({
      rowId: 'r-interp',
      status: 'decided',
      decision: {
        choiceId: 'y',
        label: 'Yes',
        provenance: 'agent_interpreted',
        outcome: 'answer',
        answeredByUserId: 'u-bob',
        answeredByName: 'Bob',
        surface: 'native',
        responseMessageId: 'resp-2',
        at: '2026-11-16T09:05:00Z',
      },
    });
    const withdrawn = makeDecision({
      rowId: 'r-wd',
      status: 'withdrawn',
      withdrawn: { at: '2026-11-16T09:10:00Z', byName: 'release-bot' },
    });

    const mixedPrompt = makePrompt({
      waitingOnYou: false,
      answerMode: 'set',
      decisions: [otherInterp, withdrawn],
    });

    expect(promptHasWaitingOnOthers(mixedPrompt)).toBe(true);
    const items = collectWaitingOnOthersItems([mixedPrompt]);
    expect(items).toHaveLength(1);
    expect(items[0]?.decision.rowId).toBe('r-interp');
    expect(items[0]?.reason).toBe('other_user_confirm');
  });

  it('counts only open actionable rows in countActionableInPrompt when siblings are withdrawn', () => {
    const prompt = makePrompt({
      waitingOnYou: true,
      answerMode: 'set',
      decisions: [
        makeDecision({ rowId: 'r1' }),
        makeDecision({
          rowId: 'r2',
          status: 'withdrawn',
          withdrawn: { at: '2026-11-16T09:10:00Z', byName: 'release-bot' },
        }),
      ],
    });
    expect(countActionableInPrompt(prompt, 'u-me')).toBe(1);
  });

  it('maps inbox surface to the inbox icon and label', () => {
    expect(surfaceDisplay('inbox')).toEqual({ icon: 'inbox', label: 'Inbox' });
  });
});
