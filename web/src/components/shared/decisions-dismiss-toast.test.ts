/**
 * Copyright 2026 Google LLC
 * Licensed under the Apache License, Version 2.0 (the "License");
 */

import { describe, expect, it } from 'vitest';
import { dismissResultToast } from './decisions-format.js';

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
