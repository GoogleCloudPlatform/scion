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
 * The terminal workspace, as one lazily loaded chunk (ptone/scion#5306).
 *
 * The client entry (main.ts) reaches these modules only through
 * terminal-workspace-loader.ts, which import()s this barrel the first time a
 * /terminals route renders. Rollup then emits one terminal-workspace chunk
 * instead of putting the workspace into main.js on every route. main.ts keeps
 * type-only imports of these modules; a value import there would pull the
 * code back into the entry, and the bundle budget check
 * (web/scripts/check-bundle-budget.mjs) fails the build if that happens.
 */

export { TerminalCoordinator } from './terminal-coordinator.js';
export { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
export { TerminalWorkspacePersistence, restoreUrlIntent } from './terminal-persistence.js';
export { openLayoutSlots } from './terminal-layout-open.js';
export { parseLayoutUrl } from './terminal-layout.js';
export {
  MOVE_TERMINALS_LABEL,
  TERMINALS_OPEN_ELSEWHERE_HELP,
  TERMINALS_OPEN_ELSEWHERE_HELP_LABEL,
  TERMINALS_OPEN_ELSEWHERE_STATUS,
  nonOwnerOpenStatus,
  offersMove,
  openPalettePickedAgent,
} from './terminal-palette-open.js';
export { moveTerminalsWithStatus, terminalsOpenElsewhere } from './terminal-move.js';
