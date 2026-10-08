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

import { afterEach, describe, expect, it, vi } from 'vitest';
import { FakeEventSource } from './__fixtures__/agent-store-harness.js';
import {
  READINESS_MARKS,
  READINESS_MARK_PREFIX,
  configureReadinessMarks,
  markReady,
  readinessMarkPending,
  readinessNavigationStarted,
  resetReadinessMarks,
} from './readiness-marks.js';
import { StateManager } from './state.js';

function readinessMarks(): PerformanceMark[] {
  return (performance.getEntriesByType('mark') as PerformanceMark[]).filter((m) =>
    m.name.startsWith(READINESS_MARK_PREFIX)
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  configureReadinessMarks(true);
  resetReadinessMarks();
  configureReadinessMarks(false);
});

describe('readiness marks', () => {
  it('has stable names under one prefix', () => {
    expect(Object.values(READINESS_MARKS)).toEqual([
      'scion:ready:agents-data',
      'scion:ready:rows-grid',
      'scion:ready:rows-list',
      'scion:ready:graph',
    ]);
    for (const name of Object.values(READINESS_MARKS)) {
      expect(name.startsWith(READINESS_MARK_PREFIX)).toBe(true);
    }
  });

  it('writes nothing while off, the default', () => {
    for (const name of Object.values(READINESS_MARKS)) {
      markReady(name, 'grid');
      expect(readinessMarkPending(name)).toBe(false);
    }
    readinessNavigationStarted(2);
    resetReadinessMarks();
    expect(readinessMarks()).toEqual([]);
  });

  it('writes nothing when configured off', () => {
    configureReadinessMarks(false);
    markReady(READINESS_MARKS.agentsData, 'list');
    expect(readinessMarks()).toEqual([]);
  });

  it('writes each mark once per load, with only the view as detail', () => {
    configureReadinessMarks(true);
    markReady(READINESS_MARKS.agentsData, 'grid');
    markReady(READINESS_MARKS.agentsData, 'grid');
    markReady(READINESS_MARKS.rowsGrid, 'grid');
    markReady(READINESS_MARKS.rowsGrid, 'grid');
    const marks = readinessMarks();
    expect(marks.map((m) => m.name)).toEqual([
      READINESS_MARKS.agentsData,
      READINESS_MARKS.rowsGrid,
    ]);
    for (const m of marks) {
      expect(m.detail).toEqual({ view: 'grid' });
    }
    expect(readinessMarkPending(READINESS_MARKS.agentsData)).toBe(false);
    expect(readinessMarkPending(READINESS_MARKS.graph)).toBe(true);
  });

  it('starts a new load on a client-side navigation, not on the first render', () => {
    configureReadinessMarks(true);
    readinessNavigationStarted(1);
    markReady(READINESS_MARKS.graph, 'graph');
    readinessNavigationStarted(1);
    expect(readinessMarks()).toHaveLength(1);
    readinessNavigationStarted(2);
    expect(readinessMarks()).toEqual([]);
    markReady(READINESS_MARKS.graph, 'graph');
    expect(readinessMarks()).toHaveLength(1);
  });

  it('starts a new load on a scope change only', () => {
    vi.stubGlobal('EventSource', FakeEventSource);
    configureReadinessMarks(true);
    const state = new StateManager();
    state.setScope({ type: 'project', projectId: 'p1' });
    markReady(READINESS_MARKS.agentsData, 'list');
    state.setScope({ type: 'project', projectId: 'p1' });
    expect(readinessMarks()).toHaveLength(1);
    state.setScope({ type: 'project', projectId: 'p2' });
    expect(readinessMarks()).toEqual([]);
    expect(readinessMarkPending(READINESS_MARKS.agentsData)).toBe(true);
    markReady(READINESS_MARKS.agentsData, 'list');
    expect(readinessMarks()).toHaveLength(1);
    state.disconnect();
  });
});
