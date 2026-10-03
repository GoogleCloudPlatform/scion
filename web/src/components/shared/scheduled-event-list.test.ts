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
 * Tests for <scion-scheduled-event-list>'s create-dialog zone label
 * (tz-refactor task 11, review round 4, R4-1).
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';

await import('./scheduled-event-list.js');
type ScionScheduledEventList = import('./scheduled-event-list.js').ScionScheduledEventList;

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

describe('scion-scheduled-event-list create dialog zone label (review R4-1)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
  });

  it('updates the "Times in" help text when the effective zone changes after mount', async () => {
    // No projectId, so connectedCallback's loadEvents() returns immediately
    // without issuing a fetch (and `loading` never flips to `false`) —
    // `compact` mode renders the dialog regardless of the loading state,
    // unlike the full-page layout, which gates it behind `!loading`.
    const el = document.createElement('scion-scheduled-event-list') as ScionScheduledEventList;
    el.compact = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const comp = el as AnyEl;
    comp.dialogOpen = true;
    comp.dialogTimingMode = 'at';
    await comp.updateComplete;

    const dateInput = () => el.shadowRoot!.querySelector('sl-input[label="Date & Time"]')!;
    expect(dateInput().getAttribute('help-text')).toBe('Times in: UTC');

    setPreferredTimeZone('Asia/Tokyo');
    await comp.updateComplete;

    expect(dateInput().getAttribute('help-text')).toBe('Times in: Asia/Tokyo');
  });
});
