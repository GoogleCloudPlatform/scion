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
 * A stand-in for the lazily loaded terminal workspace chunk
 * (terminal-workspace-entry.ts), for tests of the client entry module's
 * /terminals route. It records what main.ts builds and mounts, and touches no
 * network, storage or BroadcastChannel.
 */

import type { TerminalWorkspaceModule } from '../terminal-workspace-loader.js';

export interface FakeTerminalWorkspaceRecord {
  /** Workspaces constructed, in order. */
  workspaces: FakeTerminalWorkspaceRoot[];
  /** Coordinators constructed, in order. */
  coordinators: FakeTerminalCoordinator[];
  /** Agent ids passed to coordinator.open(). */
  opened: string[];
}

export const fakeTerminalRecord: FakeTerminalWorkspaceRecord = {
  workspaces: [],
  coordinators: [],
  opened: [],
};

export class FakeTerminalWorkspaceRoot {
  readonly element = document.createElement('div');
  readonly shown: boolean[] = [];
  readonly paths: string[] = [];
  readonly layoutManager = { restore: (): void => undefined };
  constructor() {
    this.element.dataset['fakeTerminalWorkspace'] = '';
    fakeTerminalRecord.workspaces.push(this);
  }
  setUser(): void {}
  setCurrentPath(path: string): void {
    this.paths.push(path);
  }
  show(visible: boolean): void {
    this.shown.push(visible);
  }
  setStatus(): void {}
  setStatusAction(): void {}
  setSuppressUrlSync(): void {}
  syncUrlFromLayout(): void {}
}

export class FakeTerminalCoordinator {
  readonly supported = false;
  readonly isOwner = true;
  constructor() {
    fakeTerminalRecord.coordinators.push(this);
  }
  onRelinquished(): void {}
  open(agentId: string): Promise<{ status: 'opened' }> {
    fakeTerminalRecord.opened.push(agentId);
    return Promise.resolve({ status: 'opened' });
  }
  stop(): void {}
  teardownAccount(): void {}
}

export class FakeTerminalWorkspacePersistence {
  restore(): Promise<void> {
    return Promise.resolve();
  }
}

/** The fake chunk, typed as the real module so main.ts accepts it. */
export const fakeTerminalWorkspaceModule = {
  TerminalCoordinator: FakeTerminalCoordinator,
  TerminalWorkspaceRoot: FakeTerminalWorkspaceRoot,
  TerminalWorkspacePersistence: FakeTerminalWorkspacePersistence,
  restoreUrlIntent: () => ({}),
  openLayoutSlots: () => Promise.resolve(null),
  parseLayoutUrl: () => null,
  MOVE_TERMINALS_LABEL: 'Move terminals to this window',
  TERMINALS_OPEN_ELSEWHERE_HELP: '',
  TERMINALS_OPEN_ELSEWHERE_HELP_LABEL: '',
  TERMINALS_OPEN_ELSEWHERE_STATUS: '',
  nonOwnerOpenStatus: () => '',
  offersMove: () => false,
  openPalettePickedAgent: () => Promise.resolve(),
  moveTerminalsWithStatus: () => Promise.resolve({ status: 'failed' }),
  terminalsOpenElsewhere: () => Promise.resolve(false),
} as unknown as TerminalWorkspaceModule;
