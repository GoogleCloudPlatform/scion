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
 * Tracks independent preset assignments (single, two-columns, two-rows, four)
 * with protection: ordinary terminal opens and rail clicks modify ONLY the
 * single-pane slot. Multi-pane presets are changed exclusively through explicit
 * placement. No DOM, Lit, xterm, or session-registry dependencies.
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
const SLOT_COUNTS: Record<TerminalLayout, number> = {
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
   * Open/select a session: sets single[0] and active to 'single'.
   * NEVER mutates twoColumns, twoRows, or four.
   */
  open(sessionKey: string): void {
    this.select(sessionKey);
  }

  /**
   * Select a session: sets single[0] and active to 'single'.
   * NEVER mutates twoColumns, twoRows, or four.
   */
  select(sessionKey: string): void {
    // Cancel zoom on any navigation action.
    this.zoomed = null;
    this.commit({
      ...this.state,
      active: 'single',
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
}
