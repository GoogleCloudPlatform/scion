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
 * Agent card status badge on the broker detail page (ptone/scion#2929):
 * a provision-only agent shows as created (not started) with a start hint.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import { render, type TemplateResult } from 'lit';

import type { Agent, RuntimeBroker } from '../../shared/types.js';
import { PROVISIONED_ONLY_LABEL } from '../../shared/agent-state-display.js';
import type { ScionPageBrokerDetail } from './broker-detail.js';

function makeAgent(overrides: Partial<Agent>): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 't',
    phase: 'running',
    ...overrides,
  } as Agent;
}

/** Render the agent card for `agent` and return its status badge. */
function cardBadge(agent: Agent): Element {
  const el = document.createElement('scion-page-broker-detail') as ScionPageBrokerDetail;
  const tpl = (el as unknown as { renderAgentCard(a: Agent): TemplateResult }).renderAgentCard(
    agent
  );
  const host = document.createElement('div');
  render(tpl, host);
  const badge = host.querySelector('.agent-header > scion-status-badge');
  expect(badge).not.toBeNull();
  return badge!;
}

describe('broker detail agent card status badge', () => {
  beforeAll(async () => {
    await import('./broker-detail.js');
  }, 30_000);

  it('shows a provision-only agent as created (not started) with a start hint', () => {
    const badge = cardBadge(makeAgent({ phase: 'created', provisionedOnly: true }));
    expect(badge.getAttribute('label')).toBe(PROVISIONED_ONLY_LABEL);
    expect(badge.getAttribute('title')).toContain('scion start agent-1');
  });

  it('shows a plain created agent as created, with no hint', () => {
    const badge = cardBadge(makeAgent({ phase: 'created' }));
    expect(badge.getAttribute('label')).toBe('created');
    expect(badge.hasAttribute('title')).toBe(false);
  });
});

/** Render the page header for `broker` and return the "Created" stat text. */
function createdStat(broker: Partial<RuntimeBroker>): string {
  const el = document.createElement('scion-page-broker-detail') as ScionPageBrokerDetail;
  const page = el as unknown as {
    loading: boolean;
    broker: RuntimeBroker | null;
    render(): TemplateResult;
  };
  page.loading = false;
  page.broker = {
    id: 'b-1',
    name: 'broker-1',
    slug: 'broker-1',
    version: '1.0.0',
    status: 'online',
    connectionState: 'connected',
    lastHeartbeat: '2026-09-28T12:00:00Z',
    autoProvide: false,
    ...broker,
  } as RuntimeBroker;
  const host = document.createElement('div');
  render(page.render(), host);
  const stat = Array.from(host.querySelectorAll('.stat')).find(
    (s) => s.querySelector('.stat-label')?.textContent?.trim() === 'Created'
  );
  expect(stat).toBeDefined();
  return stat!.querySelector('.stat-value-sm')?.textContent?.trim() ?? '';
}

describe('broker detail Created stat', () => {
  beforeAll(async () => {
    await import('./broker-detail.js');
  }, 30_000);

  it('shows the date the hub sends as created', () => {
    expect(createdStat({ created: '2026-08-14T12:00:00Z' })).toMatch(/Aug 14, 2026/);
  });

  it('still reads the older createdAt name', () => {
    expect(createdStat({ createdAt: '2026-07-01T12:00:00Z' })).toMatch(/Jul 1, 2026/);
  });

  it('shows a dash instead of a blank when the date is missing or invalid', () => {
    expect(createdStat({})).toBe('—');
    expect(createdStat({ created: 'not-a-date' })).toBe('—');
  });
});
