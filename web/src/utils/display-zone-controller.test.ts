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
import { LitElement, html } from 'lit';
import { customElement } from 'lit/decorators.js';
import { DisplayZoneController } from './display-zone-controller.js';
import { setPreferredTimeZone, effectiveTimeZone } from './time.js';

@customElement('scion-test-zone-host')
class TestZoneHost extends LitElement {
  readonly zone = new DisplayZoneController(this); // not private (R3-2): see display-zone-controller.ts
  renderCount = 0;

  override render() {
    this.renderCount++;
    return html`${effectiveTimeZone()}`;
  }
}

describe('DisplayZoneController', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
  });

  it('re-renders the host when the effective zone changes', async () => {
    const el = document.createElement('scion-test-zone-host') as TestZoneHost;
    document.body.appendChild(el);
    await el.updateComplete;

    const countBefore = el.renderCount;
    setPreferredTimeZone('Asia/Tokyo');
    await el.updateComplete;

    expect(el.renderCount).toBeGreaterThan(countBefore);
    expect(el.shadowRoot?.textContent).toBe('Asia/Tokyo');
  });

  it('does not re-render on a no-op set (same value)', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const el = document.createElement('scion-test-zone-host') as TestZoneHost;
    document.body.appendChild(el);
    await el.updateComplete;

    const countBefore = el.renderCount;
    setPreferredTimeZone('Asia/Tokyo');
    // No event is dispatched for a no-op set, so nothing is pending; a
    // following microtask flush should show no new render happened.
    await Promise.resolve();
    expect(el.renderCount).toBe(countBefore);
  });

  it('stops listening after the host disconnects', async () => {
    const el = document.createElement('scion-test-zone-host') as TestZoneHost;
    document.body.appendChild(el);
    await el.updateComplete;

    el.remove();
    const countBefore = el.renderCount;
    setPreferredTimeZone('Asia/Tokyo');
    await Promise.resolve();
    expect(el.renderCount).toBe(countBefore);
  });
});
