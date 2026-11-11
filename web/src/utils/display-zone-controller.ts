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
 * Reactive controller that re-renders its host when the effective display
 * zone changes (tz-refactor task 11, review round 2 R2-1).
 *
 * `time.ts`'s `DISPLAY_TIMEZONE_CHANGED_EVENT` tells the app a zone change
 * happened, but dispatching it does nothing by itself: a mounted component
 * only reflects the new zone the next time it renders. A component that
 * renders any absolute or relative time and may already be on screen when
 * the preference loads or changes — this PR's native chat, and every P3
 * (tasks 19-21) surface after it — must force that render itself.
 *
 * Usage:
 * ```ts
 * class MyTimeDisplay extends LitElement {
 *   // Not `private`: this project's tsconfig enables `noUnusedLocals`,
 *   // which flags a `private` field that nothing ever reads — and nothing
 *   // needs to read this one; it exists for its constructor's side effect
 *   // (registering itself as a controller). A non-private field compiles
 *   // under the same config (review round 3, R3-2).
 *   readonly _zone = new DisplayZoneController(this);
 *   // ... render() calls formatInstant/zoneLabel/formatInstantWithZone as
 *   // usual; no other wiring needed — the controller calls requestUpdate()
 *   // for you.
 * }
 * ```
 */

import type { ReactiveController, ReactiveControllerHost } from 'lit';
import { DISPLAY_TIMEZONE_CHANGED_EVENT } from './time.js';

export class DisplayZoneController implements ReactiveController {
  private readonly host: ReactiveControllerHost;

  constructor(host: ReactiveControllerHost) {
    this.host = host;
    host.addController(this);
  }

  private readonly handleChange = (): void => {
    this.host.requestUpdate();
  };

  hostConnected(): void {
    window.addEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, this.handleChange);
  }

  hostDisconnected(): void {
    window.removeEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, this.handleChange);
  }
}
