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
import { render } from 'lit';
import {
  ACTIVITY_DISPLAY,
  ACTIVITY_TOOLTIPS,
  activityTooltip,
  agentReportedStatusHint,
  agentStatusBadge,
  agentStatusTooltip,
  harnessErrorHint,
  isProvisionedOnly,
  STALLED_NO_HOOK_ACTIVITY_HINT,
  stalledHint,
  stateLabel,
  suspendReasonHint,
  suspendedStatusLabel,
} from './agent-state-display.js';
import type { Agent } from './types.js';

describe('stateLabel', () => {
  it("shows the 'blocked' activity as 'waiting on others' (ptone/scion#1571)", () => {
    expect(stateLabel('blocked')).toBe('waiting on others');
  });

  it('keeps the blocked icon, variant and no pulse', () => {
    expect(ACTIVITY_DISPLAY.blocked).toEqual({
      emoji: '🕓',
      icon: 'clock-history',
      variant: 'neutral',
      pulse: false,
      label: 'waiting on others',
    });
  });

  it("shows the 'waiting_for_input' activity as 'waiting on parent' (ptone/scion#3301)", () => {
    expect(stateLabel('waiting_for_input')).toBe('waiting on parent');
    expect(ACTIVITY_DISPLAY.waiting_for_input.label).toBe('waiting on parent');
  });

  it('uses other defined display labels', () => {
    expect(stateLabel('limits_exceeded')).toBe('limits exceeded');
  });

  it('falls back to the raw value for states without a label', () => {
    expect(stateLabel('thinking')).toBe('thinking');
    expect(stateLabel('running')).toBe('running');
    expect(stateLabel('something-new')).toBe('something-new');
  });
});

describe('provision-only status (ptone/scion#2929)', () => {
  const po = { name: 'po-agent', phase: 'created', provisionedOnly: true } as const;

  it('needs both the hub flag and phase created', () => {
    expect(isProvisionedOnly(po)).toBe(true);
    expect(isProvisionedOnly({ phase: 'created' })).toBe(false);
    expect(isProvisionedOnly({ phase: 'created', provisionedOnly: false })).toBe(false);
    // A stale flag after an SSE delta moved the agent on.
    expect(isProvisionedOnly({ phase: 'provisioning', provisionedOnly: true })).toBe(false);
  });

  const badge = (agent: Partial<Agent>, opts?: Parameters<typeof agentStatusBadge>[1]) => {
    const host = document.createElement('div');
    render(agentStatusBadge(agent as Agent, opts), host);
    return host.querySelector('scion-status-badge')!;
  };

  it('labels the status badge with the CLI wording and a start hint', () => {
    const el = badge(po, { size: 'small' });
    expect(el.getAttribute('status')).toBe('created');
    expect(el.getAttribute('label')).toBe('created (not started)');
    expect(el.getAttribute('tooltip')).toContain('scion start po-agent');
    expect(el.hasAttribute('title')).toBe(false);
    expect(el.getAttribute('size')).toBe('small');
  });

  it('renders other agents unchanged', () => {
    const plain = badge({ ...po, provisionedOnly: false });
    expect(plain.getAttribute('label')).toBe(stateLabel('created'));
    expect(plain.hasAttribute('tooltip')).toBe(false);
    expect(plain.hasAttribute('title')).toBe(false);
    expect(plain.hasAttribute('size')).toBe(false);
    const busy = badge({ name: 'a', phase: 'running', activity: 'blocked' });
    expect(busy.getAttribute('status')).toBe('blocked');
    expect(busy.getAttribute('label')).toBe('waiting on others');
    const custom = badge({ name: 'a', phase: 'running' }, { status: 'running', label: 'running' });
    expect(custom.getAttribute('label')).toBe('running');
  });

  it('renders suspended · <activity> label for retained suspend activities while keeping status=suspended', () => {
    expect(suspendedStatusLabel({ phase: 'suspended', activity: 'completed' })).toBe(
      'suspended · completed'
    );
    expect(suspendedStatusLabel({ phase: 'suspended', activity: 'idle' })).toBe('suspended · idle');
    expect(suspendedStatusLabel({ phase: 'suspended', activity: 'harness_error' })).toBe(
      'suspended · harness error'
    );
    expect(suspendedStatusLabel({ phase: 'suspended', activity: 'HARNESS_ERROR' })).toBe(
      'suspended · harness error'
    );
    expect(suspendedStatusLabel({ phase: 'suspended', activity: '' })).toBeUndefined();
    expect(suspendedStatusLabel({ phase: 'suspended', activity: 'blocked' })).toBeUndefined();
    expect(suspendedStatusLabel({ phase: 'running', activity: 'completed' })).toBeUndefined();

    const done = badge({ name: 'a', phase: 'suspended', activity: 'completed' });
    expect(done.getAttribute('status')).toBe('suspended');
    expect(done.getAttribute('label')).toBe('suspended · completed');

    const idle = badge({
      name: 'a',
      phase: 'suspended',
      activity: 'idle',
    });
    expect(idle.getAttribute('status')).toBe('suspended');
    expect(idle.getAttribute('label')).toBe('suspended · idle');

    const suspHarnessErr = badge({
      name: 'a',
      phase: 'suspended',
      activity: 'harness_error',
      detail: { message: 'model API: API Error: 400 denied' },
    });
    expect(suspHarnessErr.getAttribute('status')).toBe('suspended');
    expect(suspHarnessErr.getAttribute('label')).toBe('suspended · harness error');
    expect(suspHarnessErr.getAttribute('tooltip')).toBe('model API: API Error: 400 denied');

    const plain = badge({ name: 'a', phase: 'suspended' });
    expect(plain.getAttribute('status')).toBe('suspended');
    expect(plain.getAttribute('label')).toBe('suspended');
  });
});

