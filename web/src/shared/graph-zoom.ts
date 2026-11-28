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
 * Pure wheel/pinch zoom math and per-frame coalescing helpers shared by all
 * agent graph views (via `<scion-agent-tree-view>`).
 */

/** Standard DOM WheelEvent.deltaMode constants (usable without DOM globals). */
export const DELTA_MODE_PIXEL = 0;
export const DELTA_MODE_LINE = 1;
export const DELTA_MODE_PAGE = 2;

/**
 * Pixel conversion factors for non-pixel wheel delta modes.
 * - Line mode (Firefox mouse wheel typically sends ±3 lines per notch):
 *   32 px/line maps a 3-line notch to 96 px, matching Chrome's ~100 px notch.
 * - Page mode: 400 px/page (further bounded by per-event clamping).
 */
export const LINE_DELTA_PX = 32;
export const PAGE_DELTA_PX = 400;

/**
 * Exponential zoom rate `k` (per normalized pixel) for standard wheel/scroll
 * events. A 100 px mouse-wheel notch yields `exp(0.12) ≈ 1.127`, preserving
 * the classic mouse-wheel feel (~1.12x per notch), while a 4 px trackpad
 * two-finger scroll event yields `exp(0.0048) ≈ 1.005` instead of jumping.
 */
export const WHEEL_ZOOM_SENSITIVITY = 0.0012;

/**
 * Gentler exponential zoom rate `k` for trackpad pinch gestures, which
 * browsers synthesize as `wheel` events with `ctrlKey: true`.
 */
export const PINCH_ZOOM_SENSITIVITY = 0.0006;

/**
 * Divisor applied to the zoom rate when the low-gear modifier (`Alt` / Option)
 * is held, giving fine control on both trackpads and mouse wheels.
 */
export const LOW_GEAR_DIVISOR = 4;

/**
 * Maximum magnitude of `ln(factor)` for a single wheel event (`exp(±0.25)` ≈
 * `0.779`..`1.284`), preventing hardware spikes or `DOM_DELTA_PAGE` events from
 * jumping the viewport wildly while leaving normal mouse-wheel notches unclipped.
 */
export const MAX_EVENT_LOG_STEP = 0.25;

/**
 * Maximum magnitude of `ln(factor)` accumulated within a single animation
 * frame (`exp(±0.5)` ≈ `0.607`..`1.649`), bounding burst flings when coalescing.
 */
export const MAX_FRAME_LOG_STEP = 0.5;

export interface WheelZoomEvent {
  deltaY: number;
  deltaMode?: number;
  ctrlKey?: boolean;
  altKey?: boolean;
}

export interface CoalescedWheelEvent extends WheelZoomEvent {
  clientX: number;
  clientY: number;
}

export interface WheelZoomOptions {
  wheelSensitivity?: number;
  pinchSensitivity?: number;
  lowGearDivisor?: number;
  maxEventLogStep?: number;
  maxFrameLogStep?: number;
}

export interface PendingWheelZoom {
  readonly logDelta: number;
  readonly clientX: number;
  readonly clientY: number;
}

export interface FlushedWheelZoom {
  readonly factor: number;
  readonly clientX: number;
  readonly clientY: number;
}

function clampSymmetric(value: number, maxAbs: number): number {
  if (value > maxAbs) return maxAbs;
  if (value < -maxAbs) return -maxAbs;
  return value;
}

/**
 * Converts a wheel event's `deltaY` into pixel units according to `deltaMode`.
 * Non-finite or zero deltas normalize to `0`.
 */
export function normalizeWheelDeltaY(deltaY: number, deltaMode: number = DELTA_MODE_PIXEL): number {
  if (!Number.isFinite(deltaY) || deltaY === 0) return 0;
  switch (deltaMode) {
    case DELTA_MODE_LINE:
      return deltaY * LINE_DELTA_PX;
    case DELTA_MODE_PAGE:
      return deltaY * PAGE_DELTA_PX;
    case DELTA_MODE_PIXEL:
    default:
      return deltaY;
  }
}

/**
 * Computes the clamped log-scale zoom delta `ln(factor)` for a single wheel
 * event. Negative `deltaY` (wheel up / pinch spread) produces a positive log
 * delta (zoom in); positive `deltaY` produces a negative log delta (zoom out).
 */
export function wheelZoomLogDelta(event: WheelZoomEvent, options?: WheelZoomOptions): number {
  const normalized = normalizeWheelDeltaY(event.deltaY, event.deltaMode);
  if (normalized === 0) return 0;

  const baseSensitivity = event.ctrlKey
    ? (options?.pinchSensitivity ?? PINCH_ZOOM_SENSITIVITY)
    : (options?.wheelSensitivity ?? WHEEL_ZOOM_SENSITIVITY);
  const divisor = event.altKey ? (options?.lowGearDivisor ?? LOW_GEAR_DIVISOR) : 1;
  const maxStep = options?.maxEventLogStep ?? MAX_EVENT_LOG_STEP;

  return clampSymmetric((-normalized * baseSensitivity) / divisor, maxStep);
}

/**
 * Computes the multiplicative zoom factor (`exp(logDelta)`) for a single wheel
 * event. Returns `1` for zero or non-finite deltas.
 */
export function wheelZoomFactor(event: WheelZoomEvent, options?: WheelZoomOptions): number {
  const logDelta = wheelZoomLogDelta(event, options);
  return logDelta === 0 ? 1 : Math.exp(logDelta);
}

/**
 * Coalesces a wheel event into the pending per-frame zoom accumulator.
 * Zero-delta events leave `pending` unchanged (preserving its reference so
 * callers can skip scheduling an animation frame).
 */
export function accumulateWheelZoom(
  pending: PendingWheelZoom | null,
  event: CoalescedWheelEvent,
  options?: WheelZoomOptions
): PendingWheelZoom | null {
  const step = wheelZoomLogDelta(event, options);
  if (step === 0) return pending;

  const maxFrame = options?.maxFrameLogStep ?? MAX_FRAME_LOG_STEP;
  const prevLog = pending?.logDelta ?? 0;
  return {
    logDelta: clampSymmetric(prevLog + step, maxFrame),
    clientX: event.clientX,
    clientY: event.clientY,
  };
}

/**
 * Converts a pending per-frame zoom accumulator into a multiplicative factor
 * and cursor anchor, or `null` if there is no net zoom to apply.
 */
export function flushWheelZoom(pending: PendingWheelZoom | null): FlushedWheelZoom | null {
  if (!pending || pending.logDelta === 0) return null;
  return {
    factor: Math.exp(pending.logDelta),
    clientX: pending.clientX,
    clientY: pending.clientY,
  };
}
