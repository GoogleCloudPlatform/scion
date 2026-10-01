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

/**
 * Touch-primary input detection, shared by every component that needs to
 * know whether the device's *primary* pointer is a finger rather than a
 * mouse or trackpad — distinct from viewport width, which is a layout
 * concern each component handles with its own media queries.
 *
 * Both clauses matter:
 * - `(hover: none)` alone also matches headless desktop Chromium (no real
 *   pointing device attached at all reports no hover support), which would
 *   misclassify a desktop browser test run as touch.
 * - `(pointer: coarse)` alone would not exclude a touch laptop (trackpad:
 *   fine pointer, hover-capable) from ever being asked about.
 *
 * Combined, the two correctly separate:
 *   - a touch phone/tablet with no mouse: hover:none, pointer:coarse -> touch
 *   - an iPad with an external trackpad: hover:hover, pointer:fine -> desktop
 *   - a touch laptop (has a trackpad): hover:hover, pointer:fine -> desktop
 *   - headless desktop Chromium: hover:none, pointer:none -> desktop
 */
export const TOUCH_PRIMARY_QUERY = '(hover: none) and (pointer: coarse)';

/**
 * A `ReactiveController` that exposes whether the device's primary pointer
 * is touch, and keeps that value live across the host's connected lifetime.
 * Unlike a value captured once at module load or construction time, this
 * reacts to the query's own `change` event — docking a tablet, attaching a
 * trackpad, or toggling devtools touch emulation all take effect without a
 * reload, because the controller requests a host update whenever the
 * query's match state actually flips.
 */
export class TouchPrimaryController implements ReactiveController {
  private readonly host: ReactiveControllerHost;
  private mediaQueryList: MediaQueryList | null = null;
  private _isTouch = false;
  private readonly handleChange = (): void => {
    if (!this.mediaQueryList) return;
    const next = this.mediaQueryList.matches;
    if (next === this._isTouch) return;
    this._isTouch = next;
    this.host.requestUpdate();
  };

  constructor(host: ReactiveControllerHost) {
    this.host = host;
    host.addController(this);
  }

  /** Whether the device's primary pointer is currently touch. */
  get isTouch(): boolean {
    return this._isTouch;
  }

  hostConnected(): void {
    if (typeof window === 'undefined') return;
    this.mediaQueryList = window.matchMedia(TOUCH_PRIMARY_QUERY);
    this._isTouch = this.mediaQueryList.matches;
    this.mediaQueryList.addEventListener('change', this.handleChange);
    // A host reconnecting after the query flipped while it was detached
    // (no listener was live to call requestUpdate() for that flip) would
    // otherwise render stale until its next unrelated update — Lit does not
    // re-render on a bare reconnect unless something actually requests it.
    // Harmless on the very first connect too: it just folds into the
    // initial render already in progress.
    this.host.requestUpdate();
  }

  hostDisconnected(): void {
    this.mediaQueryList?.removeEventListener('change', this.handleChange);
    this.mediaQueryList = null;
  }
}
