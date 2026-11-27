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
import type { Agent, Project } from '../../shared/types.js';
import type { ScionPageAgentDetail } from './agent-detail.js';
import {
  resetServerFlagStateForTests,
  setFeatureFlag,
  TOKENOMICS_TRAJECTORY_CAPTURE_FLAG,
} from '../../utils/feature-flags.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

function makeAgent(): Agent {
  return {
    id: 'agent-traj-1',
    name: 'claude-worker',
    projectId: 'proj-1',
    template: 'default',
    phase: 'running',
    created: '2026-11-27T00:00:00Z',
    updated: '2026-11-27T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read'] },
  } as Agent;
}

function makeProject(actions: string[]): Project {
  return {
    id: 'proj-1',
    name: 'Test Project',
    slug: 'test-project',
    created: '2026-11-27T00:00:00Z',
    updated: '2026-11-27T00:00:00Z',
    _capabilities: { actions },
  } as Project;
}

describe('agent detail Trajectory tab visibility gate', () => {
  let container: HTMLElement;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  beforeEach(() => {
    container = document.createElement('div');
    document.body.appendChild(container);
    fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          agent_id: 'agent-traj-1',
          project_id: 'proj-1',
          segments: [],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    vi.stubGlobal('fetch', fetchMock);
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    container.remove();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function mountWithLoadedAgent(projectActions: string[]): Promise<ScionPageAgentDetail> {
    const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
    el.agentId = 'agent-traj-1';
    vi.spyOn(el as unknown as { loadData: () => Promise<void> }, 'loadData').mockResolvedValue();
    container.appendChild(el);
    const priv = el as unknown as {
      loading: boolean;
      agent: Agent | null;
      project: Project | null;
    };
    priv.loading = false;
    priv.agent = makeAgent();
    priv.project = makeProject(projectActions);
    await el.updateComplete;
    return el;
  }

  it('hides Trajectory tab when tokenomics.trajectory_capture is off even with project.trajectory.read', async () => {
    setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
    const el = await mountWithLoadedAgent(['read', 'trajectory_read']);

    expect(el.shadowRoot!.querySelector('[data-testid="agent-trajectory-tab"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('[data-testid="agent-trajectory-panel"]')).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('hides Trajectory tab when experiment is on but user lacks project.trajectory.read', async () => {
    setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
    const el = await mountWithLoadedAgent(['read', 'update']);

    expect(el.shadowRoot!.querySelector('[data-testid="agent-trajectory-tab"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('[data-testid="agent-trajectory-panel"]')).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shows Trajectory tab and lazy-loads segments on tab show when experiment is on and user has project.trajectory.read', async () => {
    setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
    const el = await mountWithLoadedAgent(['read', 'trajectory_read']);

    const tab = el.shadowRoot!.querySelector('[data-testid="agent-trajectory-tab"]');
    const panel = el.shadowRoot!.querySelector('[data-testid="agent-trajectory-panel"]');
    expect(tab?.textContent?.trim()).toBe('Trajectory');
    expect(panel).not.toBeNull();
    expect(panel?.querySelector('scion-agent-trajectory-panel')?.getAttribute('agentId')).toBe(
      'agent-traj-1'
    );

    // Lazy load: no fetch before the Trajectory tab is shown.
    expect(fetchMock).not.toHaveBeenCalled();

    const tabGroup = el.shadowRoot!.querySelector('sl-tab-group');
    tabGroup?.dispatchEvent(
      new CustomEvent('sl-tab-show', {
        detail: { name: 'trajectory' },
        bubbles: true,
        composed: true,
      })
    );
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/v1/agents/agent-traj-1/trajectory/segments',
        expect.any(Object)
      );
    });
  });
});