describe('harness_error status (ptone/scion#7858)', () => {
  const badge = (agent: Partial<Agent>): Element => {
    const host = document.createElement('div');
    render(agentStatusBadge(agent as Agent), host);
    return host.querySelector('scion-status-badge')!;
  };

  it('is an error-coloured badge labelled harness error', () => {
    expect(ACTIVITY_DISPLAY.harness_error.variant).toBe('danger');
    expect(stateLabel('harness_error')).toBe('harness error');
  });

  it('shows the reason as the badge tooltip', () => {
    const el = badge({
      name: 'a',
      phase: 'running',
      activity: 'harness_error',
      detail: { message: 'model API: API Error: 400 denied' },
    });
    expect(el.getAttribute('status')).toBe('harness_error');
    expect(el.getAttribute('label')).toBe('harness error');
    expect(el.getAttribute('tooltip')).toBe('model API: API Error: 400 denied');
  });

  it('falls back to the top-level message, and has no tooltip without one', () => {
    expect(harnessErrorHint({ message: 'model API: 401' }, 'harness_error')).toBe('model API: 401');
    expect(harnessErrorHint({}, 'harness_error')).toBeUndefined();
  });

  it('adds no tooltip to other statuses', () => {
    expect(harnessErrorHint({ detail: { message: 'Ready' } }, 'working')).toBeUndefined();
    const el = badge({
      name: 'a',
      phase: 'running',
      activity: 'working',
      detail: { message: 'Ready' },
    });
    expect(el.hasAttribute('tooltip')).toBe(false);
    expect(el.hasAttribute('title')).toBe(false);
  });
});

describe('stalled no-hook-since-start hint (ptone/scion#4556)', () => {
  const badge = (agent: Partial<Agent>): Element => {
    const host = document.createElement('div');
    render(agentStatusBadge(agent as Agent), host);
    return host.querySelector('scion-status-badge')!;
  };

  it('shows the stalled explanation and no-hook-since-start hint as the badge tooltip when stalled', () => {
    const el = badge({
      name: 'a',
      phase: 'running',
      activity: 'stalled',
      detail: { message: STALLED_NO_HOOK_ACTIVITY_HINT },
    });
    expect(el.getAttribute('status')).toBe('stalled');
    expect(el.getAttribute('label')).toBe('stalled');
    expect(el.getAttribute('tooltip')).toBe(
      `${ACTIVITY_TOOLTIPS.stalled}\n${STALLED_NO_HOOK_ACTIVITY_HINT}`
    );
    expect(stalledHint({ message: STALLED_NO_HOOK_ACTIVITY_HINT }, 'stalled')).toBe(
      STALLED_NO_HOOK_ACTIVITY_HINT
    );
  });

  it('does not show stale pre-stall messages on stalled badges or non-stalled statuses', () => {
    expect(stalledHint({ detail: { message: 'Ready' } }, 'stalled')).toBeUndefined();
    expect(stalledHint({ detail: { message: 'Harness started' } }, 'stalled')).toBeUndefined();
    expect(
      stalledHint({ detail: { message: STALLED_NO_HOOK_ACTIVITY_HINT } }, 'working')
    ).toBeUndefined();

    const el = badge({
      name: 'a',
      phase: 'running',
      activity: 'stalled',
      detail: { message: 'Ready' },
    });
    expect(el.getAttribute('tooltip')).toBe(ACTIVITY_TOOLTIPS.stalled);
  });
});

