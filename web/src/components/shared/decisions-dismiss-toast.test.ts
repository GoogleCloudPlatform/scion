/**
 * Copyright 2026 Google LLC
 * Licensed under the Apache License, Version 2.0 (the "License");
 */

import { describe, expect, it } from 'vitest';
import type { DismissDecisionPromptResult } from '../../client/decision-prompts.js';
import type { ProjectDecisionPromptView } from '../../client/project-decisions.js';
import {
  MAX_BULK_DISMISS_CONCURRENCY,
  bulkDismissResultToast,
  dismissResultToast,
  runWithConcurrencyLimit,
} from './decisions-format.js';

describe('dismissResultToast (ptone/scion#8213)', () => {
  it('claims delivery only when the hub says the agent was told', () => {
    expect(dismissResultToast('delivered', 2, 0, 'release-bot')).toEqual({
      message: 'Dismissed 2 decisions.',
      variant: 'success',
    });
    expect(dismissResultToast('delivered', 1, 0, 'release-bot')).toEqual({
      message: 'Decision dismissed.',
      variant: 'success',
    });
  });

  it('says a held response is sent when the agent can receive messages', () => {
    expect(dismissResultToast('held', 1, 0, 'release-bot')).toEqual({
      message: 'Decision dismissed. release-bot will be told when it can receive messages.',
      variant: 'success',
    });
  });

  it('says gone only for a deleted agent', () => {
    expect(dismissResultToast('gone', 1, 0, 'nc-x')).toEqual({
      message: 'nc-x is gone; nothing was sent.',
      variant: 'neutral',
    });
  });

  it('says a suspended or stopped agent was not told', () => {
    expect(dismissResultToast('unreachable', 2, 0, 'release-bot')).toEqual({
      message: 'release-bot is suspended or stopped; it was not told.',
      variant: 'warning',
    });
  });

  it('does not claim delivery it cannot confirm', () => {
    expect(dismissResultToast('failed', 1, 0, '')).toEqual({
      message: 'Decision dismissed. Could not confirm that the agent was told.',
      variant: 'warning',
    });
  });

  it('reports already-dismissed rows with no delivery claim when nothing new was dismissed', () => {
    expect(dismissResultToast('none', 0, 1, 'release-bot')).toEqual({
      message: 'Already dismissed.',
      variant: 'neutral',
    });
    expect(dismissResultToast('none', 0, 3, 'release-bot')).toEqual({
      message: '3 decisions were already dismissed.',
      variant: 'neutral',
    });
  });
});

function stubPrompt(
  id: string,
  askerAgentName = 'release-bot',
  chatKey = 'topic-release',
  conversationLabel = '#release'
): ProjectDecisionPromptView {
  return {
    promptMessageId: id,
    promptAvailable: true,
    chatKey,
    conversationLabel,
    conversationAvailable: true,
    askerAgentId: `ag-${askerAgentName}`,
    askerAgentName,
    createdAt: '2026-11-16T08:00:00Z',
    answerMode: 'single',
    waitingOnYou: true,
    canAnswer: true,
    decisions: [],
  };
}

function okResult(
  deliveryStatus: Extract<DismissDecisionPromptResult, { ok: true }>['deliveryStatus'],
  dismissedCount = 1,
  alreadyCount = 0
): DismissDecisionPromptResult {
  const row = {
    rowId: 'r-1',
    messageId: 'm-1',
    key: 'D1',
    ref: 'D1',
    version: 1,
    status: 'dismissed' as const,
    askerAgentId: 'a-1',
    askerAgentName: 'release-bot',
    question: 'Q?',
    choices: [],
  };
  return {
    ok: true,
    dismissed: Array.from({ length: dismissedCount }, (_, i) => ({ ...row, rowId: `r-${i}` })),
    alreadyDismissed: Array.from({ length: alreadyCount }, (_, i) => ({
      ...row,
      rowId: `ra-${i}`,
    })),
    responseMessageId: 'resp-1',
    delivered: deliveryStatus === 'delivered' || deliveryStatus === 'held',
    deliveryStatus,
  };
}

