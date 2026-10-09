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
 * Needs attention panel: server order, severity icons, links only for
 * linkable subjects, and the empty and not-reported states.
 */

import { describe, it, expect, afterEach } from 'vitest';

import { attentionHref, type HealthAttentionItem } from './health-attention.js';
import './health-attention.js';
import { elementStyleRules } from './__fixtures__/css-rules.js';

async function mount(items: HealthAttentionItem[] | null): Promise<ShadowRoot> {
  const el = document.createElement('scion-health-attention');
  el.items = items;
  document.body.appendChild(el);
  await el.updateComplete;
  return el.shadowRoot!;
}

const items: HealthAttentionItem[] = [
  {
    severity: 'critical',
    kind: 'hub_check',
    subject: { type: 'hub', id: 'hub-a' },
    message: 'Hub check database is not healthy on this instance',
  },
  {
    severity: 'warning',
    kind: 'hub_check',
    subject: { type: 'hub', id: 'hub-a' },
    message: 'Runtime broker data not available',
  },
  {
    severity: 'warning',
    kind: 'broker_offline',
    subject: { type: 'runtime_broker', id: 'b 1', name: 'zulu' },
    message: 'Runtime broker zulu is offline',
  },
  {
    severity: 'warning',
    kind: 'integration',
    subject: { type: 'integration', id: 'chat', name: 'chat' },
    message: 'Integration chat is unhealthy',
  },
  {
    severity: 'warning',
    kind: 'integration',
    subject: { type: 'integration' },
    message: '2 integrations unhealthy',
  },
  {
    severity: 'warning',
    kind: 'dispatch',
    subject: { type: 'dispatch' },
    message: '1 broker dispatch stuck in progress',
  },
  {
    severity: 'warning',
    kind: 'agents',
    subject: { type: 'agent', id: 'ag1', name: 'w', project_id: 'p' },
    message: 'Agent proj / w is in the error phase',
  },
  {
    severity: 'warning',
    kind: 'agents',
    subject: { type: 'agents' },
    message: '3 more agents in the error phase',
  },
];

describe('scion-health-attention', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('keeps the server order and the server sentences', async () => {
    const root = await mount(items);
    const text = [...root.querySelectorAll('li')].map((li) => li.textContent?.trim());
    expect(text).toEqual(items.map((i) => i.message));
  });

  it('shows a severity icon per item', async () => {
    const root = await mount(items);
    const icons = [...root.querySelectorAll('li sl-icon')];
    expect(icons).toHaveLength(items.length);
    expect(icons[0]!.getAttribute('name')).toBe('exclamation-octagon');
    expect(icons[0]!.getAttribute('label')).toBe('Critical');
    expect(icons[2]!.getAttribute('name')).toBe('exclamation-triangle');
    expect(icons[2]!.getAttribute('label')).toBe('Warning');
  });

  it('links only items whose subject is a broker, agent or integration with an ID', async () => {
    const root = await mount(items);
    const hrefs = [...root.querySelectorAll('li')].map(
      (li) => li.querySelector('a')?.getAttribute('href') ?? null
    );
    expect(hrefs).toEqual([
      null,
      // The broker list item is about the hub: no broker link.
      null,
      '/brokers/b%201',
      '/admin/integrations/chat',
      // No integration identity (integrations_detail false): no link.
      null,
      null,
      '/agents/ag1',
      null,
    ]);
  });

  it('shows Nothing needs attention for an empty list', async () => {
    const root = await mount([]);
    expect(root.querySelector('ul')).toBeNull();
    expect(root.textContent).toContain('Nothing needs attention.');
  });

  it('does not claim all clear when the list is missing', async () => {
    const root = await mount(null);
    expect(root.textContent).toContain('Attention data not available');
    expect(root.textContent).not.toContain('Nothing needs attention');
  });

  it('uses theme tokens only, with no hex fallbacks', () => {
    for (const body of elementStyleRules('scion-health-attention').values()) {
      expect(body).not.toMatch(/#[0-9a-f]{3,8}\b|rgba?\(/i);
      for (const m of body.matchAll(/var\((--[\w-]+)/g)) expect(m[1]).toMatch(/^--scion-/);
    }
  });
});

describe('attentionHref', () => {
  it('returns null for subjects without an ID or of other types', () => {
    expect(attentionHref({ ...items[2]!, subject: { type: 'runtime_broker' } })).toBeNull();
    expect(attentionHref({ ...items[2]!, subject: { type: 'hub', id: 'x' } })).toBeNull();
    expect(attentionHref({ ...items[2]!, subject: { type: 'dispatch', id: 'x' } })).toBeNull();
    expect(attentionHref({ ...items[2]!, subject: { type: 'agents', id: 'x' } })).toBeNull();
  });
});
