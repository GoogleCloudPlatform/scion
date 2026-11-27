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

import { describe, it, expect, beforeEach } from 'vitest';

import {
  AGENT_COLUMNS,
  AGENT_COLUMNS_STORAGE_KEY,
  PROJECT_AGENT_COLUMNS,
  PROJECT_AGENT_COLUMNS_STORAGE_KEY,
  TOGGLEABLE_AGENT_COLUMNS,
  TOGGLEABLE_PROJECT_AGENT_COLUMNS,
  defaultVisibleAgentColumns,
  defaultVisibleProjectAgentColumns,
  isDefaultVisibleAgentColumns,
  isDefaultVisibleProjectAgentColumns,
  loadVisibleAgentColumns,
  loadVisibleProjectAgentColumns,
  parseVisibleAgentColumns,
  parseVisibleProjectAgentColumns,
  saveVisibleAgentColumns,
  saveVisibleProjectAgentColumns,
  toggleAgentColumn,
  toggleProjectAgentColumn,
} from './agent-columns.js';

describe('agent-columns persistence and helpers', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('defaults to all current columns visible (including traits)', () => {
    const visible = defaultVisibleAgentColumns();
    for (const col of AGENT_COLUMNS) {
      expect(visible.has(col.id)).toBe(true);
    }
    expect(visible.has('traits')).toBe(true);
    expect(isDefaultVisibleAgentColumns(visible)).toBe(true);
  });

  it('marks name and actions as pinned and excludes them from toggleable columns', () => {
    const toggleableIds = TOGGLEABLE_AGENT_COLUMNS.map((c) => c.id);
    expect(toggleableIds).toEqual(['project', 'template', 'status', 'traits', 'updated', 'task']);
    expect(toggleableIds).not.toContain('name');
    expect(toggleableIds).not.toContain('actions');
  });

  it('saves and restores hidden columns via localStorage', () => {
    let visible = defaultVisibleAgentColumns();
    visible = toggleAgentColumn(visible, 'template');
    visible = toggleAgentColumn(visible, 'task');

    saveVisibleAgentColumns(visible);
    expect(localStorage.getItem(AGENT_COLUMNS_STORAGE_KEY)).toBe(
      JSON.stringify({ hidden: ['template', 'task'] })
    );

    const restored = loadVisibleAgentColumns();
    expect(restored.has('name')).toBe(true);
    expect(restored.has('actions')).toBe(true);
    expect(restored.has('project')).toBe(true);
    expect(restored.has('status')).toBe(true);
    expect(restored.has('traits')).toBe(true);
    expect(restored.has('updated')).toBe(true);
    expect(restored.has('template')).toBe(false);
    expect(restored.has('task')).toBe(false);
    expect(isDefaultVisibleAgentColumns(restored)).toBe(false);
  });

  it('removes the storage key when all columns are restored to default', () => {
    let visible = toggleAgentColumn(defaultVisibleAgentColumns(), 'template');
    saveVisibleAgentColumns(visible);
    expect(localStorage.getItem(AGENT_COLUMNS_STORAGE_KEY)).not.toBeNull();

    visible = toggleAgentColumn(visible, 'template');
    saveVisibleAgentColumns(visible);
    expect(localStorage.getItem(AGENT_COLUMNS_STORAGE_KEY)).toBeNull();
    expect(isDefaultVisibleAgentColumns(loadVisibleAgentColumns())).toBe(true);
  });

  it('never hides pinned columns even when toggled or present in stored hidden list', () => {
    const afterName = toggleAgentColumn(defaultVisibleAgentColumns(), 'name');
    expect(afterName.has('name')).toBe(true);

    const afterActions = toggleAgentColumn(defaultVisibleAgentColumns(), 'actions');
    expect(afterActions.has('actions')).toBe(true);

    localStorage.setItem(
      AGENT_COLUMNS_STORAGE_KEY,
      JSON.stringify({ hidden: ['name', 'actions', 'task'] })
    );
    const restored = loadVisibleAgentColumns();
    expect(restored.has('name')).toBe(true);
    expect(restored.has('actions')).toBe(true);
    expect(restored.has('task')).toBe(false);
  });

  it('ignores unknown keys, non-string items, and invalid JSON gracefully', () => {
    expect(parseVisibleAgentColumns(null)).toEqual(defaultVisibleAgentColumns());
    expect(parseVisibleAgentColumns('not-json{')).toEqual(defaultVisibleAgentColumns());
    expect(parseVisibleAgentColumns('null')).toEqual(defaultVisibleAgentColumns());
    expect(parseVisibleAgentColumns('"string"')).toEqual(defaultVisibleAgentColumns());
    expect(parseVisibleAgentColumns(JSON.stringify({ hidden: 'template' }))).toEqual(
      defaultVisibleAgentColumns()
    );

    const withUnknown = parseVisibleAgentColumns(
      JSON.stringify({ hidden: ['nonexistent', 42, null, 'traits', 'also-unknown'] })
    );
    expect(withUnknown.has('traits')).toBe(false);
    expect(withUnknown.has('name')).toBe(true);
    expect(withUnknown.has('project')).toBe(true);
    expect(withUnknown.size).toBe(AGENT_COLUMNS.length - 1);
  });
});

describe('project-agent-columns persistence and helpers', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('defaults to all project agents columns visible with name and actions pinned', () => {
    const visible = defaultVisibleProjectAgentColumns();
    for (const col of PROJECT_AGENT_COLUMNS) {
      expect(visible.has(col.id)).toBe(true);
    }
    expect(TOGGLEABLE_PROJECT_AGENT_COLUMNS.map((c) => c.id)).toEqual([
      'template',
      'broker',
      'status',
      'updated',
      'task',
    ]);
    expect(isDefaultVisibleProjectAgentColumns(visible)).toBe(true);
  });

  it('persists under scion-columns-project-agents independently of scion-columns-agents', () => {
    const projVisible = toggleProjectAgentColumn(
      toggleProjectAgentColumn(defaultVisibleProjectAgentColumns(), 'broker'),
      'task'
    );
    saveVisibleProjectAgentColumns(projVisible);

    expect(localStorage.getItem(PROJECT_AGENT_COLUMNS_STORAGE_KEY)).toBe(
      JSON.stringify({ hidden: ['broker', 'task'] })
    );
    expect(localStorage.getItem(AGENT_COLUMNS_STORAGE_KEY)).toBeNull();

    const restored = loadVisibleProjectAgentColumns();
    expect(restored.has('broker')).toBe(false);
    expect(restored.has('task')).toBe(false);
    expect(restored.has('name')).toBe(true);
    expect(restored.has('actions')).toBe(true);
    expect(isDefaultVisibleProjectAgentColumns(restored)).toBe(false);
  });

  it('never hides pinned columns and ignores unknown or hub-only keys', () => {
    const parsed = parseVisibleProjectAgentColumns(
      JSON.stringify({ hidden: ['name', 'actions', 'project', 'traits', 'broker'] })
    );
    expect(parsed.has('name')).toBe(true);
    expect(parsed.has('actions')).toBe(true);
    expect(parsed.has('broker')).toBe(false);
    expect(parsed.size).toBe(PROJECT_AGENT_COLUMNS.length - 1);
  });
});