describe('idle status (ptone/scion#7826)', () => {
  const badge = (agent: Partial<Agent>): Element => {
    const host = document.createElement('div');
    render(agentStatusBadge(agent as Agent), host);
    return host.querySelector('scion-status-badge')!;
  };

  it('is a neutral badge, not a warning, with no pulse', () => {
    expect(ACTIVITY_DISPLAY.idle).toEqual({
      emoji: '💤',
      icon: 'moon',
      variant: 'neutral',
      pulse: false,
    });
    expect(stateLabel('idle')).toBe('idle');
  });

  it('renders a running idle agent as idle with the idle tooltip', () => {
    const el = badge({ name: 'a', phase: 'running', activity: 'idle' });
    expect(el.getAttribute('status')).toBe('idle');
    expect(el.getAttribute('label')).toBe('idle');
    expect(el.getAttribute('tooltip')).toBe(ACTIVITY_TOOLTIPS.idle);
  });
});

describe('suspend reason (ptone/scion#7826)', () => {
  const badge = (agent: Partial<Agent>): Element => {
    const host = document.createElement('div');
    render(agentStatusBadge(agent as Agent), host);
    return host.querySelector('scion-status-badge')!;
  };

  it('appends the reason in parentheses, matching scion list', () => {
    expect(
      suspendedStatusLabel({ phase: 'suspended', activity: 'completed', suspendReason: 'idle' })
    ).toBe('suspended · completed (idle)');
    expect(
      suspendedStatusLabel({ phase: 'suspended', activity: 'idle', suspendReason: 'idle' })
    ).toBe('suspended · idle (idle)');
    expect(suspendedStatusLabel({ phase: 'suspended', suspendReason: 'user' })).toBe(
      'suspended (user)'
    );
    expect(
      suspendedStatusLabel({ phase: 'suspended', activity: 'blocked', suspendReason: 'stalled' })
    ).toBe('suspended (stalled)');
    expect(
      suspendedStatusLabel({ phase: 'suspended', activity: 'harness_error', suspendReason: 'user' })
    ).toBe('suspended · harness error (user)');
  });

  it('shows no suffix for a missing or unknown reason, or an agent that is not suspended', () => {
    expect(suspendedStatusLabel({ phase: 'suspended', suspendReason: '' })).toBeUndefined();
    expect(
      suspendedStatusLabel({ phase: 'suspended', suspendReason: 'max_lifetime' })
    ).toBeUndefined();
    expect(
      suspendedStatusLabel({ phase: 'suspended', activity: 'completed', suspendReason: 'later' })
    ).toBe('suspended · completed');
    // A stale reason on a resumed agent is never shown.
    expect(
      suspendedStatusLabel({ phase: 'running', activity: 'idle', suspendReason: 'idle' })
    ).toBeUndefined();
    expect(suspendReasonHint({ phase: 'running', suspendReason: 'idle' })).toBeUndefined();
    expect(suspendReasonHint({ phase: 'suspended' })).toBeUndefined();
  });

  it('explains each reason', () => {
    expect(suspendReasonHint({ phase: 'suspended', suspendReason: 'user' })).toMatch(/resume/);
    expect(suspendReasonHint({ phase: 'suspended', suspendReason: 'idle' })).toMatch(/wakes it/);
    expect(suspendReasonHint({ phase: 'suspended', suspendReason: 'stalled' })).toMatch(
      /auto-wake/
    );
  });

  it('labels the badge and explains the reason in its tooltip', () => {
    const el = badge({
      name: 'a',
      phase: 'suspended',
      activity: 'completed',
      suspendReason: 'idle',
    });
    expect(el.getAttribute('status')).toBe('suspended');
    expect(el.getAttribute('label')).toBe('suspended · completed (idle)');
    expect(el.getAttribute('tooltip')).toBe(
      suspendReasonHint({ phase: 'suspended', suspendReason: 'idle' })
    );

    const user = badge({ name: 'a', phase: 'suspended', suspendReason: 'user' });
    expect(user.getAttribute('label')).toBe('suspended (user)');
  });

  it('joins a retained harness error reason and the suspend reason in the tooltip', () => {
    const el = badge({
      name: 'a',
      phase: 'suspended',
      activity: 'harness_error',
      suspendReason: 'user',
      detail: { message: 'model API: 401' },
    });
    expect(el.getAttribute('tooltip')).toBe(
      `model API: 401\n${suspendReasonHint({ phase: 'suspended', suspendReason: 'user' })!}`
    );
  });

  it('keeps an explicit label', () => {
    const host = document.createElement('div');
    const agent = { name: 'a', phase: 'suspended', activity: 'completed', suspendReason: 'idle' };
    render(agentStatusBadge(agent as Agent, { status: 'suspended', label: 'suspended' }), host);
    expect(host.querySelector('scion-status-badge')!.getAttribute('label')).toBe('suspended');
  });
});

