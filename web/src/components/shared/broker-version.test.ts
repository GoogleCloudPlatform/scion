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
 * Runtime Broker version against the hub's (ptone/scion#5972): the shared
 * rendering used by both Brokers lists and the health dashboard.
 */

import { describe, it, expect } from 'vitest';
import { render } from 'lit';

import {
  brokerVersionTitle,
  hubVersionText,
  hubVersionTitle,
  normalizeVersionStatus,
  renderBrokerVersion,
  renderHubVersionNote,
} from './broker-version.js';

function rendered(t: ReturnType<typeof renderBrokerVersion>): HTMLElement {
  const host = document.createElement('div');
  render(t, host);
  return host;
}

describe('broker version helpers', () => {
  it('treats a missing or unexpected status as unknown, never match', () => {
    expect(normalizeVersionStatus('match')).toBe('match');
    expect(normalizeVersionStatus('mismatch')).toBe('mismatch');
    expect(normalizeVersionStatus('unknown')).toBe('unknown');
    expect(normalizeVersionStatus(undefined)).toBe('unknown');
    expect(normalizeVersionStatus('')).toBe('unknown');
    expect(normalizeVersionStatus('MATCH')).toBe('unknown');
  });

  it('names the hub version, an unknown one and a mixed fleet', () => {
    expect(hubVersionText('v1.3.0')).toBe('v1.3.0');
    expect(hubVersionText('')).toBe('unknown');
    expect(hubVersionText(undefined)).toBe('unknown');
    expect(hubVersionText('mixed')).toBe('mixed versions (upgrade in progress)');
    expect(hubVersionTitle('v1.3.0')).toBe('Hub runs v1.3.0');
    expect(hubVersionTitle('')).toBe('Hub version unknown');
    expect(hubVersionTitle('mixed')).toBe('Hub instances run different versions');
    // A hub built without build information reports "unknown": no token.
    expect(hubVersionText('unknown')).toBe('unknown');
    expect(hubVersionTitle('unknown')).toBe('Hub version unknown');
  });

  it('explains each broker state in the tooltip', () => {
    expect(brokerVersionTitle('v1.2.0', 'mismatch', 'v1.3.0')).toBe(
      'Differs from the hub. Hub runs v1.3.0'
    );
    expect(brokerVersionTitle('v1.3.0', 'match', 'v1.3.0')).toBe('Hub runs v1.3.0');
    expect(brokerVersionTitle('', 'unknown', 'v1.3.0')).toBe('Broker has not reported a version');
    expect(brokerVersionTitle(undefined, undefined, 'v1.3.0')).toBe(
      'Broker has not reported a version'
    );
    expect(brokerVersionTitle('v1.2.0', 'unknown', 'mixed')).toBe(
      'Hub instances run different versions'
    );
    expect(brokerVersionTitle('v1.2.0', 'unknown', '')).toBe('Hub version unknown');
    expect(brokerVersionTitle('unknown', 'unknown', 'v1.3.0')).toBe(
      'Cannot be compared with the hub. Hub runs v1.3.0'
    );
    expect(brokerVersionTitle('v1.2.0', 'unknown', 'unknown')).toBe('Hub version unknown');
  });
});

describe('renderBrokerVersion', () => {
  it('shows the token as reported, with no "v" prefix', () => {
    const host = rendered(renderBrokerVersion('1344d136', 'match', '1344d136'));
    expect(host.querySelector('.version-token')?.textContent).toBe('1344d136');
    expect(host.textContent).not.toContain('v1344d136');
    expect(host.querySelector('sl-icon')).toBeNull();
    expect(host.querySelector('[data-version-status]')?.getAttribute('data-version-status')).toBe(
      'match'
    );
  });

  it('marks a mismatch with a warning icon and names the hub version', () => {
    const host = rendered(renderBrokerVersion('v1.2.0', 'mismatch', 'v1.3.0'));
    const cell = host.querySelector('.broker-version-cell')!;
    expect(cell.getAttribute('data-version-status')).toBe('mismatch');
    expect(cell.getAttribute('title')).toBe('Differs from the hub. Hub runs v1.3.0');
    const icon = host.querySelector('sl-icon')!;
    expect(icon.getAttribute('name')).toBe('exclamation-triangle');
    expect(icon.getAttribute('label')).toBe("Differs from the hub's version");
  });

  it('shows an unreported version as unknown, not as matching', () => {
    const host = rendered(renderBrokerVersion('', undefined, 'v1.3.0'));
    expect(host.querySelector('.version-token')?.textContent).toBe('—');
    expect(host.querySelector('[data-version-status]')?.getAttribute('data-version-status')).toBe(
      'unknown'
    );
    expect(host.querySelector('sl-icon')?.getAttribute('name')).toBe('question-circle');
  });

  it('renders a hostile version as text, not markup', () => {
    const host = rendered(renderBrokerVersion('<img src=x onerror=alert(1)>', 'mismatch', 'v1'));
    expect(host.querySelector('img')).toBeNull();
    expect(host.querySelector('.version-token')?.textContent).toBe('<img src=x onerror=alert(1)>');
  });
});

describe('renderHubVersionNote', () => {
  it('reads "Hub: <version>", "Hub: unknown" and the mixed copy', () => {
    const text = (v: string): string | undefined =>
      rendered(renderHubVersionNote(v)).textContent?.replace(/\s+/g, ' ').trim();
    expect(text('v1.3.0')).toBe('Hub: v1.3.0');
    expect(text('')).toBe('Hub: unknown');
    expect(text('mixed')).toBe('Hub: mixed versions (upgrade in progress)');
  });
});
