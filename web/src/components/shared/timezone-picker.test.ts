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
 * scion-timezone-picker — unit tests.
 *
 * Covers: default rendering, the "empty" entry (its emitted value, label
 * configurability), local filtering, keyboard navigation, mouse selection,
 * typed-value commit on blur, and external value resets. Modelled on
 * `scion-project-picker`'s tests, with a static client-side list in place
 * of a debounced network search.
 */

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let TimezonePickerCtor: any;

beforeAll(async () => {
  const mod = await import('./timezone-picker.js');
  TimezonePickerCtor = mod.ScionTimezonePicker;
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function createElement(): Promise<InstanceType<typeof TimezonePickerCtor>> {
  const el = new TimezonePickerCtor();
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-timezone-picker', () => {
  it('renders with default label and placeholder', async () => {
    const el = await createElement();
    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input).not.toBeNull();
    expect(input?.getAttribute('label')).toBe('Timezone');
    expect(input?.getAttribute('placeholder')).toContain('timezone');
  });

  it('has combobox ARIA role on input', async () => {
    const el = await createElement();
    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input?.getAttribute('role')).toBe('combobox');
    expect(input?.getAttribute('aria-expanded')).toBe('false');
  });

  it('displays the current value as the input text', async () => {
    const el = await createElement();
    el.value = 'Asia/Tokyo';
    el.requestUpdate();
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input?.getAttribute('value')).toBe('Asia/Tokyo');
  });

  it('omits the empty entry when empty-label is unset', async () => {
    const el = await createElement();
    (el as Record<string, unknown>)['searchOpen'] = true;
    el.requestUpdate();
    await el.updateComplete;

    const options = Array.from(el.shadowRoot?.querySelectorAll('.timezone-search-option') ?? []);
    expect(options.some((o) => o.textContent?.trim() === '')).toBe(false);
    // The default-rendered list should only contain real IANA-looking names.
    expect(options.length).toBeGreaterThan(0);
  });

  it('shows the empty-label entry first when configured, and selecting it emits ""', async () => {
    const el = await createElement();
    el.emptyLabel = 'UTC';
    el.requestUpdate();
    await el.updateComplete;

    const events: Array<{ timezone: string }> = [];
    el.addEventListener('timezone-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    (el as Record<string, unknown>)['searchOpen'] = true;
    el.requestUpdate();
    await el.updateComplete;

    const firstOption = el.shadowRoot?.querySelector('.timezone-search-option');
    expect(firstOption?.textContent?.trim()).toBe('UTC');

    (el as Record<string, (...args: unknown[]) => void>)['selectZone']('UTC');
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    expect(events[events.length - 1].timezone).toBe('');
  });

  // tz-refactor task 12 review round 1, R1-5: listTimeZones() always
  // contains the real "UTC", so empty-label="UTC" must not produce two
  // "UTC" rows in the dropdown.
  it('does not duplicate a real zone name that collides with empty-label', async () => {
    const el = await createElement();
    el.emptyLabel = 'UTC';
    (el as Record<string, unknown>)['searchOpen'] = true;
    el.requestUpdate();
    await el.updateComplete;

    const options = Array.from(el.shadowRoot?.querySelectorAll('.timezone-search-option') ?? []);
    const utcRows = options.filter((o) => o.textContent?.trim() === 'UTC');
    expect(utcRows.length).toBe(1);
  });

  // tz-refactor task 12 review round 1, R1-4: a stored alias Intl's
  // canonical list omits (listTimeZones() has "Asia/Katmandu", not
  // "Asia/Kathmandu") must still be listed when the field is focused,
  // instead of the dropdown claiming no match for the field's own value.
  it('lists a stored alias value even though it is absent from the canonical zone list', async () => {
    const el = await createElement();
    el.value = 'Asia/Kathmandu';
    el.requestUpdate();
    await el.updateComplete;
    expect((el as Record<string, unknown>)['searchQuery']).toBe('Asia/Kathmandu');

    (el as Record<string, unknown>)['searchOpen'] = true;
    el.requestUpdate();
    await el.updateComplete;

    const filtered = (el as Record<string, unknown>)['filteredZones'] as string[];
    expect(filtered).toContain('Asia/Kathmandu');
    const options = Array.from(el.shadowRoot?.querySelectorAll('.timezone-search-option') ?? []);
    expect(options.some((o) => o.textContent?.trim() === 'Asia/Kathmandu')).toBe(true);
  });

  it('typing a full, valid alias not in the canonical list still offers it', async () => {
    const el = await createElement();
    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'Asia/Kolkata' },
    } as unknown as Event);
    await el.updateComplete;

    const filtered = (el as Record<string, unknown>)['filteredZones'] as string[];
    expect(filtered).toContain('Asia/Kolkata');
  });

  // tz-refactor task 12 review round 2, R2-2: a lowercase alias used to
  // slip through isValidTimeZone and get prepended as if it were a real,
  // selectable zone name; Go's time.LoadLocation rejects it.
  it('typing a lowercase alias Go rejects does not offer it', async () => {
    const el = await createElement();
    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'asia/kolkata' },
    } as unknown as Event);
    await el.updateComplete;

    const filtered = (el as Record<string, unknown>)['filteredZones'] as string[];
    expect(filtered).not.toContain('asia/kolkata');
  });

  it('a known alternate-name search term surfaces its canonical zone (Kolkata, Kyiv, Kathmandu)', async () => {
    const el = await createElement();

    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'Kolkata' },
    } as unknown as Event);
    await el.updateComplete;
    expect((el as Record<string, unknown>)['filteredZones'] as string[]).toContain('Asia/Calcutta');

    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'Kyiv' },
    } as unknown as Event);
    await el.updateComplete;
    expect((el as Record<string, unknown>)['filteredZones'] as string[]).toContain('Europe/Kiev');

    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'Kathmandu' },
    } as unknown as Event);
    await el.updateComplete;
    expect((el as Record<string, unknown>)['filteredZones'] as string[]).toContain('Asia/Katmandu');
  });

  // tz-refactor task 12 review round 1, R1-7: selecting from the dropdown
  // must not leave the field looking like it needs to be re-resolved (the
  // previous unconditional reset in willUpdate was a no-op dressed up as a
  // guard; this exercises the actual sequence that matters — select, then
  // the parent round-trips the same value back down as a prop update).
  it('keeps the selected display text when the parent round-trips the same value back as a prop', async () => {
    const el = await createElement();
    (el as Record<string, (...args: unknown[]) => void>)['selectZone']('Asia/Tokyo');
    await el.updateComplete;
    expect((el as Record<string, unknown>)['searchQuery']).toBe('Asia/Tokyo');

    // Simulate the parent applying the emitted value back as a prop, as
    // admin-server-config.ts's @timezone-change handler does.
    el.value = 'Asia/Tokyo';
    el.requestUpdate();
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchQuery']).toBe('Asia/Tokyo');
  });

  // tz-refactor task 12 review round 2, R2-3: a regression from R1-7's fix.
  // selectedViaDropdown stayed true after a selection (by design, to gate
  // the blur handler), but willUpdate's resync was gated on that same flag,
  // so once ANY dropdown selection had been made, every later external
  // `.value` change was silently ignored until the user typed or cleared —
  // including unrelated changes made well after the selection, not just the
  // selection's own round-trip.
  it('resyncs on an external value change after a dropdown selection, to a different zone', async () => {
    const el = await createElement();
    (el as Record<string, (...args: unknown[]) => void>)['selectZone']('Asia/Tokyo');
    await el.updateComplete;
    expect((el as Record<string, unknown>)['searchQuery']).toBe('Asia/Tokyo');

    // A later, unrelated external change — not the selection's own
    // round-trip — must still be reflected.
    el.value = 'Europe/Berlin';
    el.requestUpdate();
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchQuery']).toBe('Europe/Berlin');
  });

  it('resyncs to the empty entry on an external value change to "" after a dropdown selection', async () => {
    const el = await createElement();
    el.emptyLabel = 'Auto';
    el.requestUpdate();
    await el.updateComplete;

    (el as Record<string, (...args: unknown[]) => void>)['selectZone']('Asia/Tokyo');
    await el.updateComplete;
    expect((el as Record<string, unknown>)['searchQuery']).toBe('Asia/Tokyo');

    // An external change to a different zone first (a real property change —
    // `value` starts at '' by default, so setting it straight to '' here
    // would be a no-op Lit never reports as "changed").
    el.value = 'Europe/Berlin';
    el.requestUpdate();
    await el.updateComplete;
    expect((el as Record<string, unknown>)['searchQuery']).toBe('Europe/Berlin');

    // ...then externally cleared.
    el.value = '';
    el.requestUpdate();
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchQuery']).toBe('Auto');
  });

  it('filters the list by the typed substring, case-insensitively', async () => {
    const el = await createElement();
    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'tokyo' },
    } as unknown as Event);
    await el.updateComplete;

    const filtered = (el as Record<string, unknown>)['filteredZones'] as string[];
    expect(filtered.length).toBeGreaterThan(0);
    expect(filtered.every((z) => z.toLowerCase().includes('tokyo'))).toBe(true);
    expect(filtered).toContain('Asia/Tokyo');
  });

  it('emits the selected zone on dropdown selection', async () => {
    const el = await createElement();
    const events: Array<{ timezone: string }> = [];
    el.addEventListener('timezone-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    (el as Record<string, (...args: unknown[]) => void>)['selectZone']('Europe/Berlin');
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    expect(events[events.length - 1].timezone).toBe('Europe/Berlin');
    expect((el as Record<string, unknown>)['searchOpen']).toBe(false);
  });

  it('keyboard ArrowDown moves active descendant, Enter selects it', async () => {
    const el = await createElement();
    const events: Array<{ timezone: string }> = [];
    el.addEventListener('timezone-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'America/' },
    } as unknown as Event);
    await el.updateComplete;

    (el as Record<string, (...args: unknown[]) => void>)['handleKeydown'](
      new KeyboardEvent('keydown', { key: 'ArrowDown' })
    );
    await el.updateComplete;
    expect((el as Record<string, unknown>)['activeDescendantIndex']).toBe(0);

    (el as Record<string, (...args: unknown[]) => void>)['handleKeydown'](
      new KeyboardEvent('keydown', { key: 'ArrowDown' })
    );
    await el.updateComplete;
    expect((el as Record<string, unknown>)['activeDescendantIndex']).toBe(1);

    const filtered = (el as Record<string, unknown>)['filteredZones'] as string[];

    (el as Record<string, (...args: unknown[]) => void>)['handleKeydown'](
      new KeyboardEvent('keydown', { key: 'Enter' })
    );
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    expect(events[events.length - 1].timezone).toBe(filtered[1]);
  });

  it('keyboard Escape closes the dropdown', async () => {
    const el = await createElement();
    (el as Record<string, unknown>)['searchOpen'] = true;
    el.requestUpdate();
    await el.updateComplete;

    (el as Record<string, (...args: unknown[]) => void>)['handleKeydown'](
      new KeyboardEvent('keydown', { key: 'Escape' })
    );
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchOpen']).toBe(false);
  });

  it('commits the typed value on blur when no dropdown selection was made', async () => {
    const el = await createElement();
    const events: Array<{ timezone: string }> = [];
    el.addEventListener('timezone-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    (el as Record<string, unknown>)['searchQuery'] = 'Asia/Kathmandu';
    (el as Record<string, unknown>)['selectedViaDropdown'] = false;
    (el as Record<string, (...args: unknown[]) => void>)['commitTyped']();
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    expect(events[events.length - 1].timezone).toBe('Asia/Kathmandu');
  });

  it('shows empty state when no zones match the query', async () => {
    const el = await createElement();
    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'Not_A_Real_Zone_Prefix_Zzz' },
    } as unknown as Event);
    await el.updateComplete;

    const empty = el.shadowRoot?.querySelector('.timezone-search-empty');
    expect(empty).not.toBeNull();
    expect(empty?.textContent?.trim()).toContain('No matching timezones');
  });

  it('dropdown has listbox role for accessibility', async () => {
    const el = await createElement();
    (el as Record<string, unknown>)['searchOpen'] = true;
    el.requestUpdate();
    await el.updateComplete;

    const listbox = el.shadowRoot?.querySelector('[role="listbox"]');
    expect(listbox).not.toBeNull();
    const options = el.shadowRoot?.querySelectorAll('[role="option"]');
    expect(options?.length).toBeGreaterThan(0);
  });

  it('resets the input text when value is externally cleared', async () => {
    const el = await createElement();
    el.value = 'Asia/Tokyo';
    el.requestUpdate();
    await el.updateComplete;
    expect((el as Record<string, unknown>)['searchQuery']).toBe('Asia/Tokyo');

    el.value = '';
    el.requestUpdate();
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchQuery']).toBe('');
  });

  it('disabled prop disables the input', async () => {
    const el = await createElement();
    el.disabled = true;
    el.requestUpdate();
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input?.hasAttribute('disabled')).toBe(true);
  });
});
