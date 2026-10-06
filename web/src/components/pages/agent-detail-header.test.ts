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
 * Header action order on the agent detail page (#2538): the graph link is
 * the leftmost action in every phase, so it never sits between the
 * destructive Stop and Delete buttons. The other actions keep their order.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, type TemplateResult } from 'lit';

import type { Agent } from '../../shared/types.js';
import { PROVISIONED_ONLY_LABEL } from '../../shared/agent-state-display.js';
import type { ScionPageAgentDetail } from './agent-detail.js';

// chat-thread (imported by agent-detail) pulls in the app entry point,
// which bootstraps the SPA on load; stub it as the chat tests do.
vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: new EventTarget(),
}));

const GRAPH = 'graph';

function makeAgent(overrides: Partial<Agent>): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read', 'lifecycle', 'attach', 'delete'] },
    ...overrides,
  } as Agent;
}

/** Render the page header for `agent` and return its .header-actions. */
function renderHeaderActions(agent: Agent): Element {
  const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
  el.agentId = agent.id;
  (el as unknown as { agent: Agent }).agent = agent;
  const tpl = (el as unknown as { renderHeader(): TemplateResult }).renderHeader();
  const host = document.createElement('div');
  render(tpl, host);
  const actions = host.querySelector('.header-actions');
  expect(actions).not.toBeNull();
  return actions!;
}

/** Label each top-level action in .header-actions, in DOM order. */
function headerActionLabels(agent: Agent): string[] {
  return Array.from(renderHeaderActions(agent).children).map((child) => {
    const link = child.matches('a') ? child : child.querySelector(':scope > a');
    if (link?.getAttribute('href')?.startsWith('/agents/graph?')) return GRAPH;
    const button = child.matches('sl-button') ? child : child.querySelector('sl-button');
    const icon = button?.querySelector('sl-icon')?.getAttribute('name') ?? '';
    const text = button?.textContent?.trim() ?? '';
    return text || icon;
  });
}

describe('agent detail header actions order', () => {
  // The page module graph is large; give the first import headroom.
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  it('puts the graph link first for a running agent', () => {
    expect(headerActionLabels(makeAgent({ phase: 'running' }))).toEqual([
      GRAPH,
      'Message',
      'Terminal',
      'Suspend',
      'Stop',
      'trash',
    ]);
  });

  it('puts the graph link first for a suspended agent', () => {
    expect(headerActionLabels(makeAgent({ phase: 'suspended' }))).toEqual([
      GRAPH,
      'Message',
      'Terminal',
      'Resume',
      'trash',
    ]);
  });

  it('puts the graph link first for a stopped agent', () => {
    expect(headerActionLabels(makeAgent({ phase: 'stopped' }))).toEqual([
      GRAPH,
      'Message',
      'Terminal',
      'Start',
      'trash',
    ]);
  });

  it('puts the graph link first for an errored agent', () => {
    expect(headerActionLabels(makeAgent({ phase: 'error' }))).toEqual([
      GRAPH,
      'Message',
      'Terminal',
      'Resume (best effort)',
      'Start',
      'trash',
    ]);
  });

  it('puts the graph link first for a created agent', () => {
    expect(headerActionLabels(makeAgent({ phase: 'created' }))).toEqual([
      GRAPH,
      'Message',
      'Terminal',
      'Start',
      'Configure',
      'trash',
    ]);
  });

  it('keeps the graph link target and tooltip', () => {
    const first = renderHeaderActions(makeAgent({ phase: 'running' })).firstElementChild!;
    expect(first.tagName.toLowerCase()).toBe('sl-tooltip');
    expect(first.getAttribute('content')).toBe('See this agent in graph');
    expect(first.querySelector('a')!.getAttribute('href')).toBe(
      '/agents/graph?project=p-1&focus=a-1'
    );
  });

  it('URL-encodes the project and agent IDs in the graph link', () => {
    const agent = makeAgent({ id: 'a 1&x', projectId: 'p/1?y' });
    const first = renderHeaderActions(agent).firstElementChild!;
    expect(first.querySelector('a')!.getAttribute('href')).toBe(
      '/agents/graph?project=p%2F1%3Fy&focus=a%201%26x'
    );
  });

  it('puts the graph link before a disabled Message in its own tooltip', () => {
    const agent = makeAgent({
      phase: 'running',
      _messageability: { canMessage: false, canReachViewer: true, reason: 'missing_permission' },
    });
    expect(headerActionLabels(agent)).toEqual([
      GRAPH,
      'Message',
      'Terminal',
      'Suspend',
      'Stop',
      'trash',
    ]);
    const message = renderHeaderActions(agent).children[1];
    expect(message.tagName.toLowerCase()).toBe('sl-tooltip');
    expect(message.getAttribute('content')).toBeTruthy();
    expect(message.querySelector(':scope > sl-button')!.hasAttribute('disabled')).toBe(true);
  });

  // The configure page carries the Timezone row, which works in any phase,
  // so Configure shows whenever the caller may update the agent.
  it('shows Configure for a running agent with update capability', () => {
    expect(
      headerActionLabels(
        makeAgent({
          phase: 'running',
          _capabilities: { actions: ['read', 'lifecycle', 'attach', 'update', 'delete'] },
        })
      )
    ).toEqual([GRAPH, 'Message', 'Terminal', 'Suspend', 'Stop', 'Configure', 'trash']);
  });

  it.each(['stopped', 'suspended', 'error'] as const)(
    'shows Configure for a %s agent with update capability',
    (phase) => {
      const labels = headerActionLabels(
        makeAgent({ phase, _capabilities: { actions: ['read', 'lifecycle', 'update'] } })
      );
      expect(labels).toContain('Configure');
    }
  );

  it('hides Configure for a running agent without update capability', () => {
    const labels = headerActionLabels(makeAgent({ phase: 'running' }));
    expect(labels).not.toContain('Configure');
  });

  it('links Configure to the agent configure page', () => {
    const actions = renderHeaderActions(
      makeAgent({ phase: 'running', _capabilities: { actions: ['read', 'update'] } })
    );
    expect(actions.querySelector('a[href="/agents/a-1/configure"]')).not.toBeNull();
  });

  it('puts the graph link first even with no other actions permitted', () => {
    expect(
      headerActionLabels(makeAgent({ phase: 'running', _capabilities: { actions: ['read'] } }))
    ).toEqual([GRAPH]);
  });
});

