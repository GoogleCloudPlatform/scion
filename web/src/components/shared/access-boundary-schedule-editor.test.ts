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
 * Tests for <scion-access-boundary-schedule-editor>'s handling of a late
 * display-timezone arrival (tz-refactor task 11, review round 4, R4-1).
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';

await import('./access-boundary-schedule-editor.js');
type ScionAccessBoundaryScheduleEditor =
  import('./access-boundary-schedule-editor.js').ScionAccessBoundaryScheduleEditor;
type ScheduleChangeDetail = import('./access-boundary-schedule-editor.js').ScheduleChangeDetail;

async function mount(props: {
  notBefore?: string;
  expiresAt?: string;
}): Promise<ScionAccessBoundaryScheduleEditor> {
  const el = document.createElement(
    'scion-access-boundary-schedule-editor'
  ) as ScionAccessBoundaryScheduleEditor;
  if (props.notBefore) el.notBefore = props.notBefore;
  if (props.expiresAt) el.expiresAt = props.expiresAt;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function label(el: ScionAccessBoundaryScheduleEditor): string {
  return el.shadowRoot?.querySelector('.timezone-label')?.textContent?.trim() ?? '';
}

function notBeforeInput(el: ScionAccessBoundaryScheduleEditor): HTMLElement {
  return el.shadowRoot!.querySelector('#not-before')!;
}

function expiresAtInput(el: ScionAccessBoundaryScheduleEditor): HTMLElement {
  return el.shadowRoot!.querySelector('#expires-at')!;
}

/**
 * The rendered `datetime-local` value. The template binds it as an
 * *attribute* (`value=${...}`, not `.value=${...}`), and `sl-input` is
 * never upgraded to the real Shoelace element in this test environment, so
 * there is no `.value` property to reflect it — read the attribute instead.
 */
function displayedValue(input: HTMLElement): string | null {
  return input.getAttribute('value');
}

describe('scion-access-boundary-schedule-editor — late zone arrival (review R4-1)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
  });

  it('updates the "Times in" label when the zone changes after mount', async () => {
    const el = await mount({ notBefore: '2026-09-23T15:00:00.000Z' });
    expect(label(el)).toContain('UTC');

    setPreferredTimeZone('Asia/Tokyo');
    await el.updateComplete;

    expect(label(el)).toContain('Asia/Tokyo');
    expect(label(el)).not.toContain('UTC');
  });

  // The reviewer's exact repro: mount in UTC, the preference arrives late
  // (simulating a slow /auth/me resolving after first render, review R3-1),
  // then the user edits ONLY the expiration field. The untouched
  // `notBefore` must still round-trip to its original instant.
  it('does not shift an untouched field\'s instant after a late zone change and an edit to the other field', async () => {
    const el = await mount({
      notBefore: '2026-09-23T15:00:00.000Z',
      expiresAt: '2026-09-30T15:00:00.000Z',
    });

    setPreferredTimeZone('Asia/Tokyo');
    await el.updateComplete;

    // The label must reflect the new zone before any edit happens, so what
    // the user sees next to the input matches how it is about to be parsed.
    expect(label(el)).toContain('Asia/Tokyo');

    // Edit only the expiration field.
    let detail: ScheduleChangeDetail | null = null;
    el.addEventListener('schedule-change', (e) => {
      detail = (e as CustomEvent<ScheduleChangeDetail>).detail;
    });
    const expiresInput = expiresAtInput(el);
    (expiresInput as unknown as { value: string }).value = '2026-10-01T09:00';
    expiresInput.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;

    expect(detail).not.toBeNull();
    // The untouched notBefore must still be the exact original instant.
    expect(detail!.notBefore).toBe('2026-09-23T15:00:00.000Z');
    // The edited expiresAt is parsed in the zone the (now-updated) label
    // shows — Asia/Tokyo — not the zone at mount.
    expect(detail!.expiresAt).toBe('2026-10-01T00:00:00.000Z');
  });

  it('re-derives the displayed (not just emitted) value of an untouched field after a late zone change', async () => {
    const el = await mount({ notBefore: '2026-09-23T15:00:00.000Z' });
    // At mount, UTC: 2026-09-23T15:00:00Z -> "2026-09-23T15:00" local.
    expect(displayedValue(notBeforeInput(el))).toBe('2026-09-23T15:00');

    setPreferredTimeZone('Asia/Tokyo');
    await el.updateComplete;

    // Same instant, now displayed in Tokyo: 2026-09-24T00:00 JST.
    expect(displayedValue(notBeforeInput(el))).toBe('2026-09-24T00:00');
  });

  it('round-trips an in-progress (uncommitted) edit through a zone change without changing its instant', async () => {
    const el = await mount({});
    // No notBefore prop — hasSchedule starts false, so type into the fields
    // via the toggle first.
    (el as unknown as { hasSchedule: boolean }).hasSchedule = true;
    await el.updateComplete;

    const input = notBeforeInput(el);
    (input as unknown as { value: string }).value = '2026-09-23T09:00';
    input.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;

    expect(displayedValue(notBeforeInput(el))).toBe('2026-09-23T09:00');

    setPreferredTimeZone('Asia/Tokyo');
    await el.updateComplete;

    // 09:00 UTC (what the user actually typed, interpreted in the zone
    // shown at the time) re-displayed in Tokyo (+9h) is 18:00 the same day.
    expect(displayedValue(notBeforeInput(el))).toBe('2026-09-23T18:00');
  });
});
