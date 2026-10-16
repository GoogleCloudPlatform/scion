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
 * The agent page for an async launch (ptone/scion#4664).
 *
 * While the hub launches the agent the page shows the launch step and
 * message, and offers no Start. A create that did not complete shows its
 * failure code and message; the hub refuses to start or resume it (409
 * agent_create_incomplete), so Start and Resume are hidden and the page
 * offers delete and create again. An agent without a launch (a synchronous
 * create) keeps the generic error banner and its actions.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, nothing, type TemplateResult } from 'lit';

import type { Agent, AgentLaunch } from '../../shared/types.js';
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

function failedLaunch(overrides: Partial<AgentLaunch> = {}): AgentLaunch {
  return {
    id: 'l-1',
    state: 'ended',
    active: false,
    kind: 'create',
    step: 'launching',
    error: 'runtime_error',
    endReason: 'failed',
    ...overrides,
  };
}

const activeLaunch: AgentLaunch = {
  id: 'l-1',
  state: 'active',
  active: true,
  kind: 'create',
  step: 'launching',
  deadline: '2026-01-01T00:05:00Z',
  remainingSeconds: 240,
};

function page(agent: Agent): ScionPageAgentDetail {
  const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
  el.agentId = agent.id;
  (el as unknown as { agent: Agent }).agent = agent;
  return el;
}

