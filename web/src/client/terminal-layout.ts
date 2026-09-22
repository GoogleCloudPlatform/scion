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
 * Pure layout state module for the terminal workspace.
 *
 * Tracks independent preset assignments (single, two-columns, two-rows, four).
 * Navigation (select/rail clicks) modifies ONLY the single-pane slot.
 * Opening a new agent (open()) fills the first empty slot in the active multi
 * preset when capacity is available, or overflows to single when at capacity.
 * Multi-pane presets are also changed through explicit placement.
 * No DOM, Lit, xterm, or session-registry dependencies.
 */

export type TerminalLayout = 'single' | 'two-columns' | 'two-rows' | 'four';
export type TerminalSlot = string | null;

export interface TerminalLayoutState {
  readonly active: TerminalLayout;
  readonly single: readonly [TerminalSlot];
  readonly twoColumns: readonly [TerminalSlot, TerminalSlot];
  readonly twoRows: readonly [TerminalSlot, TerminalSlot];
  readonly four: readonly [TerminalSlot, TerminalSlot, TerminalSlot, TerminalSlot];
}

export type TerminalLayoutListener = (state: TerminalLayoutState) => void;

function initialState(): TerminalLayoutState {
  return {
    active: 'single',
    single: [null],
    twoColumns: [null, null],
    twoRows: [null, null],
    four: [null, null, null, null],
  };
}

/** Slot counts per preset, used for bounds validation. */
export const SLOT_COUNTS: Record<TerminalLayout, number> = {
  single: 1,
  'two-columns': 2,
  'two-rows': 2,
  four: 4,
};

/**
 * Manages layout preset assignments for the terminal workspace.
 *
 * Each preset independently retains its slot assignments. The same session key
 * may appear in multiple presets simultaneously. Within a single preset,
 * duplicate keys are prevented by swapping.
 */
export class TerminalLayoutManager {
  private state: TerminalLayoutState = initialState();
  private zoomed: string | null = null;
  private readonly listeners = new Set<TerminalLayoutListener>();

  /** Returns the current layout state snapshot. */
  getState(): TerminalLayoutState {
    return this.state;
  }

  /** Returns the currently zoomed session key, or null if not zoomed. */
  getZoomed(): string | null {
    return this.zoomed;
  }

  /**
   * Subscribe to layout state changes.
   * The listener receives the current state immediately on subscribe.
   * Returns an unsubscribe function.
   */
  subscribe(listener: TerminalLayoutListener): () => void {
    this.listeners.add(listener);
    listener(this.state);
    return () => {
      this.listeners.delete(listener);
    };
  }

  /**
   * Open/add a new session. Checks whether the current multi preset is at
   * capacity (every slot filled). When at capacity, overflows to single so the
   * new agent is immediately visible. When the active preset is a multi layout
   * with available (null) slots, the new agent is placed into the first empty
   * slot so it becomes immediately visible. If the session is already assigned
   * in the active preset, delegates to select() without duplicating.
   * In single mode, delegates to select().
   *
   * Multi-pane slot assignments are NEVER cleared on overflow — the user can
   * switch back to the prior preset and see the original grid.
   */
  open(sessionKey: string): void {
    const preset = this.state.active;
    const slots = this.getPresetSlots(preset);
    const atCapacity = preset !== 'single' && slots.every((s) => s !== null);

    if (atCapacity) {
      // Overflow: switch to single to show the new agent.
      // Multi-pane assignments are preserved.
      this.zoomed = null;
      this.commit({
        ...this.state,
        active: 'single',
        single: [sessionKey],
      });
    } else if (preset !== 'single') {
      // Multi preset with available slot(s).
      // If the session is already placed in this preset, just select (no duplicate).
      if (slots.includes(sessionKey)) {
        this.select(sessionKey);
        return;
      }
      // Fill the first empty (null) slot.
      const emptyIndex = slots.findIndex((s) => s === null);
      if (emptyIndex !== -1) {
        const updated = [...slots] as (string | null)[];
        updated[emptyIndex] = sessionKey;
        this.zoomed = null;
        this.commit({
          ...this.state,
          ...this.withPresetSlots(preset, updated),
          single: [sessionKey],
        });
      } else {
        // No empty slot found (shouldn't happen since not at capacity, but safe fallback).
        this.select(sessionKey);
      }
    } else {
      // Single mode: just select.
      this.select(sessionKey);
    }
  }

  /**
   * Select a session: sets single[0] without changing the active preset.
   *
   * Used for navigation between existing sessions (rail clicks, coordinator
   * requests). Unlike open(), select() NEVER mutates multi-pane preset
   * assignments (twoColumns, twoRows, four). The user switches presets
   * explicitly via setLayout().
   */
  select(sessionKey: string): void {
    // Cancel zoom on any navigation action.
    this.zoomed = null;
    this.commit({
      ...this.state,
      single: [sessionKey],
    });
  }

