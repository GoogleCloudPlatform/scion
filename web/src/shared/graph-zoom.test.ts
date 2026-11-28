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

import { describe, it, expect } from 'vitest';
import {
  DELTA_MODE_PIXEL,
  DELTA_MODE_LINE,
  DELTA_MODE_PAGE,
  LINE_DELTA_PX,
  PAGE_DELTA_PX,
  WHEEL_ZOOM_SENSITIVITY,
  PINCH_ZOOM_SENSITIVITY,
  LOW_GEAR_DIVISOR,
  MAX_EVENT_LOG_STEP,
  MAX_FRAME_LOG_STEP,
  normalizeWheelDeltaY,
  wheelZoomLogDelta,
  wheelZoomFactor,
  accumulateWheelZoom,
  flushWheelZoom,
  type PendingWheelZoom,
} from './graph-zoom.js';

describe('normalizeWheelDeltaY', () => {
  it('passes through pixel deltas unchanged (default and explicit DOM_DELTA_PIXEL)', () => {
    expect(normalizeWheelDeltaY(-100)).toBe(-100);
    expect(normalizeWheelDeltaY(4.5, DELTA_MODE_PIXEL)).toBe(4.5);
  });

  it('converts line deltas using LINE_DELTA_PX', () => {
    expect(normalizeWheelDeltaY(-3, DELTA_MODE_LINE)).toBe(-3 * LINE_DELTA_PX);
    expect(normalizeWheelDeltaY(1, DELTA_MODE_LINE)).toBe(LINE_DELTA_PX);
  });

  it('converts page deltas using PAGE_DELTA_PX', () => {
    expect(normalizeWheelDeltaY(1, DELTA_MODE_PAGE)).toBe(PAGE_DELTA_PX);
    expect(normalizeWheelDeltaY(-2, DELTA_MODE_PAGE)).toBe(-2 * PAGE_DELTA_PX);
  });

  it('returns 0 for zero, -0, NaN, and Infinity', () => {
    expect(Object.is(normalizeWheelDeltaY(0), 0)).toBe(true);
    expect(Object.is(normalizeWheelDeltaY(-0), 0)).toBe(true);
    expect(normalizeWheelDeltaY(NaN)).toBe(0);
    expect(normalizeWheelDeltaY(Infinity)).toBe(0);
    expect(normalizeWheelDeltaY(-Infinity, DELTA_MODE_LINE)).toBe(0);
  });
});

