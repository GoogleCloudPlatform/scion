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
 * Effective-role cards render a binding's expiry and activation in the
 * display zone, 24-hour, with a zone label, and re-render when the zone
 * changes (tz-refactor task 20). Vitest pins the browser zone to UTC; the
 * display preference here is Asia/Tokyo (UTC+9), where 15:00Z is midnight.
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';

await import('./effective-role-provenance.js');

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

describe('scion-effective-role-provenance lifecycle times (tz-refactor task 20)', () => {
  let el: AnyEl = null;

  afterEach(() => {
    el?.remove();
    el = null;
    setPreferredTimeZone('');
  });

  it('renders Expires and Activates in the display zone and follows a zone change', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    // No principalId, so the component never fetches; bindings are set directly.
    el = document.createElement('scion-effective-role-provenance');
    document.body.appendChild(el);
    el.loading = false;
    el.bindings = [
      {
        id: 'rb-1',
        roleDefinitionId: 'role-1',
        roleName: 'editor',
        principalType: 'user',
        principalId: 'user-1',
        scopeType: 'system',
        scopeId: '',
        createdAt: '2026-01-01T00:00:00Z',
        // A pending binding far in the future, so both lines render.
        notBefore: '2030-01-14T15:00:00Z',
        expiresAt: '2030-02-14T15:00:00Z',
        source: 'direct',
      },
    ];
    await el.updateComplete;

    const lines = (): string[] =>
      Array.from(el.shadowRoot?.querySelectorAll('.lifecycle-info') ?? []).map(
        (n) => (n as Element).textContent?.replace(/\s+/g, ' ').trim() ?? ''
      );
    expect(lines()).toEqual([
      'Expires Feb 15, 2030, 00:00 (Asia/Tokyo)',
      'Activates Jan 15, 2030, 00:00 (Asia/Tokyo)',
    ]);

    setPreferredTimeZone('UTC');
    await el.updateComplete;
    expect(lines()).toEqual([
      'Expires Feb 14, 2030, 15:00 (UTC)',
      'Activates Jan 14, 2030, 15:00 (UTC)',
    ]);
  });
});
