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

import type { AgentInlineConfig } from '../../shared/types.js';

// chat-thread (imported by agent-detail) pulls in the app entry point,
// which bootstraps the SPA on load; stub it as the chat tests do.
vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(),
  extractApiError: (): Promise<string> => Promise.resolve('error'),
}));

type CardHost = {
  renderConfigLimitsCard(inline: AgentInlineConfig | undefined): TemplateResult | typeof nothing;
};

function renderCard(inline: AgentInlineConfig | undefined): HTMLElement {
  const el = document.createElement('scion-page-agent-detail') as unknown as CardHost;
  const host = document.createElement('div');
  render(el.renderConfigLimitsCard(inline), host);
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
