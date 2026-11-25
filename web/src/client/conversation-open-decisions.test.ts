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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  ConversationOpenDecisionsController,
  type ConversationOpenDecisionsResponse,
  type ConversationOpenDecisionsSnapshot,
  type ConversationOpenPromptItem,
} from './conversation-open-decisions.js';
import type { DecisionView } from '../shared/types.js';

function makePrompt(
  overrides: Partial<ConversationOpenPromptItem> = {}
): ConversationOpenPromptItem {
  return {
    promptMessageId: 'msg-1',
    promptAvailable: true,
    createdAt: '2026-11-25T10:00:00Z',
    askerAgentId: 'a1',
    askerAgentName: 'deploy-lead',
    answerMode: 'single',
    openCount: 1,
    totalCount: 1,
    question: 'Ship now?',
    waitingOnYou: true,
    awaitingConfirmation: false,
    ...overrides,
  };
}

function makeDecidedView(rowId = 'r1'): DecisionView {
  return {
    rowId,
    messageId: 'msg-1',
    askerAgentId: 'a1',
    askerAgentName: 'deploy-lead',
    key: 'k1',
    ref: 'D1',
    version: 1,
    status: 'decided',
    question: 'Ship now?',
    choices: [{ id: '1', label: 'Yes' }],
    decision: {
      choiceId: '1',
      label: 'Yes',
      matchedDefault: false,
      answeredByUserId: 'u1',
      answeredByName: 'Alice',
      provenance: 'direct',
      surface: 'native',
      responseMessageId: 'resp-1',
      at: '2026-11-25T10:05:00Z',
    },
  };
}

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
  }
}

