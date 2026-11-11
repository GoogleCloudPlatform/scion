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
 * The client entry loads the terminal workspace chunk only when a /terminals
 * route renders, once (ptone/scion#5306).
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';
import {
  fakeTerminalRecord,
  fakeTerminalWorkspaceModule,
} from './__fixtures__/terminal-workspace-fake.js';
import type { TerminalWorkspaceModule } from './terminal-workspace-loader.js';

vi.mock('../components/pages/admin-users.js', () => {
  if (!customElements.get('scion-page-admin-users')) {
    customElements.define('scion-page-admin-users', class extends HTMLElement {});
  }
  return {};
});

const loadTerminalWorkspace = vi.fn(() => Promise.resolve(fakeTerminalWorkspaceModule));
vi.mock('./terminal-workspace-loader.js', () => ({
  loadTerminalWorkspace: (): Promise<TerminalWorkspaceModule> => loadTerminalWorkspace(),
}));

const AGENT = '0b6f8a52-1c1e-4b6a-9d55-7e2a8c3f4d10';

describe('main.ts terminal route', () => {
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('terminal route loads workspace lazily', async () => {
    const main = await bootMainAt('/admin/users');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-users')).not.toBeNull(),
      BOOT_WAIT
    );
    // A non-terminal route never loads the chunk.
    expect(loadTerminalWorkspace).not.toHaveBeenCalled();

    main.navigateTo('/terminals');
    await vi.waitFor(() => expect(fakeTerminalRecord.workspaces).toHaveLength(1), BOOT_WAIT);
    expect(loadTerminalWorkspace).toHaveBeenCalledTimes(1);
    const workspace = fakeTerminalRecord.workspaces[0]!;
    expect(workspace.element.isConnected).toBe(true);
    await vi.waitFor(() => expect(workspace.shown.at(-1)).toBe(true), BOOT_WAIT);

    // A second terminal navigation reuses the loaded workspace.
    main.navigateTo(`/terminals/${AGENT}`);
    await vi.waitFor(() => expect(fakeTerminalRecord.opened).toEqual([AGENT]), BOOT_WAIT);
    expect(loadTerminalWorkspace).toHaveBeenCalledTimes(1);
    expect(fakeTerminalRecord.workspaces).toHaveLength(1);
    expect(fakeTerminalRecord.coordinators).toHaveLength(1);
  }, 15_000);
});