/**
 * Status badges on the agent detail page (ptone/scion#2929): the header
 * and the Phase badge show a provision-only agent as created (not
 * started), and the Phase badge keeps the phase for a running agent.
 */
describe('agent detail status badges', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  function makePage(agent: Agent): ScionPageAgentDetail {
    const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
    el.agentId = agent.id;
    (el as unknown as { agent: Agent }).agent = agent;
    return el;
  }

  function renderTo(tpl: TemplateResult): HTMLElement {
    const host = document.createElement('div');
    render(tpl, host);
    return host;
  }

  /** The status badge in the page header title. */
  function headerBadge(agent: Agent): Element {
    const page = makePage(agent);
    const tpl = (page as unknown as { renderHeader(): TemplateResult }).renderHeader();
    const badge = renderTo(tpl).querySelector('.header-title > scion-status-badge');
    expect(badge).not.toBeNull();
    return badge!;
  }

  /** The status badge in the Phase item of the Current State card. */
  function phaseBadge(agent: Agent): Element {
    const page = makePage(agent);
    const tpl = (
      page as unknown as { renderCurrentStateCard(a: Agent): TemplateResult }
    ).renderCurrentStateCard(agent);
    const item = Array.from(renderTo(tpl).querySelectorAll('.info-item')).find(
      (i) => i.querySelector('.info-label')?.textContent?.trim() === 'Phase'
    );
    const badge = item?.querySelector('scion-status-badge');
    expect(badge).toBeTruthy();
    return badge!;
  }

  const provisionOnly = () => makeAgent({ phase: 'created', provisionedOnly: true });

  it('header: a provision-only agent shows the override label and start hint', () => {
    const badge = headerBadge(provisionOnly());
    expect(badge.getAttribute('label')).toBe(PROVISIONED_ONLY_LABEL);
    expect(badge.getAttribute('title')).toContain('scion start agent-1');
  });

  it('Phase badge: a provision-only agent shows the override label and start hint', () => {
    const badge = phaseBadge(provisionOnly());
    expect(badge.getAttribute('label')).toBe(PROVISIONED_ONLY_LABEL);
    expect(badge.getAttribute('title')).toContain('scion start agent-1');
  });

  it('Phase badge: a running agent keeps its phase, not its activity', () => {
    const badge = phaseBadge(makeAgent({ phase: 'running', activity: 'thinking' }));
    expect(badge.getAttribute('status')).toBe('running');
    expect(badge.getAttribute('label')).toBe('running');
    expect(badge.hasAttribute('title')).toBe(false);
  });
});
