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
import type { Agent } from '../../shared/types.js';
import type { ScionPageAgentDetail } from './agent-detail.js';
import {
  resetServerFlagStateForTests,
  setFeatureFlag,
  TOKENOMICS_USAGE_LEDGER_FLAG,
} from '../../utils/feature-flags.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

function makeAgent(): Agent {
  return {
    id: 'agent-usage-1',
    name: 'claude-worker',
    projectId: 'proj-1',
    template: 'default',
    phase: 'running',
    created: '2026-11-25T00:00:00Z',
    updated: '2026-11-25T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read'] },
  } as Agent;
}

describe('agent detail Usage tab experiment gate', () => {
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
          agent_id: 'agent-usage-1',
          project_id: 'proj-1',
          window: { since: '2026-10-26T00:00:00Z', until: '2026-11-25T18:00:00Z' },
          calls: 0,
          error_calls: 0,
          events: 0,
          null_token_events: 0,
          tokens: {
            input: null,
            output: null,
            cache_read: null,
            cache_write: null,
            cache_write_1h: null,
            reasoning: null,
            summable_tokens: null,
          },
          by_model: [],
          measurement_status: 'unmeasured',
          as_of: '2026-11-25T18:00:00Z',
          last_receipt_at: null,
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

  async function mountWithLoadedAgent(): Promise<ScionPageAgentDetail> {
    const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
    el.agentId = 'agent-usage-1';
    // Prevent connectedCallback's network load from overwriting test agent state.
    vi.spyOn(el as unknown as { loadData: () => Promise<void> }, 'loadData').mockResolvedValue();
    container.appendChild(el);
    const priv = el as unknown as { loading: boolean; agent: Agent | null };
    priv.loading = false;
    priv.agent = makeAgent();
    await el.updateComplete;
    return el;
  }

  it('hides Usage tab and panel when tokenomics.usage_ledger is off', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, false);
    const el = await mountWithLoadedAgent();

    expect(el.shadowRoot!.querySelector('[data-testid="agent-usage-tab"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('[data-testid="agent-usage-panel"]')).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders Usage tab and scion-agent-usage-panel when tokenomics.usage_ledger is on', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    const el = await mountWithLoadedAgent();

    const tab = el.shadowRoot!.querySelector('[data-testid="agent-usage-tab"]');
    const panel = el.shadowRoot!.querySelector('[data-testid="agent-usage-panel"]');
    expect(tab?.textContent?.trim()).toBe('Usage');
    expect(panel).not.toBeNull();
    expect(panel?.querySelector('scion-agent-usage-panel')?.getAttribute('agentId')).toBe(
      'agent-usage-1'
    );
  });
});
