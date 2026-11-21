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
 * Limits card on the agent detail Configuration tab.
 *
 * Rules: a stored count limit of 0 (max_turns, max_model_calls) and a stored
 * duration of "0" (max_duration) mean Unlimited, as on the Edit page. An
 * absent limit is Inherited: the hub fills it from the template, hub or
 * broker defaults. A positive limit shows its value. The card is hidden only
 * when no limit is stored.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, nothing, type TemplateResult } from 'lit';

import type { Agent, AgentInlineConfig } from '../../shared/types.js';

// chat-thread (imported by agent-detail) pulls in the app entry point,
// which bootstraps the SPA on load; stub it as the chat tests do.
vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(),
  extractApiError: (): Promise<string> => Promise.resolve('error'),
}));

type CardHost = {
  renderConfigLimitsCard(inline: AgentInlineConfig | undefined): TemplateResult | typeof nothing;
  renderLimitsUsageCard(agent: Agent): TemplateResult | typeof nothing;
};

function renderCard(inline: AgentInlineConfig | undefined): HTMLElement {
  const el = document.createElement('scion-page-agent-detail') as unknown as CardHost;
  const host = document.createElement('div');
  render(el.renderConfigLimitsCard(inline), host);
  return host;
}

function renderUsageCard(agent: Partial<Agent>): HTMLElement {
  const el = document.createElement('scion-page-agent-detail') as unknown as CardHost;
  const host = document.createElement('div');
  render(
    el.renderLimitsUsageCard({
      id: 'agent-1',
      name: 'worker',
      projectId: 'proj-1',
      template: 'default',
      phase: 'running',
      ...agent,
    }),
    host
  );
  return host;
}

/** The card's values keyed by label. */
function values(host: HTMLElement): Record<string, string> {
  const out: Record<string, string> = {};
  for (const item of host.querySelectorAll('.info-item')) {
    const label = item.querySelector('.info-label')?.textContent?.trim() ?? '';
    out[label] = item.querySelector('.info-value')?.textContent?.trim() ?? '';
  }
  return out;
}

/** Usage card items keyed by label, with text and whether a progress bar is rendered. */
function usageItems(host: HTMLElement): Record<string, { value: string; hasProgressBar: boolean }> {
  const out: Record<string, { value: string; hasProgressBar: boolean }> = {};
  for (const item of host.querySelectorAll('.limit-item')) {
    const label = item.querySelector('.limit-label')?.textContent?.trim() ?? '';
    const value = (item.querySelector('.limit-value')?.textContent ?? '')
      .replace(/\s+/g, ' ')
      .trim();
    out[label] = {
      value,
      hasProgressBar: item.querySelector('.progress-bar-track') !== null,
    };
  }
  return out;
}

describe('agent detail Limits card', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  it.each<[string, AgentInlineConfig | undefined]>([
    ['no inline config', undefined],
    ['no limit stored', {}],
    ['an empty duration only', { max_duration: '' }],
  ])('renders nothing with %s', (_name, inline) => {
    expect(renderCard(inline).innerHTML.replace(/<!--.*?-->/g, '')).toBe('');
  });

  it.each<[string, AgentInlineConfig, Record<string, string>]>([
    [
      'stored 0 count limits',
      { max_turns: 0, max_model_calls: 0 },
      { 'Max Turns': 'Unlimited', 'Max Model Calls': 'Unlimited', 'Max Duration': 'Inherited' },
    ],
    [
      'a stored "0" duration',
      { max_duration: '0' },
      { 'Max Turns': 'Inherited', 'Max Model Calls': 'Inherited', 'Max Duration': 'Unlimited' },
    ],
    [
      'explicit Unlimited on every limit',
      { max_turns: 0, max_model_calls: 0, max_duration: '0' },
      { 'Max Turns': 'Unlimited', 'Max Model Calls': 'Unlimited', 'Max Duration': 'Unlimited' },
    ],
    [
      'positive limits',
      { max_turns: 50, max_model_calls: 200, max_duration: '2h' },
      { 'Max Turns': '50', 'Max Model Calls': '200', 'Max Duration': '2h' },
    ],
    [
      'a positive limit beside a stored 0 and an absent one',
      { max_turns: 25, max_model_calls: 0 },
      { 'Max Turns': '25', 'Max Model Calls': 'Unlimited', 'Max Duration': 'Inherited' },
    ],
  ])('shows %s', (_name, inline, want) => {
    expect(values(renderCard(inline))).toEqual(want);
  });
});