describe('wheelZoomLogDelta & wheelZoomFactor', () => {
  it('zooms in on negative deltaY and out on positive deltaY with exact inverse symmetry', () => {
    const zoomIn = wheelZoomFactor({ deltaY: -100, deltaMode: DELTA_MODE_PIXEL });
    const zoomOut = wheelZoomFactor({ deltaY: 100, deltaMode: DELTA_MODE_PIXEL });
    expect(zoomIn).toBeCloseTo(Math.exp(100 * WHEEL_ZOOM_SENSITIVITY), 10);
    expect(zoomIn).toBeGreaterThan(1.1);
    expect(zoomIn).toBeLessThan(1.15);
    expect(zoomOut).toBeCloseTo(1 / zoomIn, 10);
    expect(zoomIn * zoomOut).toBeCloseTo(1, 10);
  });

  it('scales proportionally for small fractional trackpad deltas instead of jumping', () => {
    const small = wheelZoomFactor({ deltaY: -2.5, deltaMode: DELTA_MODE_PIXEL });
    const tiny = wheelZoomFactor({ deltaY: -0.5, deltaMode: DELTA_MODE_PIXEL });
    expect(small).toBeCloseTo(Math.exp(2.5 * WHEEL_ZOOM_SENSITIVITY), 10);
    expect(tiny).toBeCloseTo(Math.exp(0.5 * WHEEL_ZOOM_SENSITIVITY), 10);
    // Small trackpad events produce sub-1% changes, far gentler than a 12% fixed step.
    expect(small).toBeLessThan(1.01);
    expect(tiny).toBeGreaterThan(1);
    expect(tiny).toBeLessThan(small);
  });

  it('normalizes Firefox line-mode mouse-wheel notches to match Chrome pixel notches', () => {
    const firefoxNotch = wheelZoomFactor({ deltaY: -3, deltaMode: DELTA_MODE_LINE });
    const chromeNotch = wheelZoomFactor({ deltaY: -100, deltaMode: DELTA_MODE_PIXEL });
    expect(firefoxNotch).toBeCloseTo(Math.exp(3 * LINE_DELTA_PX * WHEEL_ZOOM_SENSITIVITY), 10);
    // Within 1% of a 100px Chrome notch.
    expect(Math.abs(firefoxNotch - chromeNotch)).toBeLessThan(0.01);
  });

  it('tracks Chromium ctrlKey pinch gestures (deltaY = -100*ln(scale)) across a 2x pinch sequence', () => {
    // Chromium emits touchpad pinch as ctrlKey wheel events with deltaY = -100 * ln(scale).
    // A physical 2x pinch sums to -100 * ln(2) ≈ -69.315 across ~20 small updates.
    const steps = 20;
    const perEventDelta = (-100 * Math.LN2) / steps;
    let scale = 1;
    for (let i = 0; i < steps; i++) {
      const pending = accumulateWheelZoom(null, {
        deltaY: perEventDelta,
        deltaMode: DELTA_MODE_PIXEL,
        ctrlKey: true,
        clientX: 200,
        clientY: 150,
      });
      scale *= flushWheelZoom(pending)!.factor;
    }
    expect(scale).toBeGreaterThan(1.4);
    expect(scale).toBeCloseTo(2, 6);

    // Single pinch event scales with PINCH_ZOOM_SENSITIVITY (distinct from wheel).
    const singleDelta = -5;
    expect(wheelZoomLogDelta({ deltaY: singleDelta, ctrlKey: true })).toBeCloseTo(
      -singleDelta * PINCH_ZOOM_SENSITIVITY,
      10
    );
    expect(wheelZoomLogDelta({ deltaY: singleDelta, ctrlKey: true })).toBeGreaterThan(
      wheelZoomLogDelta({ deltaY: singleDelta, ctrlKey: false })
    );

    // Ctrl + discrete 100px mouse-wheel click is still bounded by MAX_EVENT_LOG_STEP.
    expect(wheelZoomLogDelta({ deltaY: -100, ctrlKey: true })).toBe(MAX_EVENT_LOG_STEP);
  });

  it('divides zoom rate by LOW_GEAR_DIVISOR when altKey is held (for both wheel and pinch)', () => {
    const normalWheel = wheelZoomLogDelta({ deltaY: -100, altKey: false });
    const lowGearWheel = wheelZoomLogDelta({ deltaY: -100, altKey: true });
    expect(lowGearWheel).toBeCloseTo(normalWheel / LOW_GEAR_DIVISOR, 10);

    const normalPinch = wheelZoomLogDelta({ deltaY: -10, ctrlKey: true, altKey: false });
    const lowGearPinch = wheelZoomLogDelta({ deltaY: -10, ctrlKey: true, altKey: true });
    expect(lowGearPinch).toBeCloseTo(normalPinch / LOW_GEAR_DIVISOR, 10);

    // Full 2x pinch sequence with Alt held yields 4x finer log-scale (2^(1/4) ≈ 1.189x).
    const steps = 20;
    const perEventDelta = (-100 * Math.LN2) / steps;
    let lowGearScale = 1;
    for (let i = 0; i < steps; i++) {
      lowGearScale *= wheelZoomFactor({ deltaY: perEventDelta, ctrlKey: true, altKey: true });
    }
    expect(lowGearScale).toBeCloseTo(Math.pow(2, 1 / LOW_GEAR_DIVISOR), 6);
  });

  it('clamps extreme positive and negative deltas per event to MAX_EVENT_LOG_STEP', () => {
    const hugeIn = wheelZoomLogDelta({ deltaY: -5000, deltaMode: DELTA_MODE_PIXEL });
    const hugeOut = wheelZoomLogDelta({ deltaY: 5000, deltaMode: DELTA_MODE_PIXEL });
    expect(hugeIn).toBe(MAX_EVENT_LOG_STEP);
    expect(hugeOut).toBe(-MAX_EVENT_LOG_STEP);
    expect(wheelZoomFactor({ deltaY: -5000 })).toBeCloseTo(Math.exp(MAX_EVENT_LOG_STEP), 10);
    expect(wheelZoomFactor({ deltaY: 5000 })).toBeCloseTo(Math.exp(-MAX_EVENT_LOG_STEP), 10);

    // Page-mode event is also clamped.
    expect(wheelZoomLogDelta({ deltaY: -2, deltaMode: DELTA_MODE_PAGE })).toBe(MAX_EVENT_LOG_STEP);
  });

  it('returns factor 1 and logDelta 0 for zero or non-finite deltas', () => {
    expect(wheelZoomLogDelta({ deltaY: 0 })).toBe(0);
    expect(wheelZoomFactor({ deltaY: 0 })).toBe(1);
    expect(wheelZoomFactor({ deltaY: NaN })).toBe(1);
    expect(wheelZoomFactor({ deltaY: Infinity })).toBe(1);
  });
});

