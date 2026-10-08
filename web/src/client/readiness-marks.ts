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
 * Readiness marks: User Timing marks (performance.mark) that time when a
 * page's agent data arrived, when its first rows became visible and when
 * the graph was laid out. They are written only when the hub's profiling
 * readiness_marks setting is on, which the server hands over in the
 * shell's initial data (readinessMarks) for a signed-in user.
 *
 * - Off (the default) writes nothing: markReady returns before touching
 *   performance or any state.
 * - Each mark is written at most once per load. A load ends on a
 *   client-side navigation or a change of the view scope, which clears
 *   the marks (resetReadinessMarks).
 * - A mark carries timing only: its name, its startTime and a detail with
 *   the view it was taken in. No agent names, ids or row data.
 */

/** Prefix shared by every readiness mark name. */
export const READINESS_MARK_PREFIX = 'scion:ready:';

/** The stable readiness mark names. perf/bench reads these. */
export const READINESS_MARKS = {
  /** The first agent result for the current scope was adopted. */
  agentsData: 'scion:ready:agents-data',
  /** The grid showed its first page of cards (or its empty state). */
  rowsGrid: 'scion:ready:rows-grid',
  /** The list showed its first page of rows (or its empty state). */
  rowsList: 'scion:ready:rows-list',
  /** The agent graph was laid out and fitted to the viewport. */
  graph: 'scion:ready:graph',
} as const;

export type ReadinessMarkName = (typeof READINESS_MARKS)[keyof typeof READINESS_MARKS];

/** The agent view a mark was taken in; the only detail a mark carries. */
export type ReadinessView = 'grid' | 'list' | 'graph';

const ALL_MARKS: readonly ReadinessMarkName[] = Object.values(READINESS_MARKS);

let enabled = false;
const written = new Set<ReadinessMarkName>();

/** Sets whether marks are written, once per document, from the shell's initial data. */
export function configureReadinessMarks(on: boolean): void {
  enabled = on === true;
  written.clear();
}

/** Whether readiness marks are on for this document. */
export function readinessMarksEnabled(): boolean {
  return enabled;
}

/** Whether the named mark is still to be written in this load (false when marks are off). */
export function readinessMarkPending(name: ReadinessMarkName): boolean {
  return enabled && !written.has(name);
}

/** Writes the named mark, unless marks are off or it was already written in this load. */
export function markReady(name: ReadinessMarkName, view: ReadinessView): void {
  if (!enabled || written.has(name)) return;
  written.add(name);
  try {
    performance.mark(name, { detail: { view } });
  } catch {
    // User Timing unavailable: nothing to record.
  }
}

/**
 * Called at the start of every route render with the router's navigation
 * counter (1 for the document's first render): every later render is a
 * client-side navigation, which ends the previous page's load.
 */
export function readinessNavigationStarted(navigationId: number): void {
  if (navigationId > 1) resetReadinessMarks();
}

/** Ends the current load: clears the written marks so the next load can write them again. */
export function resetReadinessMarks(): void {
  if (!enabled) return;
  written.clear();
  for (const name of ALL_MARKS) {
    try {
      performance.clearMarks(name);
    } catch {
      // User Timing unavailable.
    }
  }
}