describe('agent detail Limits & Usage card', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  it('renders nothing when every limit is explicitly Unlimited', () => {
    const host = renderUsageCard({
      currentTurns: 12,
      currentModelCalls: 40,
      startedAt: new Date(Date.now() - 60_000).toISOString(),
      appliedConfig: {
        inlineConfig: { max_turns: 0, max_model_calls: 0, max_duration: '0' },
      },
    });
    expect(host.innerHTML.replace(/<!--.*?-->/g, '')).toBe('');
  });

  it('shows inherited usage without progress bars when no inline limits are set', () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date('2026-11-20T12:15:30Z'));
      const host = renderUsageCard({
        currentTurns: 7,
        currentModelCalls: 23,
        startedAt: '2026-11-20T12:00:00Z',
        appliedConfig: { inlineConfig: {} },
      });
      expect(usageItems(host)).toEqual({
        Turns: { value: '7 / Inherited', hasProgressBar: false },
        'Model Calls': { value: '23 / Inherited', hasProgressBar: false },
        Elapsed: { value: '15m 30s / Inherited', hasProgressBar: false },
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it('shows inherited labels when appliedConfig is absent and agent has not started', () => {
    const host = renderUsageCard({});
    expect(usageItems(host)).toEqual({
      Turns: { value: '0 / Inherited', hasProgressBar: false },
      'Model Calls': { value: '0 / Inherited', hasProgressBar: false },
      'Max Duration': { value: 'Inherited', hasProgressBar: false },
    });
  });

  it.each<[string, string]>([
    ['Go zero date', '0001-01-01T00:00:00Z'],
    ['invalid date', 'not-a-date'],
  ])('treats %s startedAt as unstarted for inherited duration', (_label, startedAt) => {
    const items = usageItems(
      renderUsageCard({
        phase: 'running',
        startedAt,
        appliedConfig: { inlineConfig: {} },
      })
    );
    expect(items['Elapsed']).toBeUndefined();
    expect(items['Max Duration']).toEqual({ value: 'Inherited', hasProgressBar: false });
  });

  it('shows inherited elapsed only while running or stopping, not when stopped', () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date('2026-11-20T12:15:30Z'));
      const base = {
        currentTurns: 7,
        currentModelCalls: 23,
        startedAt: '2026-11-20T12:00:00Z',
        appliedConfig: { inlineConfig: {} },
      };

      expect(usageItems(renderUsageCard({ ...base, phase: 'running' }))['Elapsed']).toEqual({
        value: '15m 30s / Inherited',
        hasProgressBar: false,
      });
      expect(usageItems(renderUsageCard({ ...base, phase: 'stopping' }))['Elapsed']).toEqual({
        value: '15m 30s / Inherited',
        hasProgressBar: false,
      });

      const stopped = usageItems(renderUsageCard({ ...base, phase: 'stopped' }));
      expect(stopped['Elapsed']).toBeUndefined();
      expect(stopped['Max Duration']).toEqual({
        value: 'Inherited',
        hasProgressBar: false,
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it('mixes explicit limits (with progress bar), inherited limits (no bar), and omits explicit Unlimited', () => {
    const host = renderUsageCard({
      currentTurns: 10,
      currentModelCalls: 42,
      appliedConfig: {
        inlineConfig: {
          max_turns: 50,
          max_model_calls: 0,
        },
      },
    });
    expect(usageItems(host)).toEqual({
      Turns: { value: '10 / 50', hasProgressBar: true },
      'Max Duration': { value: 'Inherited', hasProgressBar: false },
    });
  });

  it('renders progress bars for explicit positive limits', () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date('2026-11-20T12:30:00Z'));
      const host = renderUsageCard({
        currentTurns: 5,
        currentModelCalls: 20,
        startedAt: '2026-11-20T12:00:00Z',
        appliedConfig: {
          inlineConfig: {
            max_turns: 50,
            max_model_calls: 200,
            max_duration: '2h',
          },
        },
      });
      expect(usageItems(host)).toEqual({
        Turns: { value: '5 / 50', hasProgressBar: true },
        'Model Calls': { value: '20 / 200', hasProgressBar: true },
        'Time Remaining': { value: '1h 30m', hasProgressBar: true },
      });
    } finally {
      vi.useRealTimers();
    }
  });
});