  /**
   * Switch to a layout preset. Does NOT change any slot assignments.
   * Restores that preset's saved assignments.
   */
  setLayout(preset: TerminalLayout): void {
    if (this.state.active === preset) return;
    // Cancel zoom when switching layouts.
    this.zoomed = null;
    this.commit({
      ...this.state,
      active: preset,
    });
  }

  /**
   * Explicitly place a session into a specific slot of a specific preset.
   *
   * If the session already occupies another slot in the same preset, the two
   * slots are swapped. Same-slot placement is a no-op (no notification).
   * Only modifies the target preset's assignments.
   */
  place(sessionKey: string, preset: TerminalLayout, slotIndex: number): void {
    const count = SLOT_COUNTS[preset];
    if (!Number.isInteger(slotIndex) || slotIndex < 0 || slotIndex >= count) {
      throw new RangeError(
        `Slot index ${slotIndex} is out of range for preset '${preset}' (0..${count - 1}).`
      );
    }

    const slots = this.getPresetSlots(preset);

    // Same-slot no-op: session is already at the target index.
    if (slots[slotIndex] === sessionKey) return;

    const updated = [...slots] as (string | null)[];
    const existingIndex = updated.indexOf(sessionKey);

    if (existingIndex !== -1) {
      // Swap: move the current occupant of the target slot to the session's old slot.
      updated[existingIndex] = updated[slotIndex];
    }
    updated[slotIndex] = sessionKey;

    this.commit({
      ...this.state,
      ...this.withPresetSlots(preset, updated),
    });
  }

  /**
   * Close a session: removes it from ALL presets that reference it.
   * Sets those slots to null. Does NOT substitute another session.
   */
  close(sessionKey: string): void {
    let changed = false;
    const clearSlots = (slots: readonly TerminalSlot[]): TerminalSlot[] => {
      return slots.map((slot) => {
        if (slot === sessionKey) {
          changed = true;
          return null;
        }
        return slot;
      });
    };

    const next: TerminalLayoutState = {
      active: this.state.active,
      single: clearSlots(this.state.single) as [TerminalSlot],
      twoColumns: clearSlots(this.state.twoColumns) as [TerminalSlot, TerminalSlot],
      twoRows: clearSlots(this.state.twoRows) as [TerminalSlot, TerminalSlot],
      four: clearSlots(this.state.four) as [TerminalSlot, TerminalSlot, TerminalSlot, TerminalSlot],
    };

    // Also clear zoom if the zoomed session is the one being closed.
    if (this.zoomed === sessionKey) {
      this.zoomed = null;
      changed = true;
    }

    if (changed) this.commit(next);
  }

  /**
   * Temporarily show one pane full-screen. MUST NOT overwrite any saved
   * assignments. Zoom state is stored separately.
   */
  zoom(sessionKey: string): void {
    if (this.zoomed === sessionKey) return;
    this.zoomed = sessionKey;
    this.notify();
  }

  /** Exit zoom mode. All preset assignments remain unchanged. */
  unzoom(): void {
    if (this.zoomed === null) return;
    this.zoomed = null;
    this.notify();
  }

  /**
   * Returns the session keys visible in the active preset.
   * When zoomed, returns only the zoomed session key.
   * Null slots are included to preserve positional information.
   */
  getVisibleSlots(): readonly TerminalSlot[] {
    if (this.zoomed !== null) return [this.zoomed];
    return this.getPresetSlots(this.state.active);
  }

  /** Read the slot array for a given preset. */
  private getPresetSlots(preset: TerminalLayout): readonly TerminalSlot[] {
    switch (preset) {
      case 'single':
        return this.state.single;
      case 'two-columns':
        return this.state.twoColumns;
      case 'two-rows':
        return this.state.twoRows;
      case 'four':
        return this.state.four;
    }
  }

  /** Build a partial state update for one preset's slots. */
  private withPresetSlots(
    preset: TerminalLayout,
    slots: TerminalSlot[]
  ): Partial<TerminalLayoutState> {
    switch (preset) {
      case 'single':
        return { single: slots as [TerminalSlot] };
      case 'two-columns':
        return { twoColumns: slots as [TerminalSlot, TerminalSlot] };
      case 'two-rows':
        return { twoRows: slots as [TerminalSlot, TerminalSlot] };
      case 'four':
        return {
          four: slots as [TerminalSlot, TerminalSlot, TerminalSlot, TerminalSlot],
        };
    }
  }

  /** Replace state and notify listeners. */
  private commit(next: TerminalLayoutState): void {
    this.state = next;
    this.notify();
  }

  /** Notify all listeners with the current state snapshot. */
  private notify(): void {
    const snapshot = this.state;
    for (const listener of this.listeners) listener(snapshot);
  }

