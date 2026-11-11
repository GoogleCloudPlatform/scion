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

import { describe, it, expect, afterEach } from 'vitest';
import './authority-badge.js';
import type { ScionAuthorityBadge } from './authority-badge.js';
import type { AgentAuthority } from '../../shared/types.js';

describe('scion-authority-badge', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function render(a: AgentAuthority | null): Promise<ScionAuthorityBadge> {
    const el = document.createElement('scion-authority-badge');
    el.authority = a;
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('renders nothing for live, unrecorded or no state', async () => {
    for (const a of [null, { state: 'live' }, { state: 'unrecorded' }]) {
      const el = await render(a);
      expect(el.shadowRoot?.querySelector('sl-badge')).toBeNull();
    }
  });

  it('renders Orphaned with a warning badge and the cause in the tooltip', async () => {
    const el = await render({
      state: 'orphaned',
      cause: 'delegator_deleted',
      principal: { kind: 'agent', id: 'a-1', deleted: true },
    });
    const badge = el.shadowRoot?.querySelector('sl-badge');
    expect(badge?.textContent?.trim()).toBe('Orphaned');
    expect(badge?.getAttribute('variant')).toBe('warning');
    const tip = el.shadowRoot?.querySelector('sl-tooltip')?.getAttribute('content') ?? '';
    expect(tip).toContain('agent a-1, which was deleted');
    expect(tip).toContain('Stop or delete this agent');
  });

  it('renders Revoked with a danger badge and updates when the state clears', async () => {
    const el = await render({ state: 'revoked', cause: 'user_delete_cascade' });
    const badge = el.shadowRoot?.querySelector('sl-badge');
    expect(badge?.textContent?.trim()).toBe('Revoked');
    expect(badge?.getAttribute('variant')).toBe('danger');

    el.authority = { state: 'live' };
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('sl-badge')).toBeNull();
  });
});
