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
 * table (`scion-page-agents`) and the project agents list table
 * (`scion-page-project-detail`, ptone/scion#9577).
 *
 * Each table uses its own localStorage key (`scion-columns-agents` and
 * `scion-columns-project-agents`, mirroring `scion-view-agents` /
 * `scion-view-project-agents` and `scion-pagesize-*`) because their column
 * sets differ (hub-wide has Project and Traits; project-detail has Broker).
 *
 * Both store `{ hidden: ColumnId[] }`: storing hidden column IDs ensures newly
 * added default-visible columns automatically appear for existing browsers,
 * while unknown, malformed, or pinned column IDs are ignored gracefully.
 */

export interface ColumnDef<T extends string> {
  readonly id: T;
  readonly label: string;
  /** Pinned columns are always visible and cannot be hidden. */
  readonly pinned?: boolean;
  /** Whether the column carries the `.hide-mobile` responsive class. */
  readonly hideMobile?: boolean;
  readonly defaultVisible: boolean;
}

export type AgentColumnId =
  | 'name'
  | 'project'
  | 'template'
  | 'status'
  | 'traits'
  | 'updated'
  | 'task'
  | 'actions';

export type AgentColumnDef = ColumnDef<AgentColumnId>;

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

export const AGENT_COLUMNS_STORAGE_KEY = 'scion-columns-agents';

export type ProjectAgentColumnId =
  | 'name'
  | 'template'
  | 'broker'
  | 'status'
  | 'updated'
  | 'task'
  | 'actions';

export type ProjectAgentColumnDef = ColumnDef<ProjectAgentColumnId>;

export const PROJECT_AGENT_COLUMNS: readonly ProjectAgentColumnDef[] = [
  { id: 'name', label: 'Name', pinned: true, defaultVisible: true },
  { id: 'template', label: 'Template', hideMobile: true, defaultVisible: true },
  { id: 'broker', label: 'Broker', hideMobile: true, defaultVisible: true },
  { id: 'status', label: 'Status', defaultVisible: true },
  { id: 'updated', label: 'Updated', hideMobile: true, defaultVisible: true },
  { id: 'task', label: 'Task', hideMobile: true, defaultVisible: true },
  { id: 'actions', label: 'Actions', pinned: true, defaultVisible: true },
] as const;

export const TOGGLEABLE_PROJECT_AGENT_COLUMNS: readonly ProjectAgentColumnDef[] =
  PROJECT_AGENT_COLUMNS.filter((col) => !col.pinned);

export const PROJECT_AGENT_COLUMNS_STORAGE_KEY = 'scion-columns-project-agents';

interface ColumnVisibilityHelpers<T extends string> {
  readonly defaultVisible: () => Set<T>;
  readonly parse: (raw: string | null) => Set<T>;
  readonly load: (storage?: Pick<Storage, 'getItem'> | null) => Set<T>;
  readonly save: (
    visible: ReadonlySet<T>,
    storage?: Pick<Storage, 'setItem' | 'removeItem'> | null
  ) => void;
  readonly toggle: (visible: ReadonlySet<T>, id: T) => Set<T>;
  readonly isDefault: (visible: ReadonlySet<T>) => boolean;
}

function createColumnVisibilityHelpers<T extends string>(
  columns: readonly ColumnDef<T>[],
  storageKey: string
): ColumnVisibilityHelpers<T> {
  const toggleable = columns.filter((col) => !col.pinned);
  const pinnedIds = columns.filter((col) => col.pinned).map((col) => col.id);
  const toggleableIdSet: ReadonlySet<string> = new Set(toggleable.map((col) => col.id));

  const defaultVisible = (): Set<T> =>
    new Set(columns.filter((col) => col.defaultVisible).map((col) => col.id));

  const parse = (raw: string | null): Set<T> => {
    const visible = defaultVisible();
    if (!raw) return visible;

    try {
      const parsed = JSON.parse(raw) as { hidden?: unknown } | null;
      if (!parsed || typeof parsed !== 'object' || !Array.isArray(parsed.hidden)) {
        return visible;
      }
      for (const entry of parsed.hidden) {
        if (typeof entry === 'string' && toggleableIdSet.has(entry)) {
          visible.delete(entry as T);
        }
      }
    } catch {
      // Ignore invalid JSON and fall back to defaults.
    }

    return visible;
  };

  const load = (
    storage: Pick<Storage, 'getItem'> | null | undefined = typeof localStorage !== 'undefined'
      ? localStorage
      : null
  ): Set<T> => {
    if (!storage) return defaultVisible();
    try {
      return parse(storage.getItem(storageKey));
    } catch {
      return defaultVisible();
    }
  };

  const save = (
    visible: ReadonlySet<T>,
    storage: Pick<Storage, 'setItem' | 'removeItem'> | null | undefined = typeof localStorage !==
    'undefined'
      ? localStorage
      : null
  ): void => {
    if (!storage) return;
    const hidden = toggleable.filter((col) => !visible.has(col.id)).map((col) => col.id);
    try {
      if (hidden.length === 0) {
        storage.removeItem(storageKey);
      } else {
        storage.setItem(storageKey, JSON.stringify({ hidden }));
      }
    } catch {
      // Ignore storage quota / privacy mode errors.
    }
  };

  const toggle = (visible: ReadonlySet<T>, id: T): Set<T> => {
    const next = new Set(visible);
    if (toggleableIdSet.has(id)) {
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
    }
    for (const pinned of pinnedIds) {
      next.add(pinned);
    }
    return next;
  };

  const isDefault = (visible: ReadonlySet<T>): boolean =>
    toggleable.every((col) => visible.has(col.id));

  return { defaultVisible, parse, load, save, toggle, isDefault };
}

const hubHelpers = createColumnVisibilityHelpers(AGENT_COLUMNS, AGENT_COLUMNS_STORAGE_KEY);
const projectHelpers = createColumnVisibilityHelpers(
  PROJECT_AGENT_COLUMNS,
  PROJECT_AGENT_COLUMNS_STORAGE_KEY
);

/** Returns a fresh Set containing every hub-wide agents column visible by default. */
export const defaultVisibleAgentColumns = hubHelpers.defaultVisible;
export const parseVisibleAgentColumns = hubHelpers.parse;
export const loadVisibleAgentColumns = hubHelpers.load;
export const saveVisibleAgentColumns = hubHelpers.save;
export const toggleAgentColumn = hubHelpers.toggle;
export const isDefaultVisibleAgentColumns = hubHelpers.isDefault;

/** Returns a fresh Set containing every project agents column visible by default. */
export const defaultVisibleProjectAgentColumns = projectHelpers.defaultVisible;
export const parseVisibleProjectAgentColumns = projectHelpers.parse;
export const loadVisibleProjectAgentColumns = projectHelpers.load;
export const saveVisibleProjectAgentColumns = projectHelpers.save;
export const toggleProjectAgentColumn = projectHelpers.toggle;
export const isDefaultVisibleProjectAgentColumns = projectHelpers.isDefault;
