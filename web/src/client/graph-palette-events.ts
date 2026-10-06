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
 * Event contract between the header's palette button and the graph views
 * that host a "Jump to agent" palette (`GraphPaletteController`). The
 * header is not an ancestor of the page it sits above, so the two talk
 * through window-level events, the same as the chat palette.
 */

/** Dispatched by the header's palette button, over a graph, to request the palette open. */
export const GRAPH_PALETTE_OPEN_REQUEST_EVENT = 'scion:graph-palette-open-request';

/**
 * Dispatched on `window` whenever {@link isGraphPaletteAvailable} changes,
 * so the header can show or hide its palette button.
 */
export const GRAPH_PALETTE_AVAILABILITY_EVENT = 'scion:graph-palette-availability';

const availableOwners = new Set<object>();

/** Whether any graph view currently on screen offers the palette. */
export function isGraphPaletteAvailable(): boolean {
  return availableOwners.size > 0;
}

/**
 * Records whether `owner`'s graph currently offers the palette, and
 * announces a change of {@link isGraphPaletteAvailable}.
 */
export function setGraphPaletteAvailable(owner: object, available: boolean): void {
  const before = isGraphPaletteAvailable();
  if (available) availableOwners.add(owner);
  else availableOwners.delete(owner);
  if (isGraphPaletteAvailable() !== before) {
    window.dispatchEvent(new Event(GRAPH_PALETTE_AVAILABILITY_EVENT));
  }
}
