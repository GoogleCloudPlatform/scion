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

import { describe, expect, it, vi } from 'vitest';
import { TerminalLayoutManager, type TerminalLayoutState } from './terminal-layout.js';

function manager(): TerminalLayoutManager {
  return new TerminalLayoutManager();
}

describe('TerminalLayoutManager', () => {
  describe('initial state', () => {
    it('starts with all-null slots and active single', () => {
      const m = manager();
      const state = m.getState();
      expect(state.active).toBe('single');
      expect(state.single).toEqual([null]);
      expect(state.twoColumns).toEqual([null, null]);
      expect(state.twoRows).toEqual([null, null]);
      expect(state.four).toEqual([null, null, null, null]);
    });

    it('starts with no zoom', () => {
      const m = manager();
      expect(m.getZoomed()).toBeNull();
    });
  });

  describe('open / select', () => {
    it('sets single[0] without changing active preset', () => {
      const m = manager();
      m.open('agent-a');
      expect(m.getState().single).toEqual(['agent-a']);
      // Initial active is 'single', open does not change it
      expect(m.getState().active).toBe('single');
    });

    it('select is an alias for open', () => {
      const m = manager();
      m.select('agent-b');
      expect(m.getState().single).toEqual(['agent-b']);
      expect(m.getState().active).toBe('single');
    });

    it('replaces the previous single-pane session', () => {
      const m = manager();
      m.open('agent-a');
      m.open('agent-b');
      expect(m.getState().single).toEqual(['agent-b']);
    });

    it('does not mutate multi-pane presets when active is single', () => {
      const m = manager();
      // Set up four-pane layout with explicit placements
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.place('agent-c', 'four', 2);
      m.place('agent-d', 'four', 3);
      m.place('agent-x', 'two-columns', 0);
      m.place('agent-y', 'two-columns', 1);
      m.place('agent-p', 'two-rows', 0);
      m.place('agent-q', 'two-rows', 1);

      // Open a new session — active stays 'single' (initial)
      m.open('agent-e');

      expect(m.getState().single).toEqual(['agent-e']);
      // active is still whatever it was before open (initial 'single')
      expect(m.getState().active).toBe('single');
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', 'agent-d']);
      expect(m.getState().twoColumns).toEqual(['agent-x', 'agent-y']);
      expect(m.getState().twoRows).toEqual(['agent-p', 'agent-q']);
    });

    it('preserves active multi-pane preset on open and fills empty slot (#1701)', () => {
      const m = manager();
      m.setLayout('four');
      expect(m.getState().active).toBe('four');
      m.open('agent-a');
      // open must NOT clobber a user-chosen multi-pane preset
      expect(m.getState().active).toBe('four');
      expect(m.getState().single).toEqual(['agent-a']);
      // Agent is placed into the first empty slot of the active preset
      expect(m.getState().four).toEqual(['agent-a', null, null, null]);
    });

    it('preserves active two-columns preset on select', () => {
      const m = manager();
      m.setLayout('two-columns');
      expect(m.getState().active).toBe('two-columns');
      m.select('agent-a');
      expect(m.getState().active).toBe('two-columns');
      expect(m.getState().single).toEqual(['agent-a']);
    });

    it('preserves active two-rows preset on select', () => {
      const m = manager();
      m.setLayout('two-rows');
      m.select('agent-a');
      expect(m.getState().active).toBe('two-rows');
    });

    it('cancels zoom on open', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.setLayout('four');
      m.zoom('agent-a');
      expect(m.getZoomed()).toBe('agent-a');
      m.open('agent-b');
      expect(m.getZoomed()).toBeNull();
    });

    it('open places agent in first empty slot of four-pane preset (nonzero index)', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.setLayout('four');
      // Slots 0 and 1 occupied, 2 and 3 empty
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', null, null]);

      m.open('agent-c');
      // Agent placed into first empty slot (index 2)
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', null]);
      expect(m.getState().active).toBe('four');
      expect(m.getState().single[0]).toBe('agent-c');
    });

    it('open places agent in first empty slot of twoColumns', () => {
      const m = manager();
      m.place('agent-a', 'two-columns', 0);
      m.setLayout('two-columns');
      // Slot 0 occupied, slot 1 empty
      expect(m.getState().twoColumns).toEqual(['agent-a', null]);

      m.open('agent-b');
      expect(m.getState().twoColumns).toEqual(['agent-a', 'agent-b']);
      expect(m.getState().active).toBe('two-columns');
      expect(m.getState().single[0]).toBe('agent-b');
    });

    it('open places agent in first empty slot of twoRows', () => {
      const m = manager();
      m.place('agent-a', 'two-rows', 0);
      m.setLayout('two-rows');
      expect(m.getState().twoRows).toEqual(['agent-a', null]);

      m.open('agent-b');
      expect(m.getState().twoRows).toEqual(['agent-a', 'agent-b']);
      expect(m.getState().active).toBe('two-rows');
      expect(m.getState().single[0]).toBe('agent-b');
    });

    it('open in single mode does not attempt slot placement', () => {
      const m = manager();
      // Active is single (default)
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'two-columns', 0);

      m.open('agent-c');
      // Single mode: select() behavior, no slot placement in multi presets
      expect(m.getState().active).toBe('single');
      expect(m.getState().single[0]).toBe('agent-c');
      expect(m.getState().four).toEqual(['agent-a', null, null, null]);
      expect(m.getState().twoColumns).toEqual(['agent-b', null]);
    });

    it('open fills slots sequentially across multiple opens', () => {
      const m = manager();
      m.setLayout('four');

      m.open('agent-a');
      expect(m.getState().four).toEqual(['agent-a', null, null, null]);

      m.open('agent-b');
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', null, null]);

      m.open('agent-c');
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', null]);

      m.open('agent-d');
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', 'agent-d']);

      // All slots filled, active still four
      expect(m.getState().active).toBe('four');
    });

    it('open with key already in active preset does not duplicate', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 2);
      m.setLayout('four');
      expect(m.getState().four).toEqual(['agent-a', null, 'agent-b', null]);

      // Open agent-a again — already in four's slots
      m.open('agent-a');
      // No duplication: four is unchanged, just single[0] updated
      expect(m.getState().four).toEqual(['agent-a', null, 'agent-b', null]);
      expect(m.getState().active).toBe('four');
      expect(m.getState().single[0]).toBe('agent-a');
    });
  });

  describe('overflow: open at capacity', () => {
    it('overflows to single when four-pane is at capacity', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.place('agent-c', 'four', 2);
      m.place('agent-d', 'four', 3);
      m.setLayout('four');
      expect(m.getState().active).toBe('four');
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', 'agent-d']);

      // Fifth agent open — overflow to single, showing the new agent
      m.open('agent-e');

      expect(m.getState().active).toBe('single');
      expect(m.getState().single[0]).toBe('agent-e');
      // four-pane assignments preserved
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', 'agent-d']);
    });

    it('restores four-pane grid after overflow to single', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.place('agent-c', 'four', 2);
      m.place('agent-d', 'four', 3);
      m.setLayout('four');

      m.open('agent-e');
      expect(m.getState().active).toBe('single');

      // Switch back to four — grid restored
      m.setLayout('four');
      expect(m.getState().active).toBe('four');
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', 'agent-d']);
    });

    it('does not overflow when multi preset has available slots and fills first empty', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      // Only 1 of 4 slots filled
      m.setLayout('four');
      expect(m.getState().active).toBe('four');

      m.open('agent-b');
      // Not at capacity — stays in four
      expect(m.getState().active).toBe('four');
      expect(m.getState().single[0]).toBe('agent-b');
      // Agent placed into the first empty slot (index 1)
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', null, null]);
    });

    it('does not overflow when four-pane has all null slots and fills slot 0', () => {
      const m = manager();
      m.setLayout('four');
      expect(m.getState().active).toBe('four');
      expect(m.getState().four).toEqual([null, null, null, null]);

      m.open('agent-a');
      // Not at capacity (nulls are empty slots) — stays in four
      expect(m.getState().active).toBe('four');
      expect(m.getState().single[0]).toBe('agent-a');
      // Agent placed into the first empty slot (index 0)
      expect(m.getState().four).toEqual(['agent-a', null, null, null]);
    });

    it('overflows two-columns at capacity', () => {
      const m = manager();
      m.place('agent-a', 'two-columns', 0);
      m.place('agent-b', 'two-columns', 1);
      m.setLayout('two-columns');

      m.open('agent-c');
      expect(m.getState().active).toBe('single');
      expect(m.getState().single[0]).toBe('agent-c');
      // two-columns preserved
      expect(m.getState().twoColumns).toEqual(['agent-a', 'agent-b']);
    });

    it('overflows two-rows at capacity', () => {
      const m = manager();
      m.place('agent-a', 'two-rows', 0);
      m.place('agent-b', 'two-rows', 1);
      m.setLayout('two-rows');

      m.open('agent-c');
      expect(m.getState().active).toBe('single');
      expect(m.getState().single[0]).toBe('agent-c');
      // two-rows preserved
      expect(m.getState().twoRows).toEqual(['agent-a', 'agent-b']);
    });

    it('does not overflow when active is single', () => {
      const m = manager();
      m.open('agent-a');
      expect(m.getState().active).toBe('single');

      m.open('agent-b');
      // single never triggers overflow — just replaces single[0]
      expect(m.getState().active).toBe('single');
      expect(m.getState().single[0]).toBe('agent-b');
    });

    it('cancels zoom on overflow', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.place('agent-c', 'four', 2);
      m.place('agent-d', 'four', 3);
      m.setLayout('four');
      m.zoom('agent-a');
      expect(m.getZoomed()).toBe('agent-a');

      m.open('agent-e');
      expect(m.getZoomed()).toBeNull();
    });

    it('select never overflows even at capacity (#1701)', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.place('agent-c', 'four', 2);
      m.place('agent-d', 'four', 3);
      m.setLayout('four');

      // select (navigate to existing agent) must NOT overflow
      m.select('agent-a');
      expect(m.getState().active).toBe('four');
      expect(m.getState().single[0]).toBe('agent-a');
    });
  });

  describe('setLayout', () => {
    it('changes active preset without modifying assignments', () => {
      const m = manager();
      m.open('agent-a');
      m.place('agent-b', 'four', 0);
      m.place('agent-c', 'four', 1);

      m.setLayout('four');
      expect(m.getState().active).toBe('four');
      // Single slot preserved
      expect(m.getState().single).toEqual(['agent-a']);
      // Four slots preserved
      expect(m.getState().four[0]).toBe('agent-b');
      expect(m.getState().four[1]).toBe('agent-c');
    });

    it('is a no-op if already on the requested preset', () => {
      const m = manager();
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(listener);
      listener.mockClear();

      m.setLayout('single'); // Already active
      expect(listener).not.toHaveBeenCalled();
    });

    it('open overflow switches to single; setLayout still works afterward', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.place('agent-c', 'four', 2);
      m.place('agent-d', 'four', 3);
      m.setLayout('four');

      m.open('agent-e');
      // overflow → single (four is at capacity)
      expect(m.getState().active).toBe('single');
      expect(m.getState().single[0]).toBe('agent-e');

      // And switching back to four restores its assignments
      m.setLayout('four');
      expect(m.getState().active).toBe('four');
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', 'agent-d']);
    });

    it('cancels zoom when switching layouts', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.setLayout('four');
      m.zoom('agent-a');
      expect(m.getZoomed()).toBe('agent-a');

      m.setLayout('two-columns');
      expect(m.getZoomed()).toBeNull();
    });
  });

  describe('place', () => {
    it('places a session into a specific slot', () => {
      const m = manager();
      m.place('agent-a', 'four', 2);
      expect(m.getState().four).toEqual([null, null, 'agent-a', null]);
    });

    it('swaps when session already occupies another slot in the same preset', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 2);
      expect(m.getState().four).toEqual(['agent-a', null, 'agent-b', null]);

      // Move A to slot 2 — swap with B
      m.place('agent-a', 'four', 2);
      expect(m.getState().four).toEqual(['agent-b', null, 'agent-a', null]);
    });

    it('same-slot placement is a no-op', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(listener);
      listener.mockClear();

      m.place('agent-a', 'four', 0);
      expect(listener).not.toHaveBeenCalled();
    });

    it('only modifies the target preset', () => {
      const m = manager();
      m.open('agent-a');
      m.place('agent-b', 'two-columns', 0);
      m.place('agent-c', 'two-rows', 1);

      m.place('agent-x', 'four', 3);

      expect(m.getState().single).toEqual(['agent-a']);
      expect(m.getState().twoColumns).toEqual(['agent-b', null]);
      expect(m.getState().twoRows).toEqual([null, 'agent-c']);
      expect(m.getState().four).toEqual([null, null, null, 'agent-x']);
    });

    it('replaces a null slot without swap', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      // Place a new session into an empty slot
      m.place('agent-c', 'four', 2);
      expect(m.getState().four).toEqual(['agent-a', 'agent-b', 'agent-c', null]);
    });

    it('replaces an occupied slot', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 0);
      // agent-b replaces agent-a (agent-b was not in the preset before, no swap)
      expect(m.getState().four).toEqual(['agent-b', null, null, null]);
    });

    it('validates slot index bounds', () => {
      const m = manager();
      expect(() => m.place('agent-a', 'single', 1)).toThrow(RangeError);
      expect(() => m.place('agent-a', 'two-columns', 2)).toThrow(RangeError);
      expect(() => m.place('agent-a', 'four', 4)).toThrow(RangeError);
      expect(() => m.place('agent-a', 'four', -1)).toThrow(RangeError);
    });

    it('allows placement into single preset', () => {
      const m = manager();
      m.place('agent-a', 'single', 0);
      expect(m.getState().single).toEqual(['agent-a']);
    });

    it('swaps within two-column preset', () => {
      const m = manager();
      m.place('agent-a', 'two-columns', 0);
      m.place('agent-b', 'two-columns', 1);
      m.place('agent-a', 'two-columns', 1);
      expect(m.getState().twoColumns).toEqual(['agent-b', 'agent-a']);
    });

    it('does not change active layout', () => {
      const m = manager();
      expect(m.getState().active).toBe('single');
      m.place('agent-a', 'four', 0);
      expect(m.getState().active).toBe('single');
    });
  });

  describe('close', () => {
    it('clears all references across all presets', () => {
      const m = manager();
      m.open('agent-x');
      m.place('agent-x', 'two-columns', 1);
      m.place('agent-x', 'four', 3);

      m.close('agent-x');

      expect(m.getState().single[0]).toBeNull();
      expect(m.getState().twoColumns[1]).toBeNull();
      expect(m.getState().four[3]).toBeNull();
    });

    it('does not substitute another session', () => {
      const m = manager();
      m.open('agent-a');
      m.place('agent-b', 'four', 0);
      m.place('agent-a', 'four', 1);

      m.close('agent-a');

      expect(m.getState().single[0]).toBeNull();
      expect(m.getState().four[0]).toBe('agent-b');
      expect(m.getState().four[1]).toBeNull();
    });

    it('is a no-op for an unknown session key', () => {
      const m = manager();
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(listener);
      listener.mockClear();

      m.close('nonexistent');
      expect(listener).not.toHaveBeenCalled();
    });

    it('does not change the active layout', () => {
      const m = manager();
      m.open('agent-a');
      m.setLayout('four');

      m.close('agent-a');
      expect(m.getState().active).toBe('four');
    });

    it('clears zoom when the zoomed session is closed', () => {
      const m = manager();
      m.open('agent-a');
      m.zoom('agent-a');
      expect(m.getZoomed()).toBe('agent-a');

      m.close('agent-a');
      expect(m.getZoomed()).toBeNull();
    });

    it('preserves zoom when a different session is closed', () => {
      const m = manager();
      m.open('agent-a');
      m.place('agent-b', 'four', 0);
      m.zoom('agent-a');

      m.close('agent-b');
      expect(m.getZoomed()).toBe('agent-a');
    });
  });

  describe('cross-preset references', () => {
    it('same session can exist in single and four simultaneously', () => {
      const m = manager();
      m.open('agent-x');
      m.place('agent-x', 'four', 1);

      expect(m.getState().single[0]).toBe('agent-x');
      expect(m.getState().four[1]).toBe('agent-x');
    });

    it('same session can exist in all presets simultaneously', () => {
      const m = manager();
      m.open('agent-x');
      m.place('agent-x', 'two-columns', 0);
      m.place('agent-x', 'two-rows', 1);
      m.place('agent-x', 'four', 2);

      expect(m.getState().single[0]).toBe('agent-x');
      expect(m.getState().twoColumns[0]).toBe('agent-x');
      expect(m.getState().twoRows[1]).toBe('agent-x');
      expect(m.getState().four[2]).toBe('agent-x');
    });
  });

  describe('zoom / unzoom', () => {
    it('zoom does not overwrite any preset assignments', () => {
      const m = manager();
      m.open('agent-a');
      m.place('agent-b', 'four', 0);
      m.place('agent-c', 'four', 1);
      m.place('agent-d', 'four', 2);
      m.place('agent-e', 'four', 3);

      const beforeZoom = m.getState();
      m.zoom('agent-b');
      const afterZoom = m.getState();

      // State object is the same — zoom is separate
      expect(afterZoom.single).toEqual(beforeZoom.single);
      expect(afterZoom.twoColumns).toEqual(beforeZoom.twoColumns);
      expect(afterZoom.twoRows).toEqual(beforeZoom.twoRows);
      expect(afterZoom.four).toEqual(beforeZoom.four);
      expect(afterZoom.active).toBe(beforeZoom.active);
    });

    it('unzoom restores all preset assignments unchanged', () => {
      const m = manager();
      m.open('agent-a');
      m.place('agent-b', 'four', 0);
      m.place('agent-c', 'four', 1);

      const beforeZoom = m.getState();
      m.zoom('agent-b');
      m.unzoom();
      const afterUnzoom = m.getState();

      expect(afterUnzoom).toEqual(beforeZoom);
    });

    it('getVisibleSlots returns only the zoomed session', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.setLayout('four');

      m.zoom('agent-a');
      expect(m.getVisibleSlots()).toEqual(['agent-a']);
    });

    it('getVisibleSlots returns active preset slots when not zoomed', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.setLayout('four');

      expect(m.getVisibleSlots()).toEqual(['agent-a', 'agent-b', null, null]);
    });

    it('zoom is idempotent for same session', () => {
      const m = manager();
      m.open('agent-a');
      m.zoom('agent-a');
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(listener);
      listener.mockClear();

      m.zoom('agent-a');
      expect(listener).not.toHaveBeenCalled();
    });

    it('unzoom is idempotent when not zoomed', () => {
      const m = manager();
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(listener);
      listener.mockClear();

      m.unzoom();
      expect(listener).not.toHaveBeenCalled();
    });

    it('zoom can switch to a different session', () => {
      const m = manager();
      m.open('agent-a');
      m.zoom('agent-a');
      m.zoom('agent-b');
      expect(m.getZoomed()).toBe('agent-b');
      expect(m.getVisibleSlots()).toEqual(['agent-b']);
    });
  });

  describe('getVisibleSlots', () => {
    it('returns single slot for single layout', () => {
      const m = manager();
      m.open('agent-a');
      expect(m.getVisibleSlots()).toEqual(['agent-a']);
    });

    it('returns all nulls initially', () => {
      const m = manager();
      expect(m.getVisibleSlots()).toEqual([null]);
    });

    it('returns two-columns slots', () => {
      const m = manager();
      m.place('agent-a', 'two-columns', 0);
      m.place('agent-b', 'two-columns', 1);
      m.setLayout('two-columns');
      expect(m.getVisibleSlots()).toEqual(['agent-a', 'agent-b']);
    });

    it('returns two-rows slots', () => {
      const m = manager();
      m.place('agent-a', 'two-rows', 0);
      m.place('agent-b', 'two-rows', 1);
      m.setLayout('two-rows');
      expect(m.getVisibleSlots()).toEqual(['agent-a', 'agent-b']);
    });
  });

  describe('subscription correctness', () => {
    it('listener is called immediately with current state on subscribe', () => {
      const m = manager();
      m.open('agent-a');
      const listener = vi.fn<(state: TerminalLayoutState) => void>();

      m.subscribe(listener);
      expect(listener).toHaveBeenCalledTimes(1);
      expect(listener).toHaveBeenCalledWith(m.getState());
    });

    it('listener is called on each mutation', () => {
      const m = manager();
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(listener);
      listener.mockClear();

      m.open('agent-a');
      expect(listener).toHaveBeenCalledTimes(1);

      m.place('agent-b', 'four', 0);
      expect(listener).toHaveBeenCalledTimes(2);

      m.setLayout('four');
      expect(listener).toHaveBeenCalledTimes(3);

      m.close('agent-a');
      expect(listener).toHaveBeenCalledTimes(4);
    });

    it('no-op operations do NOT trigger notification', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(listener);
      listener.mockClear();

      // Same-slot place is a no-op
      m.place('agent-a', 'four', 0);
      expect(listener).not.toHaveBeenCalled();

      // setLayout to current layout is a no-op
      m.setLayout('single');
      expect(listener).not.toHaveBeenCalled();

      // Close of nonexistent key is a no-op
      m.close('nonexistent');
      expect(listener).not.toHaveBeenCalled();

      // Unzoom when not zoomed is a no-op
      m.unzoom();
      expect(listener).not.toHaveBeenCalled();

      // Zoom same session twice is a no-op
      m.zoom('agent-a');
      listener.mockClear();
      m.zoom('agent-a');
      expect(listener).not.toHaveBeenCalled();
    });

    it('unsubscribe prevents further notifications', () => {
      const m = manager();
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      const unsub = m.subscribe(listener);
      listener.mockClear();

      unsub();
      m.open('agent-a');
      expect(listener).not.toHaveBeenCalled();
    });

    it('multiple listeners receive updates independently', () => {
      const m = manager();
      const l1 = vi.fn<(state: TerminalLayoutState) => void>();
      const l2 = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(l1);
      m.subscribe(l2);
      l1.mockClear();
      l2.mockClear();

      m.open('agent-a');
      expect(l1).toHaveBeenCalledTimes(1);
      expect(l2).toHaveBeenCalledTimes(1);
    });

    it('zoom and unzoom notify listeners', () => {
      const m = manager();
      m.open('agent-a');
      const listener = vi.fn<(state: TerminalLayoutState) => void>();
      m.subscribe(listener);
      listener.mockClear();

      m.zoom('agent-a');
      expect(listener).toHaveBeenCalledTimes(1);

      m.unzoom();
      expect(listener).toHaveBeenCalledTimes(2);
    });
  });

  describe('no-duplicate invariant within a preset', () => {
    it('prevents duplicate by swapping', () => {
      const m = manager();
      m.place('agent-a', 'four', 0);
      m.place('agent-b', 'four', 1);
      m.place('agent-c', 'four', 2);
      m.place('agent-d', 'four', 3);

      // Move agent-a to slot 2, swapping with agent-c
      m.place('agent-a', 'four', 2);

      const four = m.getState().four;
      expect(four[0]).toBe('agent-c'); // swapped from slot 2
      expect(four[2]).toBe('agent-a'); // moved to slot 2
      expect(four[1]).toBe('agent-b'); // unchanged
      expect(four[3]).toBe('agent-d'); // unchanged

      // No duplicates
      const uniqueNonNull = four.filter((s) => s !== null);
      expect(new Set(uniqueNonNull).size).toBe(uniqueNonNull.length);
    });
  });

  describe('edge cases', () => {
    it('place into single preset slot 0', () => {
      const m = manager();
      m.place('agent-a', 'single', 0);
      expect(m.getState().single[0]).toBe('agent-a');
    });

    it('open after place into single swaps via open semantics', () => {
      const m = manager();
      m.place('agent-a', 'single', 0);
      m.open('agent-b');
      // open replaces single[0], not a swap
      expect(m.getState().single[0]).toBe('agent-b');
    });

    it('close while zoomed clears zoom and slot references', () => {
      const m = manager();
      m.open('agent-a');
      m.place('agent-a', 'four', 0);
      m.zoom('agent-a');

      m.close('agent-a');
      expect(m.getZoomed()).toBeNull();
      expect(m.getState().single[0]).toBeNull();
      expect(m.getState().four[0]).toBeNull();
    });

    it('getVisibleSlots returns correct preset after layout switch', () => {
      const m = manager();
      m.open('agent-a');
      m.place('agent-b', 'two-columns', 0);
      m.place('agent-c', 'two-columns', 1);

      expect(m.getVisibleSlots()).toEqual(['agent-a']); // single active

      m.setLayout('two-columns');
      expect(m.getVisibleSlots()).toEqual(['agent-b', 'agent-c']);

      m.setLayout('single');
      expect(m.getVisibleSlots()).toEqual(['agent-a']);
    });

    it('multiple closes on the same key are safe', () => {
      const m = manager();
      m.open('agent-a');
      m.close('agent-a');
      // Second close should be a no-op
      m.close('agent-a');
      expect(m.getState().single[0]).toBeNull();
    });
  });
});
