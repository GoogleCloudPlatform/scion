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
 * The signed-in user's choices-card preference
 * (`preferences.preselectRecommendedChoices`, ptone/scion#7842).
 *
 * Off by default: a multi-decision card only outlines recommended choices
 * and nothing is selected until the user clicks. On: each recommended
 * choice starts selected. The app shell sets it from `/auth/me`; the
 * profile settings page sets it after its PATCH. It has no dependencies so
 * the app shell can import it without the choices code.
 */

import type { ReactiveController, ReactiveControllerHost } from 'lit';

let preselect = false;

/** Dispatched on `window` when the preference changes. */
export const CHOICE_PREFERENCES_CHANGED_EVENT = 'scion-choice-preferences-changed';

/** Whether multi-decision cards start with recommended choices selected. */
export function preselectRecommendedChoices(): boolean {
  return preselect;
}

/** Sets the preference; absent means off. */
export function setPreselectRecommendedChoices(value: boolean | null | undefined): void {
  const next = value === true;
  if (next === preselect) return;
  preselect = next;
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new Event(CHOICE_PREFERENCES_CHANGED_EVENT));
  }
}

/** Re-renders its host when the preference changes. */
export class ChoicePreferencesController implements ReactiveController {
  private readonly host: ReactiveControllerHost;

  constructor(host: ReactiveControllerHost) {
    this.host = host;
    host.addController(this);
  }

  private readonly handleChange = (): void => {
    this.host.requestUpdate();
  };

  hostConnected(): void {
    window.addEventListener(CHOICE_PREFERENCES_CHANGED_EVENT, this.handleChange);
  }

  hostDisconnected(): void {
    window.removeEventListener(CHOICE_PREFERENCES_CHANGED_EVENT, this.handleChange);
  }
}
