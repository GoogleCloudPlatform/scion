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

import { describe, it, expect, beforeAll } from 'vitest';
import { render, type TemplateResult } from 'lit';
import { elementStyleRules } from './__fixtures__/card-layout.js';

type BrokerPage = HTMLElement & {
  renderBrokerCard(item: Record<string, unknown>): TemplateResult;
};

describe('broker card layout', () => {
  let rules: Map<string, string>;
  let page: BrokerPage;

  beforeAll(async () => {
    await import('./brokers.js');
    rules = elementStyleRules('scion-page-brokers');
    page = document.createElement('scion-page-brokers') as unknown as BrokerPage;
  });

  it('renders the broker name in the span the shared wrapping rules apply to', () => {
    const name = 'a_very_long_broker_name_with_no_spaces_or_hyphens_to_break_on';
    const container = document.createElement('div');
    render(
      page.renderBrokerCard({
        id: 'b1',
        status: 'online',
        version: '1.0.0',
        lastHeartbeat: new Date().toISOString(),
        name,
      }),
      container
    );
    const span = container.querySelector('.resource-name > span');
    expect(span?.textContent?.trim()).toBe(name);
  });

  it('keeps the broker type badge inside the wrapping span', () => {
    const container = document.createElement('div');
    render(
      page.renderBrokerCard({
        id: 'b2',
        name: 'hosted_broker',
        status: 'online',
        lastHeartbeat: new Date().toISOString(),
        labels: { 'scion.io/broker-type': 'hosted' },
      }),
      container
    );
    expect(container.querySelector('.resource-name > span .broker-type-badge')).not.toBeNull();
  });

  it('lets a long unbroken name wrap instead of spilling past the card', () => {
    expect(rules.get('.broker-header > div') ?? '').toMatch(/min-width:\s*0/);
    expect(rules.get('.resource-name > span') ?? '').toMatch(/overflow-wrap:\s*anywhere/);
    expect(rules.get('.resource-name') ?? '').toMatch(/min-width:\s*0/);
  });
});