describe('per-frame coalescing (accumulateWheelZoom & flushWheelZoom)', () => {
  it('coalesces a burst of small trackpad events into one combined factor at the latest cursor', () => {
    let pending: PendingWheelZoom | null = null;
    const deltas = [-1.5, -2.25, -3.75, -2.5]; // total = -10 px
    deltas.forEach((deltaY, i) => {
      pending = accumulateWheelZoom(pending, {
        deltaY,
        deltaMode: DELTA_MODE_PIXEL,
        clientX: 100 + i * 5,
        clientY: 200 + i * 3,
      });
    });

    const flushed = flushWheelZoom(pending);
    expect(flushed).not.toBeNull();
    expect(flushed!.factor).toBeCloseTo(Math.exp(10 * WHEEL_ZOOM_SENSITIVITY), 10);
    expect(flushed!.clientX).toBe(115);
    expect(flushed!.clientY).toBe(209);
  });

  it('ignores zero-delta events without changing the pending reference', () => {
    expect(accumulateWheelZoom(null, { deltaY: 0, clientX: 50, clientY: 60 })).toBeNull();

    const first = accumulateWheelZoom(null, { deltaY: -10, clientX: 50, clientY: 60 });
    const afterZero = accumulateWheelZoom(first, { deltaY: 0, clientX: 99, clientY: 88 });
    expect(afterZero).toBe(first);
  });

  it('returns null from flushWheelZoom when pending is null or opposite deltas cancel out', () => {
    expect(flushWheelZoom(null)).toBeNull();

    let pending: PendingWheelZoom | null = null;
    pending = accumulateWheelZoom(pending, { deltaY: -15, clientX: 10, clientY: 20 });
    pending = accumulateWheelZoom(pending, { deltaY: 15, clientX: 10, clientY: 20 });
    expect(flushWheelZoom(pending)).toBeNull();
  });

  it('clamps accumulated frame total to MAX_FRAME_LOG_STEP on massive bursts', () => {
    let pending: PendingWheelZoom | null = null;
    for (let i = 0; i < 10; i++) {
      pending = accumulateWheelZoom(pending, { deltaY: -500, clientX: 100, clientY: 100 });
    }
    expect(pending!.logDelta).toBe(MAX_FRAME_LOG_STEP);
    expect(flushWheelZoom(pending)!.factor).toBeCloseTo(Math.exp(MAX_FRAME_LOG_STEP), 10);

    let outPending: PendingWheelZoom | null = null;
    for (let i = 0; i < 10; i++) {
      outPending = accumulateWheelZoom(outPending, { deltaY: 500, clientX: 100, clientY: 100 });
    }
    expect(outPending!.logDelta).toBe(-MAX_FRAME_LOG_STEP);
  });

  it('handles mixed trackpad pinch burst with low-gear modifier', () => {
    let pending: PendingWheelZoom | null = null;
    for (let i = 0; i < 5; i++) {
      pending = accumulateWheelZoom(pending, {
        deltaY: -4,
        ctrlKey: true,
        altKey: true,
        clientX: 300,
        clientY: 250,
      });
    }
    const flushed = flushWheelZoom(pending);
    expect(flushed).not.toBeNull();
    const expected = Math.exp((20 * PINCH_ZOOM_SENSITIVITY) / LOW_GEAR_DIVISOR);
    expect(flushed!.factor).toBeCloseTo(expected, 10);
  });
});
