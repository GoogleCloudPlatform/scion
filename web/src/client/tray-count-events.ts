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
 * Event contract between the inbox and notification trays and the header that
 * shows their counts as badges. Each tray owns its list; it dispatches this
 * event whenever the list changes (a fetch is applied, the list is cleared on
 * a user change or sign-out, or an item is acknowledged or marked read), so
 * the header never has to read the trays' internal state.
 */

/** Dispatched by a tray, bubbling and composed, whenever its list changes. */
export const TRAY_COUNT_EVENT = 'scion:tray-count';

/** Which tray a count belongs to. */
export type TrayCountSource = 'inbox' | 'notifications';

export interface TrayCountDetail {
  /** The tray that dispatched the count. */
  source: TrayCountSource;
  /** The number of items now in the tray's list. */
  count: number;
}

/** Dispatches TRAY_COUNT_EVENT from a tray element. */
export function dispatchTrayCount(
  target: EventTarget,
  source: TrayCountSource,
  count: number
): void {
  target.dispatchEvent(
    new CustomEvent<TrayCountDetail>(TRAY_COUNT_EVENT, {
      detail: { source, count },
      bubbles: true,
      composed: true,
    })
  );
}
