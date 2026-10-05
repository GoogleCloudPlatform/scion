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

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';
import './provisioned-badge.js';
import type { ScionProvisionedBadge } from './provisioned-badge.js';
import type { Agent } from '../../shared/types.js';
import { isProvisionedOnly } from '../../shared/agent-state-display.js';

type BadgeAgent = Pick<Agent, 'name' | 'phase' | 'provisionedOnly'>;

describe('isProvisionedOnly', () => {
  it('needs both the hub flag and phase created', () => {
    expect(isProvisionedOnly({ phase: 'created', provisionedOnly: true })).toBe(true);
    expect(isProvisionedOnly({ phase: 'created' })).toBe(false);
    expect(isProvisionedOnly({ phase: 'created', provisionedOnly: false })).toBe(false);
    // A stale flag after an SSE delta moved the agent on.
    expect(isProvisionedOnly({ phase: 'provisioning', provisionedOnly: true })).toBe(false);
  });
});

describe('scion-provisioned-badge', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function render(agent: BadgeAgent | null): Promise<ScionProvisionedBadge> {
    const el = document.createElement('scion-provisioned-badge');
    el.agent = agent;
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('renders nothing and is hidden for a normal created agent', async () => {
    const el = await render({ name: 'a', phase: 'created' });
    expect(el.hasAttribute('hidden')).toBe(true);
    expect(el.shadowRoot?.querySelector('.badge')).toBeNull();
  });

  it('renders nothing for null', async () => {
    const el = await render(null);
    expect(el.hasAttribute('hidden')).toBe(true);
    expect(el.shadowRoot?.querySelector('.badge')).toBeNull();
  });

  it('shows the badge with a start hint, and clears once started', async () => {
    const el = await render({ name: 'po-agent', phase: 'created', provisionedOnly: true });
    const badge = el.shadowRoot?.querySelector('.badge');
    expect(el.hasAttribute('hidden')).toBe(false);
    expect(badge?.textContent?.trim()).toBe('provisioned, not started');
    expect(badge?.getAttribute('title')).toContain('scion start po-agent');
    expect(badge?.getAttribute('title')).toContain('Start');

    el.agent = { name: 'po-agent', phase: 'starting', provisionedOnly: true };
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.badge')).toBeNull();
    expect(el.hasAttribute('hidden')).toBe(true);
  });
});
