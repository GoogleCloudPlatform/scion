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

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import { render, type TemplateResult } from 'lit';
import type { Agent } from '../../shared/types.js';
import type { ScionPageAgents } from './agents.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

const NOW = new Date('2026-11-28T12:00:00Z');

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'agent-1',
    name: 'worker-1',
    projectId: 'proj-1',
    project: 'scion',
    template: 'developer',
    phase: 'running',
    activity: 'idle',
    created: '2026-11-28T10:00:00Z',
    updated: '2026-11-28T11:55:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read', 'lifecycle'] },
    ...overrides,
  };
}

describe('agents list lastReincarnatedAt display', () => {
  beforeAll(async () => {
    await import('./agents.js');
  }, 30_000);

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });

  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  function renderCard(agent: Agent): HTMLElement {
    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    const tpl = (
      el as unknown as { renderAgentCard(a: Agent, badgesOn: boolean): TemplateResult }
    ).renderAgentCard(agent, false);
    const host = document.createElement('div');
    render(tpl, host);
    return host;
  }

  function renderUpdatedCell(agent: Agent): HTMLElement {
    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    const tpl = (
      el as unknown as {
        renderAgentRowCell(
          col: { id: string },
          a: Agent,
          badgesOn: boolean,
          neverSuspendOn: boolean,
          statusVisible: boolean
        ): TemplateResult;
      }
    ).renderAgentRowCell({ id: 'updated' }, agent, false, false, true);
    const host = document.createElement('table');
    const row = document.createElement('tr');
    host.appendChild(row);
    render(tpl, row);
    return row;
  }

  it('shows Reincarnated relative time on cards only when lastReincarnatedAt is present', () => {
    const withTs = renderCard(makeAgent({ lastReincarnatedAt: '2026-11-28T11:30:00Z' }));
    const meta = withTs.querySelector('[data-testid="agent-card-reincarnated"]');
    expect(meta).not.toBeNull();
    expect(meta?.textContent).toContain('Reincarnated');
    expect(meta?.textContent).toContain('30m ago');

    const withoutTs = renderCard(makeAgent({ lastReincarnatedAt: undefined }));
    expect(withoutTs.querySelector('[data-testid="agent-card-reincarnated"]')).toBeNull();

    const zeroTs = renderCard(makeAgent({ lastReincarnatedAt: '0001-01-01T00:00:00Z' }));
    expect(zeroTs.querySelector('[data-testid="agent-card-reincarnated"]')).toBeNull();
  });

  it('shows Reincarnated relative time in table rows only when lastReincarnatedAt is present', () => {
    const withTs = renderUpdatedCell(makeAgent({ lastReincarnatedAt: '2026-11-28T11:30:00Z' }));
    const meta = withTs.querySelector('[data-testid="agent-row-reincarnated"]');
    expect(meta).not.toBeNull();
    expect(meta?.textContent).toContain('Reincarnated');
    expect(meta?.textContent).toContain('30m ago');

    const withoutTs = renderUpdatedCell(makeAgent({ lastReincarnatedAt: undefined }));
    expect(withoutTs.querySelector('[data-testid="agent-row-reincarnated"]')).toBeNull();
  });
});