describe('runWithConcurrencyLimit and bulkDismissResultToast (ptone/scion#8217)', () => {
  it('caps concurrent in-flight tasks at MAX_BULK_DISMISS_CONCURRENCY (4) and preserves result order', async () => {
    expect(MAX_BULK_DISMISS_CONCURRENCY).toBe(4);
    let inFlight = 0;
    let peakInFlight = 0;
    const resolvers: Array<() => void> = [];

    const promise = runWithConcurrencyLimit(
      [0, 1, 2, 3, 4, 5, 6, 7, 8, 9],
      MAX_BULK_DISMISS_CONCURRENCY,
      async (item) => {
        inFlight++;
        peakInFlight = Math.max(peakInFlight, inFlight);
        await new Promise<void>((resolve) => {
          resolvers.push(resolve);
        });
        inFlight--;
        return item * 10;
      }
    );

    await new Promise((r) => setTimeout(r, 0));
    expect(inFlight).toBe(4);
    expect(resolvers).toHaveLength(4);

    while (resolvers.length > 0) {
      const batch = resolvers.splice(0, 2);
      for (const resolve of batch) resolve();
      await new Promise((r) => setTimeout(r, 0));
      expect(inFlight).toBeLessThanOrEqual(4);
    }

    const out = await promise;
    expect(peakInFlight).toBe(4);
    expect(out).toEqual([0, 10, 20, 30, 40, 50, 60, 70, 80, 90]);
  });

  it('summarizes all-delivered multi-card dismissals cleanly', () => {
    expect(
      bulkDismissResultToast([
        { prompt: stubPrompt('m-1'), result: okResult('delivered') },
        {
          prompt: stubPrompt('m-2', 'docs-bot', 'topic-docs', '#docs'),
          result: okResult('delivered', 2),
        },
      ])
    ).toEqual({
      message: 'Dismissed 2 cards.',
      variant: 'success',
    });
  });

  it('reports partial success and names per-card failures (changed, too_recent, and errors)', () => {
    const partialChanged = bulkDismissResultToast([
      {
        prompt: stubPrompt('m-1', 'nc-x', 'dm:ncx', 'DM with nc-x'),
        result: okResult('delivered'),
      },
      {
        prompt: stubPrompt('m-2', 'release-bot', 'topic-rel', '#release'),
        result: okResult('delivered'),
      },
      {
        prompt: stubPrompt('m-3', 'docs-bot', 'topic-docs', '#docs'),
        result: {
          ok: false,
          status: 409,
          code: 'dismiss_changed',
          message: 'changed',
          reasons: { 'r-3': 'decided' },
        },
      },
    ]);
    expect(partialChanged.variant).toBe('warning');
    expect(partialChanged.message).toContain('Dismissed 2 of 3 cards; 1 had changed');
    expect(partialChanged.message).toContain('docs-bot · #docs');

    const mixedFailures = bulkDismissResultToast([
      { prompt: stubPrompt('m-1'), result: okResult('delivered') },
      {
        prompt: stubPrompt('m-2', 'qa-bot', 'topic-qa', '#qa'),
        result: {
          ok: false,
          status: 409,
          code: 'dismiss_changed',
          message: 'changed',
          reasons: { 'r-2': 'too_recent' },
        },
      },
      {
        prompt: stubPrompt('m-3', 'ops-bot', 'topic-ops', '#ops'),
        result: {
          ok: false,
          status: 500,
          code: 'internal_error',
          message: 'Database unavailable',
        },
      },
    ]);
    expect(mixedFailures.variant).toBe('warning');
    expect(mixedFailures.message).toContain('Dismissed 1 of 3 cards');
    expect(mixedFailures.message).toContain('1 too recent (qa-bot · #qa)');
    expect(mixedFailures.message).toContain('ops-bot · #ops: Database unavailable');
  });

  it('aggregates deliveryStatus truthfully across gone, suspended/unreachable, held, failed, and none', () => {
    // All gone -> neutral, never claims sent
    const allGone = bulkDismissResultToast([
      { prompt: stubPrompt('m-1', 'nc-1'), result: okResult('gone') },
      { prompt: stubPrompt('m-2', 'nc-2'), result: okResult('gone') },
    ]);
    expect(allGone.variant).toBe('neutral');
    expect(allGone.message).toContain('Dismissed 2 cards.');
    expect(allGone.message).toContain('gone');
    expect(allGone.message).toContain('nothing was sent');

    // Mixed delivered + unreachable (suspended/stopped) -> warning, says suspended/stopped not told (not "gone")
    const withSuspended = bulkDismissResultToast([
      { prompt: stubPrompt('m-1', 'live-bot'), result: okResult('delivered') },
      { prompt: stubPrompt('m-2', 'sleep-bot'), result: okResult('unreachable') },
    ]);
    expect(withSuspended.variant).toBe('warning');
    expect(withSuspended.message).toContain('Dismissed 2 cards.');
    expect(withSuspended.message).toContain('suspended or stopped');
    expect(withSuspended.message).not.toContain('is gone');

    // Held + unconfirmed -> warning
    const heldAndFailed = bulkDismissResultToast([
      { prompt: stubPrompt('m-1', 'hold-bot'), result: okResult('held') },
      { prompt: stubPrompt('m-2', 'flaky-bot'), result: okResult('failed') },
    ]);
    expect(heldAndFailed.variant).toBe('warning');
    expect(heldAndFailed.message).toContain('receive messages');
    expect(heldAndFailed.message).toContain('confirm');

    // Already dismissed only -> neutral, no delivery claim
    const alreadyOnly = bulkDismissResultToast([
      { prompt: stubPrompt('m-1'), result: okResult('none', 0, 1) },
      { prompt: stubPrompt('m-2'), result: okResult('none', 0, 2) },
    ]);
    expect(alreadyOnly).toEqual({
      message: '2 cards were already dismissed.',
      variant: 'neutral',
    });
  });
});
