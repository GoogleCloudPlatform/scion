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

import type { ReactiveController, ReactiveControllerHost } from 'lit';

/** How long a finger must stay down before a press counts as a long-press. */
export const LONG_PRESS_MS = 450;

/** How far the finger may drift, in CSS px, before the press is abandoned. */
export const LONG_PRESS_SLOP_PX = 10;

/**
 * How long after the finger lifts a click is still treated as the tail of a
 * long-press and swallowed. Browsers dispatch the click right after
 * `pointerup`, so this only needs to cover event-loop latency.
 */
const CLICK_SWALLOW_GRACE_MS = 400;

/** Where the press happened, in viewport coordinates. */
export interface LongPressPoint {
  x: number;
  y: number;
}

/**
 * Touch long-press detection for rows that open a menu.
 *
 * iOS never fires `contextmenu` for a long-press, so a menu bound only to
 * `contextmenu` is unreachable there. This fills the gap:
 *
 * - Only `pointerType === 'touch'` presses are tracked. Mouse and pen keep
 *   using right-click.
 * - It fires after `LONG_PRESS_MS` unless the finger moves more than
 *   `LONG_PRESS_SLOP_PX`, lifts, the browser cancels the pointer, or a
 *   scroll container under the finger scrolls. Any of those means the user
 *   was scrolling or swiping, not pressing.
 * - Once it fires, the click that follows the finger lifting is swallowed,
 *   so the row's own tap action (opening a thread, say) does not run too.
 * - Android does fire `contextmenu` on a long-press. Hosts route their
 *   `contextmenu` handler through `contextMenu()`, and whichever of the two
 *   fires first opens the menu; the other is suppressed for that gesture.
 *
 * Usage, from a Lit template:
 *
 *   @pointerdown=${(e: PointerEvent) => this.longPress.pointerDown(e, (p) => this.openMenu(p))}
 *   @contextmenu=${(e: MouseEvent) => { if (this.longPress.contextMenu(e)) return; ... }}
 */
export class LongPressController implements ReactiveController {
  private timer: ReturnType<typeof setTimeout> | null = null;
  private pointerId: number | null = null;
  private start: LongPressPoint = { x: 0, y: 0 };
  /** The current gesture already opened a menu, by timer or by contextmenu. */
  private fired = false;
  private swallowClick = false;
  private swallowTimer: ReturnType<typeof setTimeout> | null = null;
  private scrollers: EventTarget[] = [];

  constructor(host?: ReactiveControllerHost) {
    host?.addController(this);
  }

  hostConnected(): void {}

  hostDisconnected(): void {
    this.dispose();
  }

  /**
   * Begin tracking a press. `onFire` runs once, if the press survives
   * `LONG_PRESS_MS`, with the point where the finger went down.
   */
  pointerDown(e: PointerEvent, onFire: (point: LongPressPoint) => void): void {
    // A new press always ends any previous gesture's click swallowing: the
    // next click belongs to this press, not the last one.
    this.disarmClickSwallow();
    this.cancel();
    if (e.pointerType !== 'touch' || !e.isPrimary) return;

    this.pointerId = e.pointerId;
    this.start = { x: e.clientX, y: e.clientY };
    window.addEventListener('pointermove', this.handleMove, true);
    window.addEventListener('pointerup', this.handleEnd, true);
    window.addEventListener('pointercancel', this.handleEnd, true);
    this.watchScrollers(e);
    this.timer = setTimeout(() => {
      this.timer = null;
      this.fired = true;
      this.armClickSwallow();
      onFire({ ...this.start });
    }, LONG_PRESS_MS);
  }

  /**
   * Route a `contextmenu` event through the controller. Returns `true` when
   * the event belongs to a gesture whose long-press already opened the
   * menu: the default is prevented and the caller must do nothing else.
   * Otherwise returns `false` and the caller opens its menu as usual; a
   * pending long-press for the same gesture is cancelled so it cannot open
   * a second one.
   */
  contextMenu(e: MouseEvent): boolean {
    if (this.fired) {
      e.preventDefault();
      e.stopPropagation();
      return true;
    }
    if (this.pointerId !== null) {
      // The browser's own long-press beat the timer (Android). Treat it as
      // this gesture's long-press: no second menu, and no click afterwards.
      this.clearTimer();
      this.fired = true;
      this.armClickSwallow();
    }
    return false;
  }

