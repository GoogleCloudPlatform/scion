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
  agentStatusBadge,
  isProvisionedOnly,
  stateLabel,
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

  it("shows the 'waiting_for_input' activity as 'waiting for user' (ptone/scion#3301)", () => {
    expect(stateLabel('waiting_for_input')).toBe('waiting for user');
    expect(ACTIVITY_DISPLAY.waiting_for_input.label).toBe('waiting for user');
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
    expect(el.getAttribute('title')).toContain('scion start po-agent');
    expect(el.getAttribute('size')).toBe('small');
  });

  it('renders other agents unchanged', () => {
    const plain = badge({ ...po, provisionedOnly: false });
    expect(plain.getAttribute('label')).toBe(stateLabel('created'));
    expect(plain.hasAttribute('title')).toBe(false);
    expect(plain.hasAttribute('size')).toBe(false);
    const busy = badge({ name: 'a', phase: 'running', activity: 'blocked' });
    expect(busy.getAttribute('status')).toBe('blocked');
    expect(busy.getAttribute('label')).toBe('waiting on others');
    const custom = badge({ name: 'a', phase: 'running' }, { status: 'running', label: 'running' });
    expect(custom.getAttribute('label')).toBe('running');
  });
});
