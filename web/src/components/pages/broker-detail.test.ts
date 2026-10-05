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

import type { Agent } from '../../shared/types.js';
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
