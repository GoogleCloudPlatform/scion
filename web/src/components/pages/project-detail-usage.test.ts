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
 * Tests for project-detail.ts canonical token total fix (§4.6) and
 * measurement_status badges gated by tokenomics.usage_ledger.
 */

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import type {
  Project,
  ProjectSessionMetricsSummary,
  ScopedUsageResponse,
} from '../../shared/types.js';
import type { ScionPageProjectDetail } from './project-detail.js';
import {
  resetServerFlagStateForTests,
  setFeatureFlag,
  TOKENOMICS_USAGE_LEDGER_FLAG,
} from '../../utils/feature-flags.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

function makeProject(): Project {
  return {
    id: 'proj-usage-1',
    name: 'Tokenomics Project',
    slug: 'tokenomics-project',
    created: '2026-11-20T00:00:00Z',
    updated: '2026-11-26T00:00:00Z',
    _capabilities: { actions: ['read'] },
  } as Project;
}

function makeSessionSummary(): ProjectSessionMetricsSummary {
  return {
    totalSessions: 4,
    totalTokensInput: 10_000,
    totalTokensOutput: 5_000,
    activeAgents: 2,
  } as ProjectSessionMetricsSummary;
}

function makeProjectUsage(overrides: Partial<ScopedUsageResponse> = {}): ScopedUsageResponse {
  return {
    scope: { kind: 'project', id: 'proj-usage-1' },
    window: {
      since: '2026-10-28T00:00:00Z',
      until: '2026-11-27T00:00:00Z',
      granularity: 'day',
    },
    calls: 12,
    error_calls: 0,
    events: 12,
    null_token_events: 0,
    tokens: {
      input: 10_000,
      output: 5_000,
      cache_read: 40_000,
      cache_write: 5_000,
      cache_write_1h: null,
      reasoning: null,
      summable_tokens: 60_000,
    },
    truncated: false,
    measurement_status: 'measured',
    unmeasured_agents: 0,
    stale_agents: 0,
    as_of: '2026-11-27T00:00:00Z',
    last_receipt_at: '2026-11-26T23:55:00Z',
    ...overrides,
  };
}

