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

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import type { UsageMeasurementStatus } from '../../shared/types.js';
import {
  formatMeasurementAgentCounts,
  measurementStatusDisplay,
  type ScionMeasurementStatusBadge,
} from './measurement-status-badge.js';
import './measurement-status-badge.js';

describe('scion-measurement-status-badge', () => {
  let container: HTMLElement;

  beforeEach(() => {
    container = document.createElement('div');
    document.body.appendChild(container);
  });

  afterEach(() => {
    container.remove();
  });

  it.each<[UsageMeasurementStatus, string, string]>([
    ['measured', 'success', 'Measured'],
    ['partial', 'warning', 'Partial'],
    ['calls_only', 'warning', 'Calls only'],
    ['stale', 'warning', 'Stale'],
    ['legacy_client', 'neutral', 'Legacy client'],
    ['unmeasured', 'neutral', 'Unmeasured'],
  ])('renders %s with variant=%s and label=%s', async (status, variant, label) => {
    expect(measurementStatusDisplay(status)).toEqual({ variant, label });

    const el = document.createElement(
      'scion-measurement-status-badge'
    ) as ScionMeasurementStatusBadge;
    el.status = status;
    container.appendChild(el);
    await el.updateComplete;

    const pill = el.shadowRoot!.querySelector('[data-testid="measurement-status-pill"]');
    expect(pill?.getAttribute('variant')).toBe(variant);
    expect(pill?.getAttribute('data-status')).toBe(status);
    expect(pill?.textContent?.trim()).toBe(label);
    expect(el.shadowRoot!.querySelector('[data-testid="measurement-status-counts"]')).toBeNull();
  });

  it('renders unmeasured and stale agent counts when non-zero', async () => {
    expect(formatMeasurementAgentCounts(0, 0)).toBeNull();
    expect(formatMeasurementAgentCounts(2, 0)).toBe('2 unmeasured');
    expect(formatMeasurementAgentCounts(0, 1)).toBe('1 stale');
    expect(formatMeasurementAgentCounts(3, 2)).toBe('3 unmeasured · 2 stale');

    const el = document.createElement(
      'scion-measurement-status-badge'
    ) as ScionMeasurementStatusBadge;
    el.status = 'stale';
    el.unmeasuredAgents = 3;
    el.staleAgents = 2;
    container.appendChild(el);
    await el.updateComplete;

    const counts = el.shadowRoot!.querySelector('[data-testid="measurement-status-counts"]');
    expect(counts?.textContent?.trim()).toBe('3 unmeasured · 2 stale');
  });
});
