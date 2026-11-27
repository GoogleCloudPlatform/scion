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
 * Column definitions and per-browser persistence for the hub-wide agents list
 * table (`scion-page-agents`, ptone/scion#9577).
 *
 * Persisted in localStorage under `scion-columns-agents` (mirroring
 * `scion-view-agents`, `scion-sort-agents`, and `scion-pagesize-agents`) as
 * `{ hidden: AgentColumnId[] }`. Storing hidden column IDs ensures newly added
 * default-visible columns automatically appear for existing browsers, while
 * unknown, malformed, or pinned column IDs are ignored gracefully.
 */

export type AgentColumnId =
  | 'name'
  | 'project'
  | 'template'
  | 'status'
  | 'traits'
  | 'updated'
  | 'task'
  | 'actions';

export interface AgentColumnDef {
  readonly id: AgentColumnId;
  readonly label: string;
  /** Pinned columns are always visible and cannot be hidden. */
  readonly pinned?: boolean;
  /** Whether the column carries the `.hide-mobile` responsive class. */
  readonly hideMobile?: boolean;
  readonly defaultVisible: boolean;
}

export const AGENT_COLUMNS: readonly AgentColumnDef[] = [
  { id: 'name', label: 'Name', pinned: true, defaultVisible: true },
  { id: 'project', label: 'Project', defaultVisible: true },
  { id: 'template', label: 'Template', hideMobile: true, defaultVisible: true },
  { id: 'status', label: 'Status', defaultVisible: true },
  { id: 'traits', label: 'Traits', hideMobile: true, defaultVisible: true },
  { id: 'updated', label: 'Updated', hideMobile: true, defaultVisible: true },
  { id: 'task', label: 'Task', hideMobile: true, defaultVisible: true },
  { id: 'actions', label: 'Actions', pinned: true, defaultVisible: true },
] as const;

export const TOGGLEABLE_AGENT_COLUMNS: readonly AgentColumnDef[] = AGENT_COLUMNS.filter(
  (col) => !col.pinned
);

const TOGGLEABLE_ID_SET: ReadonlySet<string> = new Set(
  TOGGLEABLE_AGENT_COLUMNS.map((col) => col.id)
);

export const AGENT_COLUMNS_STORAGE_KEY = 'scion-columns-agents';

/** Returns a fresh Set containing every column that is visible by default. */
export function defaultVisibleAgentColumns(): Set<AgentColumnId> {
  return new Set(AGENT_COLUMNS.filter((col) => col.defaultVisible).map((col) => col.id));
}

/**
 * Parses a raw localStorage value into a visible column set.
 * Unknown keys, non-string entries, malformed JSON, and pinned column IDs are
 * ignored gracefully.
 */
export function parseVisibleAgentColumns(raw: string | null): Set<AgentColumnId> {
  const visible = defaultVisibleAgentColumns();
  if (!raw) return visible;

  try {
    const parsed = JSON.parse(raw) as { hidden?: unknown } | null;
    if (!parsed || typeof parsed !== 'object' || !Array.isArray(parsed.hidden)) {
      return visible;
    }
    for (const entry of parsed.hidden) {
      if (typeof entry === 'string' && TOGGLEABLE_ID_SET.has(entry)) {
        visible.delete(entry as AgentColumnId);
      }
    }
  } catch {
    // Ignore invalid JSON and fall back to defaults.
  }

  return visible;
}

/** Loads the visible columns for the hub-wide agents table from storage. */
export function loadVisibleAgentColumns(
  storage: Pick<Storage, 'getItem'> | null | undefined = typeof localStorage !== 'undefined'
    ? localStorage
    : null
): Set<AgentColumnId> {
  if (!storage) return defaultVisibleAgentColumns();
  try {
    return parseVisibleAgentColumns(storage.getItem(AGENT_COLUMNS_STORAGE_KEY));
  } catch {
    return defaultVisibleAgentColumns();
  }
}

/** Persists the visible columns for the hub-wide agents table to storage. */
export function saveVisibleAgentColumns(
  visible: ReadonlySet<AgentColumnId>,
  storage: Pick<Storage, 'setItem' | 'removeItem'> | null | undefined = typeof localStorage !==
  'undefined'
    ? localStorage
    : null
): void {
  if (!storage) return;
  const hidden = TOGGLEABLE_AGENT_COLUMNS.filter((col) => !visible.has(col.id)).map(
    (col) => col.id
  );
  try {
    if (hidden.length === 0) {
      storage.removeItem(AGENT_COLUMNS_STORAGE_KEY);
    } else {
      storage.setItem(AGENT_COLUMNS_STORAGE_KEY, JSON.stringify({ hidden }));
    }
  } catch {
    // Ignore storage quota / privacy mode errors.
  }
}

/**
 * Returns a new visible-column set with `id` toggled. Pinned columns (`name`,
 * `actions`) are always kept visible.
 */
export function toggleAgentColumn(
  visible: ReadonlySet<AgentColumnId>,
  id: AgentColumnId
): Set<AgentColumnId> {
  const next = new Set(visible);
  if (!TOGGLEABLE_ID_SET.has(id)) {
    next.add('name');
    next.add('actions');
    return next;
  }
  if (next.has(id)) {
    next.delete(id);
  } else {
    next.add(id);
  }
  // Invariant: pinned columns are always visible.
  next.add('name');
  next.add('actions');
  return next;
}

/** Reports whether the visible set matches the default (all columns visible). */
export function isDefaultVisibleAgentColumns(visible: ReadonlySet<AgentColumnId>): boolean {
  return TOGGLEABLE_AGENT_COLUMNS.every((col) => visible.has(col.id));
}
