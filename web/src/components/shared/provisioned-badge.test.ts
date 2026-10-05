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
import { render } from 'lit';
import { renderProvisionedBadge } from './provisioned-badge.js';
import type { ScionStatusBadge } from './status-badge.js';
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

describe('renderProvisionedBadge', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function mount(agent: BadgeAgent, size?: 'small'): Promise<ScionStatusBadge | null> {
    const host = document.createElement('div');
    document.body.appendChild(host);
    render(renderProvisionedBadge(agent, size), host);
    const el = host.querySelector('scion-status-badge');
    await el?.updateComplete;
    return el;
  }

  it('renders nothing for a normal created agent', async () => {
    expect(await mount({ name: 'a', phase: 'created' })).toBeNull();
  });

  it('renders nothing once a start moved the agent out of created', async () => {
    expect(await mount({ name: 'a', phase: 'starting', provisionedOnly: true })).toBeNull();
  });

  it('shows the label and a start hint for a provision-only agent', async () => {
    const el = await mount({ name: 'po-agent', phase: 'created', provisionedOnly: true }, 'small');
    expect(el).not.toBeNull();
    expect(el!.getAttribute('label')).toBe('provisioned, not started');
    expect(el!.getAttribute('size')).toBe('small');
    const hint = el!.getAttribute('title') ?? '';
    expect(hint).toContain('Start');
    expect(hint).toContain('scion start po-agent');
    expect(el!.shadowRoot?.textContent).toContain('provisioned, not started');
  });
});
