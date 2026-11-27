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
 * Shared column visibility picker renderer used by both the hub-wide agents
 * list (`agents.ts`) and the project agents list (`project-detail.ts`,
 * ptone/scion#9577).
 *
 * Renders a Shoelace `<sl-dropdown>` + `<sl-menu>` with checkbox items for
 * every column in table order (pinned columns such as Name and Actions appear
 * checked and disabled consistently), plus a "Reset to defaults" item.
 */

import { html } from 'lit';
import type { TemplateResult } from 'lit';

import type { ColumnDef } from '../../shared/agent-columns.js';

export interface ColumnPickerOptions<T extends string> {
  readonly columns: readonly ColumnDef<T>[];
  readonly visible: ReadonlySet<T>;
  readonly onToggle: (id: T) => void;
  readonly onReset: () => void;
}

export function renderColumnPicker<T extends string>(opts: ColumnPickerOptions<T>): TemplateResult {
  const { columns, visible, onToggle, onReset } = opts;
  const toggleable = columns.filter((col) => !col.pinned);
  const visibleToggleableCount = toggleable.filter((col) => visible.has(col.id)).length;
  const isDefault = visibleToggleableCount === toggleable.length;

  return html`
    <sl-dropdown class="columns-picker" placement="bottom-end" stay-open-on-select hoist>
      <sl-button slot="trigger" size="small" outline caret data-testid="columns-picker-trigger">
        <sl-icon slot="prefix" name="layout-three-columns"></sl-icon>
        Columns${isDefault ? '' : ` (${visibleToggleableCount}/${toggleable.length})`}
      </sl-button>
      <sl-menu
        aria-label="Visible columns"
        @sl-select=${(e: CustomEvent<{ item: { value: string } }>): void => {
          const value = e.detail.item.value;
          if (value === '__reset__') {
            onReset();
          } else {
            onToggle(value as T);
          }
        }}
      >
        ${columns.map(
          (col) => html`
            <sl-menu-item
              type="checkbox"
              value=${col.id}
              ?checked=${Boolean(col.pinned) || visible.has(col.id)}
              ?disabled=${Boolean(col.pinned)}
              data-testid="column-toggle-${col.id}"
            >
              ${col.label}
            </sl-menu-item>
          `
        )}
        <sl-divider></sl-divider>
        <sl-menu-item value="__reset__" ?disabled=${isDefault} data-testid="column-toggle-reset">
          Reset to defaults
        </sl-menu-item>
      </sl-menu>
    </sl-dropdown>
  `;
}
