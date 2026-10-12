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
 * Pins the data-testid attributes UI automation (UAT) reads on the agent page
 * for an async launch (ptone/scion#4664). Renaming or dropping one of these
 * breaks the UAT journeys, so change them only together with those.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, nothing, type TemplateResult } from 'lit';

import type { Agent, AgentLaunch } from '../../shared/types.js';
import { agentStatusBadge } from '../../shared/agent-state-display.js';
import type { ScionPageAgentDetail } from './agent-detail.js';

// chat-thread (imported by agent-detail) pulls in the app entry point,
// which bootstraps the SPA on load; stub it as the chat tests do.
vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

function makeAgent(overrides: Partial<Agent>): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 'coder',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read', 'lifecycle', 'attach', 'delete'] },
    ...overrides,
  } as Agent;
}

const inFlight: AgentLaunch = {
  id: 'l-1',
  state: 'active',
  active: true,
  kind: 'create',
  step: 'awaiting_capacity',
};

const failed: AgentLaunch = {
  id: 'l-1',
  state: 'ended',
  active: false,
  kind: 'create',
  error: 'launch_timeout',
  endReason: 'timed_out',
};

type Renderable = TemplateResult | typeof nothing;

type Part = 'renderStateBanner' | 'renderHeader' | 'renderCurrentStateCard';

function renderPart(agent: Agent, part: Part): HTMLElement {
  const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
  el.agentId = agent.id;
  (el as unknown as { agent: Agent }).agent = agent;
  const host = document.createElement('div');
  render((el as unknown as Record<Part, (a: Agent) => Renderable>)[part](agent), host);
  return host;
}

function byId(host: HTMLElement, id: string): Element | null {
  return host.querySelector(`[data-testid="${id}"]`);
}

function text(el: Element | null): string {
  return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('agent page test ids for UAT', () => {
  // The page module graph is large; give the first import headroom.
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  it('marks the launch banner step and message', () => {
    const host = renderPart(
      makeAgent({ phase: 'provisioning', launch: inFlight, message: 'waiting for a node' }),
      'renderStateBanner'
    );

    expect(text(byId(host, 'launch-step'))).toBe('Step: awaiting capacity');
    expect(text(byId(host, 'launch-message'))).toBe('waiting for a node');
  });

  it('marks the incomplete-create panel fields and actions', () => {
    const host = renderPart(
      makeAgent({
        phase: 'error',
        message: 'launch timed out',
        launch: failed,
        appliedConfig: { task: 'fix the flaky test' },
      }),
      'renderStateBanner'
    );

    expect(text(byId(host, 'launch-error-code'))).toBe('launch_timeout — timed out');
    expect(text(byId(host, 'launch-code'))).toBe('launch_timeout');
    expect(text(byId(host, 'launch-end-reason'))).toBe('timed out');
    expect(text(byId(host, 'launch-error-message'))).toBe('launch timed out');
    expect(text(byId(host, 'incomplete-create-template'))).toBe('coder');
    expect(text(byId(host, 'incomplete-create-task'))).toBe('fix the flaky test');
    expect(text(byId(host, 'incomplete-create-delete'))).toBe('Delete');
    expect(byId(host, 'incomplete-create-recreate')?.getAttribute('href')).toBe(
      '/agents/new?projectId=p-1'
    );
  });

  it('marks the phase badge on the Status tab', () => {
    const host = renderPart(
      makeAgent({ phase: 'provisioning', launch: inFlight }),
      'renderCurrentStateCard'
    );

    expect(byId(host, 'agent-phase')?.getAttribute('status')).toBe('provisioning');
  });

  it('puts agent-phase only on the Status tab badge, not on the header badge', () => {
    const host = renderPart(makeAgent({ phase: 'provisioning', launch: inFlight }), 'renderHeader');

    expect(host.querySelector('scion-status-badge')).not.toBeNull();
    expect(byId(host, 'agent-phase')).toBeNull();
  });

  it('adds no data-testid to a status badge without a testId', () => {
    const host = document.createElement('div');
    render(agentStatusBadge(makeAgent({ phase: 'running' })), host);

    const badge = host.querySelector('scion-status-badge');
    expect(badge).not.toBeNull();
    expect(badge!.hasAttribute('data-testid')).toBe(false);
  });

  it('marks the header Start and Resume (best effort) buttons', () => {
    const host = renderPart(makeAgent({ phase: 'error' }), 'renderHeader');

    expect(text(byId(host, 'header-start'))).toBe('Start');
    expect(text(byId(host, 'header-force-resume'))).toBe('Resume (best effort)');
    expect(byId(host, 'header-resume')).toBeNull();
  });

  it('marks the header Resume button of a suspended agent', () => {
    const host = renderPart(makeAgent({ phase: 'suspended' }), 'renderHeader');

    expect(text(byId(host, 'header-resume'))).toBe('Resume');
    expect(byId(host, 'header-start')).toBeNull();
  });

  it('has no header Start or Resume for a create that did not complete', () => {
    const host = renderPart(makeAgent({ phase: 'error', launch: failed }), 'renderHeader');

    expect(byId(host, 'header-start')).toBeNull();
    expect(byId(host, 'header-resume')).toBeNull();
    expect(byId(host, 'header-force-resume')).toBeNull();
  });
});
