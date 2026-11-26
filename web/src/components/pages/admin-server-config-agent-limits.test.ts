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

import { describe, it, expect, beforeAll, afterEach, vi } from 'vitest';
import { AGENT_LIMIT_CEILINGS_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';

type ConfigHost = HTMLElement & {
  updateComplete: Promise<unknown>;
  populateForm(data: Record<string, unknown>): void;
  buildLayer1Payload(): Record<string, unknown>;
  settingsTier: 'db' | 'file';
  loading: boolean;
  activeTab: string;
  agentLimitMaxTurns: number;
  agentLimitMaxModelCalls: number;
  agentLimitMaxDuration: string;
};

describe('admin server config Agent Limit Ceilings panel', () => {
  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({}), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
    await import('./admin-server-config.js');
  }, 30_000);

  afterEach(() => {
    setFeatureFlag(AGENT_LIMIT_CEILINGS_FLAG, false);
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  async function mount(data: Record<string, unknown>, flagOn: boolean): Promise<ConfigHost> {
    setFeatureFlag(AGENT_LIMIT_CEILINGS_FLAG, flagOn);
    const el = document.createElement('scion-page-admin-server-config') as ConfigHost;
    document.body.appendChild(el);
    el.loading = false;
    el.activeTab = 'general';
    el.settingsTier = 'db';
    el.populateForm(data);
    await el.updateComplete;
    return el;
  }

  it('hides the panel when experiment is off and no ceilings are stored', async () => {
    const el = await mount({ settings_tier: 'db' }, false);
    expect(el.shadowRoot!.querySelector('[data-section="agent-limits"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('[data-section="agent-limits-mismatch"]')).toBeNull();
  });

  it('shows mismatch warning when experiment is off and ceilings are stored', async () => {
    const el = await mount({ settings_tier: 'db', agent_limits: { max_turns: 50 } }, false);
    expect(el.shadowRoot!.querySelector('[data-section="agent-limits"]')).toBeNull();
    const alert = el.shadowRoot!.querySelector('[data-agent-limits-mismatch]');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain(AGENT_LIMIT_CEILINGS_FLAG);
  });

  it('renders three ceiling inputs and cooperative guardrail note when experiment is on', async () => {
    const el = await mount(
      {
        settings_tier: 'db',
        agent_limits: { max_turns: 50, max_model_calls: 200, max_duration: '2h' },
      },
      true
    );
    const section = el.shadowRoot!.querySelector('[data-section="agent-limits"]');
    expect(section).not.toBeNull();
    const text = section!.textContent!.replace(/\s+/g, ' ');
    expect(text).toContain('cooperative budget guardrails');
    expect(text).toContain('not a security boundary');

    const turns = section!.querySelector('[data-field="agent_limits.max_turns"]')!;
    const calls = section!.querySelector('[data-field="agent_limits.max_model_calls"]')!;
    const dur = section!.querySelector('[data-field="agent_limits.max_duration"]')!;
    expect(turns.getAttribute('value')).toBe('50');
    expect(calls.getAttribute('value')).toBe('200');
    expect(dur.getAttribute('value')).toBe('2h');
  });

  it('omits agent_limits from save payload when untouched, and includes it when edited or cleared', async () => {
    const el = await mount({ settings_tier: 'db', agent_limits: { max_turns: 50 } }, true);
    expect(el.buildLayer1Payload().agent_limits).toBeUndefined();

    el.agentLimitMaxTurns = 75;
    el.agentLimitMaxDuration = '4h';
    expect(el.buildLayer1Payload().agent_limits).toEqual({
      max_turns: 75,
      max_duration: '4h',
    });

    el.agentLimitMaxTurns = 0;
    el.agentLimitMaxDuration = '';
    expect(el.buildLayer1Payload().agent_limits).toEqual({});
  });
});