function renderInto(tpl: TemplateResult | typeof nothing): HTMLElement {
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

/** The banner under the header, rendered on its own. */
function renderBanner(agent: Agent): HTMLElement {
  const el = page(agent) as unknown as { renderStateBanner(): TemplateResult | typeof nothing };
  return renderInto(el.renderStateBanner());
}

/** The labels of the header's buttons. */
function headerButtonLabels(agent: Agent): string[] {
  const el = page(agent) as unknown as { renderHeader(): TemplateResult };
  const host = renderInto(el.renderHeader());
  return Array.from(host.querySelectorAll('.header-actions sl-button')).map(
    (b) => b.textContent?.trim() ?? ''
  );
}

function text(el: Element | null): string {
  return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('agent page during an async launch', () => {
  // The page module graph is large; give the first import headroom.
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  it('shows the launch step and message while the hub launches the agent', () => {
    const host = renderBanner(
      makeAgent({ phase: 'provisioning', launch: activeLaunch, message: 'pulling image' })
    );

    const banner = host.querySelector('[data-testid="agent-launch-banner"]');
    expect(banner).not.toBeNull();
    expect(text(banner!.querySelector('.launch-title'))).toBe('Launching');
    expect(text(banner!.querySelector('.launch-step'))).toBe('Step: launching');
    expect(text(banner!.querySelector('.launch-message'))).toBe('pulling image');
  });

  it('shows the label of the launch step, with the raw step as data-step', () => {
    const host = renderBanner(
      makeAgent({ phase: 'provisioning', launch: { ...activeLaunch, step: 'container_create' } })
    );

    const step = host.querySelector('[data-testid="launch-step"]');
    expect(text(step)).toBe('Step: preparing the container (image pull, volume mounts)');
    expect(step?.getAttribute('data-step')).toBe('container_create');
  });

  it.each([
    ['start', 'Starting'],
    ['restart', 'Restarting'],
  ])('titles a %s launch in flight', (kind, title) => {
    const host = renderBanner(
      makeAgent({ phase: 'starting', launch: { ...activeLaunch, kind, step: 'pod_create' } })
    );

    expect(text(host.querySelector('.launch-title'))).toBe(title);
    expect(text(host.querySelector('[data-testid="launch-step"]'))).toBe('Step: creating the pod');
  });

  it('offers no Start while the hub launches the agent', () => {
    const labels = headerButtonLabels(makeAgent({ phase: 'provisioning', launch: activeLaunch }));

    expect(labels).not.toContain('Start');
    expect(labels).not.toContain('Resume (best effort)');
  });

  it('keeps Start for a provisioning agent without an async launch', () => {
    expect(headerButtonLabels(makeAgent({ phase: 'provisioning' }))).toContain('Start');
  });
});

describe('agent page for a create that did not complete', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  const failed = makeAgent({
    phase: 'error',
    message: 'launching: the runtime could not start the agent',
    launch: failedLaunch(),
    appliedConfig: { task: 'fix the flaky test' },
  });

  it('shows the failure code and message', () => {
    const host = renderBanner(failed);

    const banner = host.querySelector('[data-testid="incomplete-create-banner"]');
    expect(banner).not.toBeNull();
    expect(text(banner!.querySelector('.error-title'))).toBe('Agent create did not complete');
    expect(text(banner!.querySelector('[data-testid="launch-error-code"]'))).toBe(
      'runtime_error: the runtime could not start the agent — failed'
    );
    expect(text(banner!.querySelector('[data-testid="launch-failed-step"]'))).toBe(
      'Failed at: launching'
    );
    expect(text(banner!.querySelector('.error-message'))).toBe(
      'launching: the runtime could not start the agent'
    );
  });

  it('shows no cause line when the launch has no cause', () => {
    const host = renderBanner(failed);
    expect(host.querySelector('[data-testid="launch-error-cause"]')).toBeNull();
  });

  it('shows the runtime cause the hub reported', () => {
    const host = renderBanner(
      makeAgent({
        phase: 'error',
        message: 'launching: Failed to create agent',
        launch: failedLaunch({ cause: 'runtime_forbidden' }),
      })
    );
    expect(text(host.querySelector('[data-testid="launch-error-code"]'))).toBe(
      'runtime_error: the runtime could not start the agent — failed'
    );
    expect(text(host.querySelector('[data-testid="launch-error-cause"]'))).toBe(
      'Cause: runtime forbidden'
    );
  });

  it('labels a typed runtime failure and the step it failed in', () => {
    const host = renderBanner(
      makeAgent({
        phase: 'error',
        message: 'container_create: the agent image could not be pulled (ImagePullBackOff)',
        launch: failedLaunch({ error: 'image_pull_failed', step: 'container_create' }),
      })
    );

    expect(text(host.querySelector('[data-testid="launch-code"]'))).toBe('image_pull_failed');
    expect(text(host.querySelector('[data-testid="launch-code-label"]'))).toBe(
      'the agent image could not be pulled'
    );
    const step = host.querySelector('[data-testid="launch-failed-step"]');
    expect(text(step)).toBe('Failed at: preparing the container (image pull, volume mounts)');
    expect(step?.getAttribute('data-step')).toBe('container_create');
  });

  it('shows no failed-step line when the launch has no step', () => {
    const host = renderBanner(
      makeAgent({ phase: 'error', message: 'm', launch: failedLaunch({ step: undefined }) })
    );
    expect(host.querySelector('[data-testid="launch-failed-step"]')).toBeNull();
  });

  it('shows the template and task to create it again with', () => {
    const banner = renderBanner(failed).querySelector('.error-recreate');

    expect(text(banner)).toBe('Template coder Task fix the flaky test');
  });

  it('offers delete and create again in the banner', () => {
    const banner = renderBanner(failed).querySelector('.error-actions');

    const labels = Array.from(banner?.querySelectorAll('sl-button') ?? []).map(text);
    expect(labels).toEqual(['Delete', 'Create again']);
    expect(banner?.querySelector('a')?.getAttribute('href')).toBe('/agents/new?projectId=p-1');
  });

  it('hides Start and Resume', () => {
    const labels = headerButtonLabels(failed);

    expect(labels).not.toContain('Start');
    expect(labels).not.toContain('Resume (best effort)');
    expect(labels).not.toContain('Resume');
  });

  it('hides Resume for a suspended incomplete create', () => {
    const labels = headerButtonLabels(
      makeAgent({ phase: 'suspended', launch: failedLaunch({ error: 'launch_stopped' }) })
    );

    expect(labels).not.toContain('Resume');
  });

  // A create winding down after a failure or stop: the launch is still
  // active (state), but no longer in flight (active), and has no error yet.
  // The hub already refuses to start it ("its create is still stopping").
  const windingDown = makeAgent({
    phase: 'stopped',
    launch: { id: 'l-1', state: 'active', active: false, kind: 'create', step: 'launching' },
  });

  it('shows a create winding down as stopping', () => {
    const host = renderBanner(windingDown);

    expect(host.querySelector('[data-testid="agent-launch-banner"]')).toBeNull();
    expect(text(host.querySelector('[data-testid="launch-error-code"]'))).toBe('stopping');
  });

  it('hides Start and Resume for a create winding down', () => {
    const labels = headerButtonLabels(windingDown);

    expect(labels).not.toContain('Start');
    expect(labels).not.toContain('Resume');
  });

  it.each([
    ['timed_out', 'launch_timeout', 'launch_timeout: the launch timed out — timed out'],
    ['lost', 'broker_lost', 'broker_lost: the runtime broker stopped responding — broker lost'],
  ])('labels a launch that ended %s', (endReason, error, want) => {
    const host = renderBanner(
      makeAgent({ phase: 'error', message: 'm', launch: failedLaunch({ endReason, error }) })
    );

    expect(text(host.querySelector('[data-testid="launch-error-code"]'))).toBe(want);
  });

  // The hub omits an empty endReason (omitempty) and never sends null, but
  // the banner must still render if a null reaches it (GCP#3379 review).
  it('shows only the code when the end reason is null', () => {
    const launch = { ...failedLaunch(), endReason: null } as unknown as AgentLaunch;
    const agent = makeAgent({ phase: 'error', message: 'm', launch });

    let host: HTMLElement | undefined;
    expect(() => {
      host = renderBanner(agent);
    }).not.toThrow();
    expect(text(host!.querySelector('[data-testid="launch-error-code"]'))).toBe(
      'runtime_error: the runtime could not start the agent'
    );
  });

  it('omits the delete offer without delete permission', () => {
    const host = renderBanner({ ...failed, _capabilities: { actions: ['read', 'lifecycle'] } });

    const labels = Array.from(host.querySelectorAll('.error-actions sl-button')).map(text);
    expect(labels).toEqual(['Create again']);
  });
});

describe('agent page for an error without an async launch', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  const errored = makeAgent({ phase: 'error', message: 'container exited' });

  it('keeps the generic error banner', () => {
    const host = renderBanner(errored);

    expect(host.querySelector('[data-testid="incomplete-create-banner"]')).toBeNull();
    expect(text(host.querySelector('.error-title'))).toBe('Agent Error');
    expect(text(host.querySelector('.error-message'))).toBe('container exited');
  });

  it('keeps Resume (best effort) and Start', () => {
    const labels = headerButtonLabels(errored);

    expect(labels).toContain('Resume (best effort)');
    expect(labels).toContain('Start');
  });
});

