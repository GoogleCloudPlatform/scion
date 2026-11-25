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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { AgentUsageResponse, AgentUsageTokens } from '../../shared/types.js';
import {
  computeCacheHitRate,
  formatCacheHitRate,
  formatUsageToken,
  measurementStatusDisplay,
  ScionAgentUsagePanel,
} from './agent-usage-panel.js';
import './agent-usage-panel.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function waitForPanel(el: ScionAgentUsagePanel): Promise<void> {
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

describe('agent-usage-panel helpers', () => {
  it('formats null and undefined token counts as em dash, never 0, and marks partial counts with ≥', () => {
    expect(formatUsageToken(null)).toBe('—');
    expect(formatUsageToken(undefined)).toBe('—');
    expect(formatUsageToken(null, 2)).toBe('—');
    expect(formatUsageToken(0)).toBe('0');
    expect(formatUsageToken(18)).toBe('18');
    expect(formatUsageToken(12500)).toBe('12,500');
    expect(formatUsageToken(18, 1)).toBe('≥ 18');
  });

  it('computes cache hit rate from input, cache_read, and cache_write', () => {
    const canonical: AgentUsageTokens = {
      input: 8,
      output: 5,
      cache_read: 3,
      cache_write: 2,
      cache_write_1h: null,
      reasoning: 2,
      summable_tokens: 18,
    };
    // 3 / (8 + 3 + 2) = 3/13 ≈ 23.1%
    expect(computeCacheHitRate(canonical)).toBeCloseTo(3 / 13, 6);
    expect(formatCacheHitRate(canonical)).toBe('23.1%');

    // Partially unknown input/cache_read/cache_write marks hit rate as approximate
    expect(
      formatCacheHitRate({
        ...canonical,
        unknown_events: { cache_write: 1 },
      })
    ).toBe('≈ 23.1%');

    // Measured zero cache_read with positive input -> 0.0%
    expect(
      formatCacheHitRate({
        input: 10,
        output: 5,
        cache_read: 0,
        cache_write: 0,
        cache_write_1h: null,
        reasoning: null,
        summable_tokens: 15,
      })
    ).toBe('0.0%');

    // Any null required bucket -> unknown ("—")
    expect(
      formatCacheHitRate({
        input: 10,
        output: 5,
        cache_read: null,
        cache_write: 0,
        cache_write_1h: null,
        reasoning: null,
        summable_tokens: 15,
      })
    ).toBe('—');

    // Zero denominator -> unknown ("—")
    expect(
      formatCacheHitRate({
        input: 0,
        output: 5,
        cache_read: 0,
        cache_write: 0,
        cache_write_1h: null,
        reasoning: null,
        summable_tokens: 5,
      })
    ).toBe('—');
  });

  it('maps measurement_status values to badge variants and labels', () => {
    expect(measurementStatusDisplay('measured')).toEqual({ variant: 'success', label: 'Measured' });
    expect(measurementStatusDisplay('stale')).toEqual({ variant: 'warning', label: 'Stale' });
    expect(measurementStatusDisplay('unmeasured')).toEqual({
      variant: 'neutral',
      label: 'Unmeasured',
    });
  });
});