  /**
   * Restore layout from externally provided state (e.g. URL query params).
   * Sets the active preset and slot assignments for the active preset.
   * Also sets single[0] to the first non-null slot for rail highlighting.
   * Does NOT trigger overflow logic — slots are set directly.
   */
  restore(preset: TerminalLayout, slots: readonly TerminalSlot[]): void {
    const expected = SLOT_COUNTS[preset];
    // Pad or truncate to the expected slot count
    const normalized = Array.from({ length: expected }, (_, i) => slots[i] ?? null);
    const firstOccupied = normalized.find((s) => s !== null) ?? null;
    this.zoomed = null;
    this.commit({
      ...this.state,
      active: preset,
      ...this.withPresetSlots(preset, normalized),
      single: [firstOccupied ?? this.state.single[0]],
    });
  }
}

// ────────────────────────────────────────────────────────────────────────────
// URL Layout Encoding/Decoding (#1715)
//
// Encodes the active layout preset and ordered slot assignments (by agent ID)
// into URL query parameters. Format:
//   ?lv=1&lp=<preset>&s0=<agentId>&s1=<agentId>&...
//
// - lv: layout version (currently 1). Unknown versions cause fallback.
// - lp: layout preset name (single, two-columns, two-rows, four).
// - s0..s3: ordered slot agent IDs. Empty string = empty slot.
//
// Design invariants:
// - Versioned: unknown lv → ignore layout query entirely
// - Canonical: same logical state → same URL string
// - Max 4 slot params (matching four-pane layout)
// - Agent IDs only — no session keys, no transient state
// ────────────────────────────────────────────────────────────────────────────

/** Current layout URL format version. */
export const LAYOUT_URL_VERSION = '1';

/** UUID format pattern for agent ID validation. */
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Valid preset names for URL encoding. */
const VALID_PRESETS = new Set<string>(['single', 'two-columns', 'two-rows', 'four']);

/** Result of parsing layout state from URL query parameters. */
export interface LayoutUrlState {
  preset: TerminalLayout;
  /** Ordered slot agent IDs. null = empty slot. Length matches the preset. */
  slots: ReadonlyArray<string | null>;
}

/**
 * Serialize layout state to URL query parameters.
 * Returns a URLSearchParams object (caller can merge with existing params).
 *
 * @param preset  Active layout preset
 * @param agentIds  Ordered agent IDs for the active preset slots. null = empty.
 */
export function serializeLayoutUrl(
  preset: TerminalLayout,
  agentIds: ReadonlyArray<string | null>
): URLSearchParams {
  const params = new URLSearchParams();
  params.set('lv', LAYOUT_URL_VERSION);
  params.set('lp', preset);
  const count = SLOT_COUNTS[preset];
  for (let i = 0; i < count; i++) {
    params.set(`s${i}`, agentIds[i] ?? '');
  }
  return params;
}

/**
 * Parse layout state from URL query parameters.
 * Returns null on any validation failure (unknown version, malformed data).
 * Deduplicates agent IDs (keeps first occurrence).
 *
 * @param search  The URL search string (e.g. window.location.search)
 */
export function parseLayoutUrl(search: string): LayoutUrlState | null {
  const params = new URLSearchParams(search);

  // Version check: unknown version → ignore entirely
  const version = params.get('lv');
  if (version !== LAYOUT_URL_VERSION) return null;

  // Preset validation
  const preset = params.get('lp');
  if (!preset || !VALID_PRESETS.has(preset)) return null;
  const layout = preset as TerminalLayout;

  // Parse slot agent IDs
  const count = SLOT_COUNTS[layout];
  const seen = new Set<string>();
  const slots: Array<string | null> = [];

  for (let i = 0; i < count; i++) {
    const value = params.get(`s${i}`);
    if (!value) {
      slots.push(null);
      continue;
    }
    // Validate UUID format
    if (!UUID_RE.test(value)) {
      slots.push(null);
      continue;
    }
    // Normalize to lowercase for canonical form
    const normalized = value.toLowerCase();
    // Deduplicate: keep first occurrence
    if (seen.has(normalized)) {
      slots.push(null);
      continue;
    }
    seen.add(normalized);
    slots.push(normalized);
  }

  return { preset: layout, slots };
}

/**
 * Build a canonical URL path + query string for a terminal layout.
 * The path agent ID is the first non-null agent in the slots, or omitted
 * if all slots are empty.
 *
 * @param preset  Active layout preset
 * @param agentIds  Ordered agent IDs for the active preset slots
 * @param basePath  Base path prefix (default: '/terminals')
 */
export function buildLayoutUrl(
  preset: TerminalLayout,
  agentIds: ReadonlyArray<string | null>,
  basePath = '/terminals'
): string {
  const params = serializeLayoutUrl(preset, agentIds);
  const query = params.toString();
  return query ? `${basePath}?${query}` : basePath;
}