describe('agent page for a start or restart that did not complete', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  const failedStart = failedLaunch({
    kind: 'start',
    error: 'crash_loop',
    step: 'container_start',
  });
  const message = 'container_start: the agent container keeps crashing on start (CrashLoopBackOff)';

  it('shows the failure of a start restored to stopped', () => {
    const host = renderBanner(makeAgent({ phase: 'stopped', message, launch: failedStart }));

    const banner = host.querySelector('[data-testid="failed-start-banner"]');
    expect(banner).not.toBeNull();
    expect(text(banner!.querySelector('.error-title'))).toBe('Agent start did not complete');
    expect(text(banner!.querySelector('[data-testid="launch-error-code"]'))).toBe(
      'crash_loop: the agent container keeps crashing on start — failed'
    );
    expect(text(banner!.querySelector('[data-testid="launch-failed-step"]'))).toBe(
      'Failed at: starting the container'
    );
    expect(text(banner!.querySelector('[data-testid="launch-error-message"]'))).toBe(message);
    expect(text(banner!.querySelector('.error-hint'))).toBe('Start or resume the agent to try again.');
    expect(host.querySelector('[data-testid="incomplete-create-banner"]')).toBeNull();
    expect(banner!.querySelector('.error-actions')).toBeNull();
  });

  it('shows it instead of the generic error banner in phase error', () => {
    const host = renderBanner(
      makeAgent({
        phase: 'error',
        message,
        launch: { ...failedStart, resumePhase: 'suspended' },
      })
    );

    expect(host.querySelector('[data-testid="failed-start-banner"]')).not.toBeNull();
    expect(host.querySelectorAll('.error-title')).toHaveLength(1);
    expect(text(host.querySelector('.error-title'))).toBe('Agent start did not complete');
  });

  it('titles a failed restart and shows its cause', () => {
    const host = renderBanner(
      makeAgent({
        phase: 'stopped',
        message: 'launching: Failed to create agent',
        launch: failedLaunch({ kind: 'restart', cause: 'runtime_forbidden' }),
      })
    );

    expect(text(host.querySelector('.error-title'))).toBe('Agent restart did not complete');
    expect(text(host.querySelector('[data-testid="launch-error-cause"]'))).toBe(
      'Cause: runtime forbidden'
    );
  });

  it('keeps Start, and Resume for a suspended row, in the header', () => {
    expect(headerButtonLabels(makeAgent({ phase: 'error', launch: failedStart }))).toContain(
      'Start'
    );
    expect(headerButtonLabels(makeAgent({ phase: 'stopped', launch: failedStart }))).toContain(
      'Start'
    );
    expect(headerButtonLabels(makeAgent({ phase: 'suspended', launch: failedStart }))).toContain(
      'Resume'
    );
  });

  it('shows no failure for a start the user stopped', () => {
    const host = renderBanner(
      makeAgent({
        phase: 'stopped',
        message: 'launching: launch stopped',
        launch: failedLaunch({ kind: 'start', error: 'launch_stopped', endReason: 'failed' }),
      })
    );

    expect(host.querySelector('[data-testid="failed-start-banner"]')).toBeNull();
    expect(host.querySelector('.agent-error-banner')).toBeNull();
    expect(host.textContent ?? '').not.toContain('did not complete');
  });

  it('shows no banner once the agent runs', () => {
    const host = renderBanner(makeAgent({ phase: 'running', launch: failedStart }));
    expect(host.querySelector('.agent-error-banner')).toBeNull();
  });
});