describe('scion-agent-usage-panel', () => {
  let container: HTMLElement;
  let fetchMock: ReturnType<typeof vi.fn<() => Promise<Response>>>;

  beforeEach(() => {
    container = document.createElement('div');
    document.body.appendChild(container);
    fetchMock = vi.fn<() => Promise<Response>>();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    container.remove();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('does not fetch until loadIfNeeded is called, then renders canonical buckets and per-model rows', async () => {
    const payload: AgentUsageResponse = {
      agent_id: 'agent-1',
      project_id: 'proj-1',
      window: {
        since: '2026-10-26T00:00:00Z',
        until: '2026-11-25T23:59:59.999999Z',
      },
      calls: 4,
      error_calls: 1,
      events: 4,
      null_token_events: 0,
      tokens: {
        input: 8,
        output: 5,
        cache_read: 3,
        cache_write: 2,
        cache_write_1h: null,
        reasoning: 2,
        summable_tokens: 18,
      },
      by_model: [
        {
          model: 'claude-sonnet-4-6',
          harness: 'claude',
          usage_source: 'native',
          calls: 3,
          error_calls: 0,
          events: 3,
          null_token_events: 0,
          tokens: {
            input: 8,
            output: 5,
            cache_read: 3,
            cache_write: 2,
            cache_write_1h: null,
            reasoning: 2,
            summable_tokens: 18,
          },
        },
        {
          model: 'claude-haiku-4-5',
          harness: 'claude',
          usage_source: 'native',
          calls: 1,
          error_calls: 1,
          events: 1,
          null_token_events: 1,
          tokens: {
            input: null,
            output: null,
            cache_read: null,
            cache_write: null,
            cache_write_1h: null,
            reasoning: null,
            summable_tokens: null,
          },
        },
      ],
      measurement_status: 'measured',
      as_of: '2026-11-25T18:00:00Z',
      last_receipt_at: '2026-11-25T17:59:30Z',
    };

    fetchMock.mockResolvedValue(jsonResponse(payload));

    const el = document.createElement('scion-agent-usage-panel') as ScionAgentUsagePanel;
    el.agentId = 'agent-1';
    container.appendChild(el);
    await waitForPanel(el);

    // Lazy loading (N3): no request until loadIfNeeded() is triggered.
    expect(fetchMock).not.toHaveBeenCalled();

    await el.loadIfNeeded();
    await waitForPanel(el);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/agents/agent-1/usage?since=30d',
      expect.any(Object)
    );

    // Second loadIfNeeded() for the same agentId is a no-op.
    await el.loadIfNeeded();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    const root = el.shadowRoot!;
    expect(root.querySelector('[data-testid="usage-summable-total"]')?.textContent?.trim()).toBe(
      '18'
    );
    expect(root.querySelector('[data-testid="usage-input"]')?.textContent?.trim()).toBe('8');
    expect(root.querySelector('[data-testid="usage-output"]')?.textContent?.trim()).toBe('5');
    expect(root.querySelector('[data-testid="usage-cache-read"]')?.textContent?.trim()).toBe('3');
    expect(root.querySelector('[data-testid="usage-cache-write"]')?.textContent?.trim()).toBe('2');
    expect(root.querySelector('[data-testid="usage-cache-hit-rate"]')?.textContent?.trim()).toBe(
      '23.1%'
    );
    expect(root.querySelector('[data-testid="usage-reasoning"]')?.textContent?.trim()).toBe('2');
    expect(root.querySelector('[data-testid="usage-calls"]')?.textContent).toContain('4');
    expect(root.querySelector('[data-testid="usage-calls"]')?.textContent).toContain('1 errors');

    const badge = root.querySelector('[data-testid="usage-measurement-status"]');
    expect(badge?.getAttribute('variant')).toBe('success');
    expect(badge?.textContent?.trim()).toBe('Measured');

    const lastUpdated = root.querySelector('[data-testid="usage-last-updated"]')?.textContent ?? '';
    expect(lastUpdated).toContain('Last updated:');
    expect(lastUpdated).not.toContain('Last updated: —');

    const rows = root.querySelectorAll('[data-testid="usage-model-row"]');
    expect(rows.length).toBe(2);
    expect(rows[0].textContent).toContain('claude-sonnet-4-6');
    expect(rows[1].textContent).toContain('claude-haiku-4-5');
    expect(rows[1].textContent).toContain('—');
  });

  it('renders unknown/null buckets as em dash, shows stale badge and null-events notice', async () => {
    const payload: AgentUsageResponse = {
      agent_id: 'agent-2',
      project_id: 'proj-1',
      window: {
        since: '2026-10-26T00:00:00Z',
        until: '2026-11-25T23:59:59.999999Z',
      },
      calls: 2,
      error_calls: 0,
      events: 2,
      null_token_events: 2,
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
      measurement_status: 'stale',
      as_of: '2026-11-25T18:00:00Z',
      last_receipt_at: null,
    };

    fetchMock.mockResolvedValue(jsonResponse(payload));

    const el = document.createElement('scion-agent-usage-panel') as ScionAgentUsagePanel;
    el.agentId = 'agent-2';
    container.appendChild(el);
    await el.loadIfNeeded();
    await waitForPanel(el);

    const root = el.shadowRoot!;
    expect(root.querySelector('[data-testid="usage-summable-total"]')?.textContent?.trim()).toBe(
      '—'
    );
    expect(root.querySelector('[data-testid="usage-input"]')?.textContent?.trim()).toBe('—');
    expect(root.querySelector('[data-testid="usage-output"]')?.textContent?.trim()).toBe('—');
    expect(root.querySelector('[data-testid="usage-cache-read"]')?.textContent?.trim()).toBe('—');
    expect(root.querySelector('[data-testid="usage-cache-write"]')?.textContent?.trim()).toBe('—');
    expect(root.querySelector('[data-testid="usage-cache-hit-rate"]')?.textContent?.trim()).toBe(
      '—'
    );
    expect(root.querySelector('[data-testid="usage-last-updated"]')?.textContent?.trim()).toBe(
      'Last updated: —'
    );

    const badge = root.querySelector('[data-testid="usage-measurement-status"]');
    expect(badge?.getAttribute('variant')).toBe('warning');
    expect(badge?.textContent?.trim()).toBe('Stale');

    expect(root.querySelector('[data-testid="usage-null-events-notice"]')).not.toBeNull();
  });

  it('marks partially-known buckets with ≥ prefix and notice banner (O1)', async () => {
    const payload: AgentUsageResponse = {
      agent_id: 'agent-partial',
      project_id: 'proj-1',
      window: { since: '2026-10-26T00:00:00Z', until: '2026-11-25T23:59:59.999999Z' },
      calls: 2,
      error_calls: 0,
      events: 2,
      null_token_events: 0,
      tokens: {
        input: 17,
        output: 7,
        cache_read: null,
        cache_write: 5,
        cache_write_1h: null,
        reasoning: null,
        summable_tokens: 29,
        unknown_events: {
          cache_write: 1,
        },
      },
      by_model: [],
      measurement_status: 'measured',
      as_of: '2026-11-25T18:00:00Z',
      last_receipt_at: '2026-11-25T17:59:00Z',
    };

    fetchMock.mockResolvedValue(jsonResponse(payload));

    const el = document.createElement('scion-agent-usage-panel') as ScionAgentUsagePanel;
    el.agentId = 'agent-partial';
    container.appendChild(el);
    await el.loadIfNeeded();
    await waitForPanel(el);

    const root = el.shadowRoot!;
    expect(root.querySelector('[data-testid="usage-input"]')?.textContent?.trim()).toBe('17');
    expect(root.querySelector('[data-testid="usage-cache-write"]')?.textContent?.trim()).toBe(
      '≥ 5'
    );
    expect(root.querySelector('[data-testid="usage-summable-total"]')?.textContent?.trim()).toBe(
      '≥ 29'
    );
    expect(root.querySelector('[data-testid="usage-null-events-notice"]')).not.toBeNull();
  });

  it('re-fetches with updated since query param when sl-select window changes', async () => {
    const payload: AgentUsageResponse = {
      agent_id: 'agent-3',
      project_id: 'proj-1',
      window: { since: '2026-11-18T00:00:00Z', until: '2026-11-25T23:59:59.999999Z' },
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
    };

    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(payload)));

    const el = document.createElement('scion-agent-usage-panel') as ScionAgentUsagePanel;
    el.agentId = 'agent-3';
    container.appendChild(el);
    await el.loadIfNeeded();
    await waitForPanel(el);

    const select = el.shadowRoot!.querySelector(
      'sl-select[data-testid="usage-window-select"]'
    ) as HTMLElement & { value: string };
    expect(select).not.toBeNull();
    select.value = '7d';
    select.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await waitForPanel(el);

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/agents/agent-3/usage?since=7d',
      expect.any(Object)
    );
  });
});