describe('project-detail token total fix & measurement badges', () => {
  let container: HTMLElement;
  let fetchUrls: string[];
  let usageResponse: ScopedUsageResponse | null;
  let metricsSummaryResponse: Record<string, unknown>;

  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    container = document.createElement('div');
    document.body.appendChild(container);
    fetchUrls = [];
    usageResponse = makeProjectUsage();
    metricsSummaryResponse = {
      source: 'hub_rollups',
      granularity: 'hour',
      sessionsCount24h: 2,
      apiCalls24h: 6,
      tokenUsage24h: 25_000,
      activeAgents24h: 1,
      periodLabel: 'Last 24 hours',
      tokens: {
        input: 5_000,
        output: 2_000,
        cacheRead: 16_000,
        cacheWrite: 2_000,
        cacheWrite1h: null,
        reasoning: null,
        summableTokens: 25_000,
        cacheHitRate: 0.6957,
      },
      cacheHitRate: 0.6957,
      measurement: {
        status: 'measured',
        unmeasuredAgents: 0,
        staleAgents: 0,
      },
    };

    vi.stubGlobal(
      'EventSource',
      class {
        addEventListener(): void {}
        removeEventListener(): void {}
        close(): void {}
      }
    );

    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        fetchUrls.push(url.pathname);

        if (url.pathname.endsWith('/usage')) {
          if (!usageResponse) {
            return Promise.resolve(new Response('Not Found', { status: 404 }));
          }
          return Promise.resolve(
            new Response(JSON.stringify(usageResponse), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        if (url.pathname.endsWith('/metrics-summary')) {
          return Promise.resolve(
            new Response(JSON.stringify(metricsSummaryResponse), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        if (url.pathname.endsWith('/metrics/summary')) {
          return Promise.resolve(
            new Response(JSON.stringify(makeSessionSummary()), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ agents: [], totalCount: 0 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

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

  async function mountProjectDetail(): Promise<ScionPageProjectDetail> {
    const el = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    el.projectId = 'proj-usage-1';
    vi.spyOn(el as unknown as { loadData: () => Promise<void> }, 'loadData').mockResolvedValue();
    container.appendChild(el);
    const priv = el as unknown as {
      loading: boolean;
      project: Project | null;
      loadMetricsSummary: () => Promise<void>;
      loadSessionMetricsSummary: () => Promise<void>;
      loadProjectUsage: () => Promise<void>;
      usageLedgerEnabled: boolean;
    };
    priv.loading = false;
    priv.project = makeProject();
    await priv.loadMetricsSummary();
    await priv.loadSessionMetricsSummary();
    if (priv.usageLedgerEnabled) {
      await priv.loadProjectUsage();
    }
    await el.updateComplete;
    return el;
  }

  it('keeps legacy input+output sum and skips /usage fetch when tokenomics.usage_ledger is off', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, false);
    const el = await mountProjectDetail();

    expect(fetchUrls).not.toContain('/api/v1/projects/proj-usage-1/usage');
    expect(
      el.shadowRoot!.querySelector('[data-testid="project-total-tokens"]')?.textContent?.trim()
    ).toBe('15.0K');
    expect(
      el.shadowRoot!.querySelector('[data-testid="project-total-tokens-measurement"]')
    ).toBeNull();
    expect(
      el.shadowRoot!.querySelector('[data-testid="project-metrics-24h-measurement"]')
    ).toBeNull();
  });

  it('uses canonical summable_tokens and renders measurement badges when tokenomics.usage_ledger is on', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    const el = await mountProjectDetail();

    expect(fetchUrls).toContain('/api/v1/projects/proj-usage-1/usage');
    // Canonical summable total is 60,000 (10K input + 5K output + 40K cache_read + 5K cache_write), not 15K.
    expect(
      el.shadowRoot!.querySelector('[data-testid="project-total-tokens"]')?.textContent?.trim()
    ).toBe('60.0K');
    expect(
      el.shadowRoot!.querySelector('[data-testid="project-tokens-24h"]')?.textContent?.trim()
    ).toBe('25.0K');

    const totalBadge = el.shadowRoot!.querySelector(
      '[data-testid="project-total-tokens-measurement"]'
    );
    expect(totalBadge?.tagName.toLowerCase()).toBe('scion-measurement-status-badge');
    expect(
      totalBadge?.shadowRoot
        ?.querySelector('[data-testid="measurement-status-pill"]')
        ?.textContent?.trim()
    ).toBe('Measured');

    const badge24h = el.shadowRoot!.querySelector(
      '[data-testid="project-metrics-24h-measurement"]'
    );
    expect(badge24h?.tagName.toLowerCase()).toBe('scion-measurement-status-badge');
  });

  it('renders em dash for null summable_tokens and marks partial coverage with ≥ and agent counts', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    usageResponse = makeProjectUsage({
      tokens: {
        input: 10_000,
        output: 5_000,
        cache_read: 20_000,
        cache_write: 5_000,
        cache_write_1h: null,
        reasoning: null,
        summable_tokens: 40_000,
      },
      measurement_status: 'stale',
      unmeasured_agents: 2,
      stale_agents: 1,
    });
    metricsSummaryResponse = {
      ...metricsSummaryResponse,
      tokenUsage24h: 0,
      tokens: {
        input: null,
        output: null,
        cacheRead: null,
        cacheWrite: null,
        cacheWrite1h: null,
        reasoning: null,
        summableTokens: null,
        cacheHitRate: null,
      },
      measurement: {
        status: 'unmeasured',
        unmeasuredAgents: 2,
        staleAgents: 0,
      },
    };

    const el = await mountProjectDetail();

    expect(
      el.shadowRoot!.querySelector('[data-testid="project-total-tokens"]')?.textContent?.trim()
    ).toBe('≥ 40.0K');
    expect(
      el.shadowRoot!.querySelector('[data-testid="project-tokens-24h"]')?.textContent?.trim()
    ).toBe('—');

    const totalBadge = el.shadowRoot!.querySelector(
      '[data-testid="project-total-tokens-measurement"]'
    );
    expect(
      totalBadge?.shadowRoot
        ?.querySelector('[data-testid="measurement-status-counts"]')
        ?.textContent?.trim()
    ).toBe('2 unmeasured · 1 stale');
  });
});