describe('ConversationOpenDecisionsController', () => {
  let eventSource: EventTarget;
  let snapshots: ConversationOpenDecisionsSnapshot[];

  beforeEach(() => {
    vi.useFakeTimers();
    eventSource = new EventTarget();
    snapshots = [];
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('optimistically drops last open row on decision-updated and reconciles via coalesced refetch', async () => {
    let call = 0;
    const fetchFn = vi.fn((): Promise<ConversationOpenDecisionsResponse | null> => {
      call++;
      if (call === 1) {
        return Promise.resolve({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          prompts: [makePrompt({ promptMessageId: 'msg-1', openCount: 1 })],
          truncated: false,
        });
      }
      return Promise.resolve({
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        truncated: false,
      });
    });

    const ctrl = new ConversationOpenDecisionsController({
      onChange: (s) => snapshots.push(s),
      fetchFn,
      eventSource,
      coalesceMs: 250,
    });
    ctrl.setConversation('topic-1');
    ctrl.connect();
    await flushMicrotasks();

    expect(ctrl.snapshot.counts.waitingOnYou).toBe(1);
    expect(fetchFn).toHaveBeenCalledTimes(1);

    // Emit 5 rapid decision-updated events for msg-1; optimistic drop happens immediately,
    // and all 5 coalesce into 1 trailing refetch (2 requests total).
    for (let i = 0; i < 5; i++) {
      eventSource.dispatchEvent(
        new CustomEvent('decision-updated', {
          detail: {
            data: {
              messageId: 'msg-1',
              threadId: 'topic-1',
              decision: makeDecidedView(`r${i}`),
            },
          },
        })
      );
    }

    // Immediate optimistic drop before the 250 ms timer fires.
    expect(ctrl.snapshot.counts.waitingOnYou).toBe(0);
    expect(ctrl.snapshot.prompts).toHaveLength(0);
    expect(fetchFn).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(250);
    await flushMicrotasks();
    expect(fetchFn).toHaveBeenCalledTimes(2);
    expect(ctrl.snapshot.counts.waitingOnYou).toBe(0);

    ctrl.disconnect();
  });

  it('refetches on new decision-prompt message, message deletion, reconnect, and page return', async () => {
    let promptAvailable = true;
    const fetchFn = vi.fn(
      (): Promise<ConversationOpenDecisionsResponse | null> =>
        Promise.resolve({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          prompts: [makePrompt({ promptMessageId: 'msg-1', promptAvailable })],
          truncated: false,
        })
    );

    const ctrl = new ConversationOpenDecisionsController({
      onChange: (s) => snapshots.push(s),
      fetchFn,
      eventSource,
      coalesceMs: 250,
    });
    ctrl.setConversation('topic-1');
    ctrl.connect();
    await flushMicrotasks();
    expect(fetchFn).toHaveBeenCalledTimes(1);

    // New decision-prompt message in this conversation (real hub UserMessageEvent shape) triggers coalesced refetch.
    eventSource.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: {
          data: {
            threadId: 'topic-1',
            type: 'decision-prompt',
            id: 'msg-new',
            decisions: [],
          },
        },
      })
    );
    // Ordinary message in this conversation does NOT trigger refetch.
    eventSource.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { threadId: 'topic-1', type: 'text', id: 'msg-txt' } },
      })
    );
    await vi.advanceTimersByTimeAsync(250);
    await flushMicrotasks();
    expect(fetchFn).toHaveBeenCalledTimes(2);

    // Deleting a listed prompt marks it unavailable immediately and refetches.
    promptAvailable = false;
    eventSource.dispatchEvent(
      new CustomEvent('chat-message-deleted', {
        detail: { data: { messageId: 'msg-1' } },
      })
    );
    expect(ctrl.snapshot.prompts[0].promptAvailable).toBe(false);
    await vi.advanceTimersByTimeAsync(250);
    await flushMicrotasks();
    expect(fetchFn).toHaveBeenCalledTimes(3);

    // SSE reconnect refetches immediately.
    eventSource.dispatchEvent(new Event('connected'));
    await flushMicrotasks();
    expect(fetchFn).toHaveBeenCalledTimes(4);

    // Visibility change to visible refetches immediately.
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      value: 'visible',
    });
    document.dispatchEvent(new Event('visibilitychange'));
    await flushMicrotasks();
    expect(fetchFn).toHaveBeenCalledTimes(5);

    ctrl.disconnect();
  });

  it('drops stale-generation responses when switching conversations mid-fetch', async () => {
    let resolveFirst!: (v: ConversationOpenDecisionsResponse | null) => void;
    let call = 0;
    const fetchFn = vi.fn((key: string): Promise<ConversationOpenDecisionsResponse | null> => {
      call++;
      if (call === 1) {
        return new Promise((r) => {
          resolveFirst = r;
        });
      }
      return Promise.resolve({
        counts: { waitingOnYou: 0, waitingOnOthers: 1 },
        prompts: [makePrompt({ promptMessageId: `msg-${key}`, waitingOnYou: false })],
        truncated: false,
      });
    });

    const ctrl = new ConversationOpenDecisionsController({
      onChange: (s) => snapshots.push(s),
      fetchFn,
      eventSource,
    });
    ctrl.setConversation('topic-a');
    ctrl.connect();

    // Switch to topic-b before topic-a's fetch resolves.
    ctrl.setConversation('topic-b');
    await flushMicrotasks();
    expect(ctrl.snapshot.prompts.map((p) => p.promptMessageId)).toEqual(['msg-topic-b']);

    // Now resolve the stale topic-a response; it must be ignored.
    resolveFirst({
      counts: { waitingOnYou: 3, waitingOnOthers: 0 },
      prompts: [makePrompt({ promptMessageId: 'stale-a' })],
      truncated: false,
    });
    await flushMicrotasks();

    expect(ctrl.snapshot.prompts.map((p) => p.promptMessageId)).toEqual(['msg-topic-b']);
    expect(ctrl.snapshot.counts).toEqual({ waitingOnYou: 0, waitingOnOthers: 1 });

    ctrl.disconnect();
  });

  it('discards stale in-flight response after optimistic drop so dropped entry does not flicker back', async () => {
    let resolveInFlight!: (v: ConversationOpenDecisionsResponse | null) => void;
    let call = 0;
    const fetchFn = vi.fn((): Promise<ConversationOpenDecisionsResponse | null> => {
      call++;
      if (call === 1) {
        return Promise.resolve({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          prompts: [makePrompt({ promptMessageId: 'msg-1', openCount: 1 })],
          truncated: false,
        });
      }
      if (call === 2) {
        return new Promise((r) => {
          resolveInFlight = r;
        });
      }
      return Promise.resolve({
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        truncated: false,
      });
    });

    const ctrl = new ConversationOpenDecisionsController({
      onChange: (s) => snapshots.push(s),
      fetchFn,
      eventSource,
    });
    ctrl.setConversation('topic-1');
    ctrl.connect();
    await flushMicrotasks();
    expect(ctrl.snapshot.prompts).toHaveLength(1);

    // Start a second fetch (e.g., reconnect / panel open) that is still in flight.
    const inflightPromise = ctrl.refetchNow();
    expect(fetchFn).toHaveBeenCalledTimes(2);

    // While call 2 is in flight, viewer's answer arrives and drops msg-1 optimistically.
    eventSource.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: {
          data: {
            messageId: 'msg-1',
            threadId: 'topic-1',
            decision: makeDecidedView('r1'),
          },
        },
      })
    );
    expect(ctrl.snapshot.prompts).toHaveLength(0);
    const snapshotsBeforeStale = snapshots.length;

    // Now the pre-answer in-flight response resolves with msg-1 still present.
    resolveInFlight({
      counts: { waitingOnYou: 1, waitingOnOthers: 0 },
      prompts: [makePrompt({ promptMessageId: 'msg-1', openCount: 1 })],
      truncated: false,
    });
    await inflightPromise;
    await flushMicrotasks();

    // The stale response must NOT have re-inserted msg-1 at any point, and the
    // owed 3rd fetch must have run to reconcile cleanly.
    const postDropSnapshots = snapshots.slice(snapshotsBeforeStale);
    expect(postDropSnapshots.every((s) => s.prompts.length === 0)).toBe(true);
    expect(fetchFn).toHaveBeenCalledTimes(3);
    expect(ctrl.snapshot.counts).toEqual({ waitingOnYou: 0, waitingOnOthers: 0 });

    ctrl.disconnect();
  });
});
