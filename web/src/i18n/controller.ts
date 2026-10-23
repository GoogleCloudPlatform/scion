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
import { LOCALE_CHANGED_EVENT } from './events.js';

/** Re-render translated components; clean up when a route unmounts them. */
export class LocaleController implements ReactiveController {
  constructor(
    private readonly host: ReactiveControllerHost,
    private readonly changed?: () => void
  ) {
    host.addController(this);
  }

  private readonly update = (): void => {
    this.changed?.();
    this.host.requestUpdate();
  };

  hostConnected(): void {
    window.addEventListener(LOCALE_CHANGED_EVENT, this.update);
  }

  hostDisconnected(): void {
    window.removeEventListener(LOCALE_CHANGED_EVENT, this.update);
  }
}
