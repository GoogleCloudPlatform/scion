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
  it("does not shift an untouched field's instant after a late zone change and an edit to the other field", async () => {
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

// Review round 5, R5-1: a zone change while the editor is detached (or
// before it is ever connected) is invisible to DisplayZoneController, since
// its listener is only active while connected. connectedCallback already
// re-derives the cached strings in the *current* zone when it reconnects —
// but if it doesn't also refresh `_renderedZone`, willUpdate's first
// post-reconnect pass treats those freshly-current strings as if they were
// still in the stale (pre-detach) zone, and shifts them a second time.
describe('scion-access-boundary-schedule-editor — zone change while detached (review R5-1)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
  });

  it('does not double-shift an untouched field when the zone changes while detached and the editor reconnects', async () => {
    const el = await mount({
      notBefore: '2026-09-23T15:00:00.000Z',
      expiresAt: '2026-09-30T15:00:00.000Z',
    });

    el.remove();
    setPreferredTimeZone('Asia/Tokyo');
    document.body.appendChild(el); // reconnect — re-runs connectedCallback
    await el.updateComplete;

    // Same instant as at mount, displayed once in the new zone — not
    // shifted a second time by willUpdate treating it as still-UTC.
    expect(displayedValue(notBeforeInput(el))).toBe('2026-09-24T00:00');
    expect(label(el)).toContain('Asia/Tokyo');

    let detail: ScheduleChangeDetail | null = null;
    el.addEventListener('schedule-change', (e) => {
      detail = (e as CustomEvent<ScheduleChangeDetail>).detail;
    });
    const expiresInput = expiresAtInput(el);
    (expiresInput as unknown as { value: string }).value = '2026-10-01T09:00';
    expiresInput.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;

    expect(detail).not.toBeNull();
    expect(detail!.notBefore).toBe('2026-09-23T15:00:00.000Z');
    expect(detail!.expiresAt).toBe('2026-10-01T00:00:00.000Z');
  });

  it('does not double-shift when the zone changes between construction and the first connection', async () => {
    const el = document.createElement(
      'scion-access-boundary-schedule-editor'
    ) as ScionAccessBoundaryScheduleEditor;
    el.notBefore = '2026-09-23T15:00:00.000Z';
    el.expiresAt = '2026-09-30T15:00:00.000Z';

    setPreferredTimeZone('Asia/Tokyo'); // zone changes before the element ever connects
    document.body.appendChild(el);
    await el.updateComplete;

    expect(displayedValue(notBeforeInput(el))).toBe('2026-09-24T00:00');
    expect(label(el)).toContain('Asia/Tokyo');
  });
});

// Review round 6, R6-1: a regression in the R5-1 fix. connectedCallback
// marked _renderedZone as the current zone unconditionally, but only
// re-derived a cached string when its *backing prop* was set. A string with
// no backing prop (typed into an uncontrolled instance, with no host to
// round-trip it back through a prop) was left holding old-zone text while
// the tracker claimed it was already current-zone — so it was never
// rebased, and the next emit parsed it in the wrong zone.
describe('scion-access-boundary-schedule-editor — retained typed value with no backing prop across a detach (review R6-1)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    setPreferredTimeZone('');
  });

  it('rebases a typed value with no backing prop before marking the tracker current, across a detach/reconnect', async () => {
    // notBefore has a backing prop; expiresAt does not — simulating an
    // uncontrolled instance (no host listening on `schedule-change` to
    // round-trip the typed value back through the `expiresAt` prop).
    const el = await mount({ notBefore: '2026-09-23T15:00:00.000Z' });

    const expiresInput = expiresAtInput(el);
    (expiresInput as unknown as { value: string }).value = '2026-10-01T09:00';
    expiresInput.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;
    expect(displayedValue(expiresInput)).toBe('2026-10-01T09:00');

    el.remove();
    setPreferredTimeZone('Asia/Tokyo');
    document.body.appendChild(el); // reconnect — re-runs connectedCallback
    await el.updateComplete;

    // The untouched-by-props expiresAt must be rebased into the new zone
    // too, same as the prop-backed notBefore is.
    expect(displayedValue(notBeforeInput(el))).toBe('2026-09-24T00:00');
    expect(displayedValue(expiresAtInput(el))).toBe('2026-10-01T18:00');
    expect(label(el)).toContain('Asia/Tokyo');

    // Editing the (prop-backed) sibling field must emit the *rebased*
    // expiresAt instant, not the one its stale wall-clock text would give
    // if parsed in the new zone without ever having been rebased.
    let detail: ScheduleChangeDetail | null = null;
    el.addEventListener('schedule-change', (e) => {
      detail = (e as CustomEvent<ScheduleChangeDetail>).detail;
    });
    const notBeforeInputEl = notBeforeInput(el);
    (notBeforeInputEl as unknown as { value: string }).value = '2026-09-24T00:00';
    notBeforeInputEl.dispatchEvent(new Event('sl-input'));
    await el.updateComplete;

    expect(detail).not.toBeNull();
    expect(detail!.expiresAt).toBe('2026-10-01T09:00:00.000Z');
  });
});