  /**
   * A touch press is being tracked and has not fired yet. A `contextmenu`
   * arriving now is the browser's own long-press for that touch, which a
   * host may want to present differently from a mouse right-click.
   */
  get pressing(): boolean {
    return this.pointerId !== null && !this.fired;
  }

  /** Abandon the current press without firing. */
  cancel(): void {
    this.clearTimer();
    this.stopTracking();
  }

  /** Remove every listener and timer. */
  dispose(): void {
    this.cancel();
    this.disarmClickSwallow();
  }

  private clearTimer(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  private stopTracking(): void {
    this.pointerId = null;
    window.removeEventListener('pointermove', this.handleMove, true);
    window.removeEventListener('pointerup', this.handleEnd, true);
    window.removeEventListener('pointercancel', this.handleEnd, true);
    for (const el of this.scrollers) {
      el.removeEventListener('scroll', this.handleScroll, true);
    }
    this.scrollers = [];
  }

  /**
   * Listen for scrolls on every scroll container the press started inside.
   * `scroll` does not bubble and is not composed, so it never leaves the
   * shadow root of the element that scrolled. A capture listener on window
   * would miss the rail's, members' and thread's own scrollers, which all
   * live inside shadow roots.
   */
  private watchScrollers(e: PointerEvent): void {
    for (const node of e.composedPath()) {
      if (!(node instanceof Element)) continue;
      const overflowY = getComputedStyle(node).overflowY;
      if (overflowY === 'auto' || overflowY === 'scroll') {
        node.addEventListener('scroll', this.handleScroll, true);
        this.scrollers.push(node);
      }
    }
  }

  private armClickSwallow(): void {
    this.swallowClick = true;
    window.addEventListener('click', this.handleClick, true);
    // Any new press, on this row or anywhere else (a sheet item, say),
    // starts a gesture whose click must go through.
    window.addEventListener('pointerdown', this.handleNextPointerDown, true);
  }

  private disarmClickSwallow(): void {
    if (this.swallowTimer !== null) {
      clearTimeout(this.swallowTimer);
      this.swallowTimer = null;
    }
    // The gesture is over, so a later contextmenu (a right-click, say) is a
    // fresh request rather than this gesture's duplicate.
    this.fired = false;
    if (!this.swallowClick) return;
    this.swallowClick = false;
    window.removeEventListener('click', this.handleClick, true);
    window.removeEventListener('pointerdown', this.handleNextPointerDown, true);
  }

  private readonly handleMove = (e: PointerEvent): void => {
    // After firing, movement no longer matters; keep tracking only to see
    // the finger lift.
    if (e.pointerId !== this.pointerId || this.fired) return;
    const dx = e.clientX - this.start.x;
    const dy = e.clientY - this.start.y;
    if (Math.hypot(dx, dy) > LONG_PRESS_SLOP_PX) this.cancel();
  };

  private readonly handleEnd = (e: PointerEvent): void => {
    if (e.pointerId !== this.pointerId) return;
    this.cancel();
    if (this.swallowClick) {
      // The click for this lift is dispatched right after pointerup. If it
      // never comes (the browser suppressed it), stop swallowing shortly
      // after so an unrelated later click is not eaten.
      this.swallowTimer = setTimeout(() => this.disarmClickSwallow(), CLICK_SWALLOW_GRACE_MS);
    }
  };

  private readonly handleScroll = (): void => {
    if (!this.fired) this.cancel();
  };

  private readonly handleNextPointerDown = (e: PointerEvent): void => {
    if (e.pointerId === this.pointerId) return;
    this.disarmClickSwallow();
  };

  private readonly handleClick = (e: MouseEvent): void => {
    e.preventDefault();
    e.stopPropagation();
    this.disarmClickSwallow();
  };
}