describe('status badge tooltips and agent-reported status (ptone/scion#8723)', () => {
  const badge = (agent: Partial<Agent>, opts?: Parameters<typeof agentStatusBadge>[1]): Element => {
    const host = document.createElement('div');
    render(agentStatusBadge(agent as Agent, opts), host);
    return host.querySelector('scion-status-badge')!;
  };

  it('provides one-line explanations for completed, idle, blocked, and stalled', () => {
    expect(activityTooltip('completed')).toBe(ACTIVITY_TOOLTIPS.completed);
    expect(activityTooltip('idle')).toBe(ACTIVITY_TOOLTIPS.idle);
    expect(activityTooltip('blocked')).toBe(ACTIVITY_TOOLTIPS.blocked);
    expect(activityTooltip('BLOCKED')).toBe(ACTIVITY_TOOLTIPS.blocked);
    expect(activityTooltip('stalled')).toBe(ACTIVITY_TOOLTIPS.stalled);
    expect(activityTooltip('working')).toBeUndefined();
    expect(activityTooltip('running')).toBeUndefined();
  });

  it('shows completed tooltip and appends agent-reported status from taskSummary or message', () => {
    const bare = badge({ name: 'a', phase: 'running', activity: 'completed' });
    expect(bare.getAttribute('tooltip')).toBe(ACTIVITY_TOOLTIPS.completed);

    const withTask = badge({
      name: 'a',
      phase: 'running',
      activity: 'completed',
      taskSummary: 'Opened PR #42',
      message: 'Ready',
    });
    expect(withTask.getAttribute('tooltip')).toBe(
      `${ACTIVITY_TOOLTIPS.completed}\nAgent says: Opened PR #42`
    );

    const withMessageOnly = badge({
      name: 'a',
      phase: 'running',
      activity: 'completed',
      message: 'Finished migration',
    });
    expect(withMessageOnly.getAttribute('tooltip')).toBe(
      `${ACTIVITY_TOOLTIPS.completed}\nAgent says: Finished migration`
    );
  });

  it('shows idle tooltip and appends agent-reported status message when available', () => {
    const withMsg = badge({
      name: 'a',
      phase: 'running',
      activity: 'idle',
      detail: { message: 'Between assignments' },
    });
    expect(withMsg.getAttribute('tooltip')).toBe(
      `${ACTIVITY_TOOLTIPS.idle}\nAgent says: Between assignments`
    );
  });

  it('shows waiting on others tooltip and appends agent-reported status message when available', () => {
    const bare = badge({ name: 'a', phase: 'running', activity: 'blocked' });
    expect(bare.getAttribute('label')).toBe('waiting on others');
    expect(bare.getAttribute('tooltip')).toBe(ACTIVITY_TOOLTIPS.blocked);

    const withMsg = badge({
      name: 'a',
      phase: 'running',
      activity: 'blocked',
      message: 'waiting for review from X',
    });
    expect(withMsg.getAttribute('tooltip')).toBe(
      `${ACTIVITY_TOOLTIPS.blocked}\nAgent says: waiting for review from X`
    );
  });

  it('ignores whitespace-only status messages and non-signaled states', () => {
    expect(agentReportedStatusHint({ message: '   ' }, 'blocked')).toBeUndefined();
    expect(agentReportedStatusHint({ message: 'Ready' }, 'working')).toBeUndefined();
    expect(agentReportedStatusHint({ message: 'Ready' }, 'stalled')).toBeUndefined();
    expect(
      agentStatusTooltip({ name: 'a', phase: 'running', activity: 'blocked', message: '   ' })
    ).toBe(ACTIVITY_TOOLTIPS.blocked);
  });
});
