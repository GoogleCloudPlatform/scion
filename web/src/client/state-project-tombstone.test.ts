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
 * Project tombstones (ptone/scion#5332): a project re-created with a
 * previously deleted ID must leave the deleted set, so consumers that skip
 * deleted IDs keep the re-created project.
 */

import { describe, it, expect } from 'vitest';
import { StateManager } from './state.js';

/** Feed a subject/data pair through the SSE update path. */
function emit(sm: StateManager, subject: string, data: unknown): void {
  (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
    subject,
    data,
  });
}

describe('StateManager project tombstones', () => {
  it('records a deleted project ID', () => {
    const sm = new StateManager();
    emit(sm, 'project.p1.created', { projectId: 'p1', name: 'One' });
    emit(sm, 'project.p1.deleted', { projectId: 'p1' });
    expect(sm.getDeletedProjectIds().has('p1')).toBe(true);
    expect(sm.getProject('p1')).toBeUndefined();
  });

  it('clears the tombstone when a project is re-created with the same ID', () => {
    const sm = new StateManager();
    emit(sm, 'project.p1.created', { projectId: 'p1', name: 'One' });
    emit(sm, 'project.p1.deleted', { projectId: 'p1' });
    emit(sm, 'project.p1.created', { projectId: 'p1', name: 'One again' });
    expect(sm.getDeletedProjectIds().has('p1')).toBe(false);
    expect(sm.getProject('p1')?.name).toBe('One again');
  });

  it('keeps the tombstone for a late update to a deleted project', () => {
    const sm = new StateManager();
    emit(sm, 'project.p1.created', { projectId: 'p1', name: 'One' });
    emit(sm, 'project.p1.deleted', { projectId: 'p1' });
    emit(sm, 'project.p1.updated', { projectId: 'p1', name: 'Late' });
    expect(sm.getDeletedProjectIds().has('p1')).toBe(true);
  });
});
