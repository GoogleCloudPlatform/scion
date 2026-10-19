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
 * While the terminal workspace chunk loads, the previous page is hidden: the
 * URL already says /terminals, so the old page (and its header) must not stay
 * interactive under it (ptone/scion#5306; this matches the behaviour before
 * the workspace became a lazy chunk). Needs a fresh entry module, so it is its
 * own file.
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

let resolveLoad: ((mod: TerminalWorkspaceModule) => void) | null = null;
const loadTerminalWorkspace = vi.fn(
  () =>
    new Promise<TerminalWorkspaceModule>((resolve) => {
      resolveLoad = resolve;
    })
);
vi.mock('./terminal-workspace-loader.js', () => ({
  loadTerminalWorkspace: (): Promise<TerminalWorkspaceModule> => loadTerminalWorkspace(),
}));

describe('main.ts terminal route', () => {
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('terminal route hides the previous page while the workspace loads', async () => {
    const main = await bootMainAt('/admin/users');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-users')).not.toBeNull(),
      BOOT_WAIT
    );
    const outlet = document.getElementById('route-outlet')!;
    expect(outlet.hidden).toBe(false);

    main.navigateTo('/terminals');
    await vi.waitFor(() => expect(loadTerminalWorkspace).toHaveBeenCalledTimes(1), BOOT_WAIT);
    // The load is still pending: the previous page is already hidden, and
    // nothing of the workspace exists yet.
    expect(outlet.hidden).toBe(true);
    expect(fakeTerminalRecord.workspaces).toHaveLength(0);

    resolveLoad!(fakeTerminalWorkspaceModule);
    await vi.waitFor(() => expect(fakeTerminalRecord.workspaces).toHaveLength(1), BOOT_WAIT);
    const workspace = fakeTerminalRecord.workspaces[0]!;
    await vi.waitFor(() => expect(workspace.shown.at(-1)).toBe(true), BOOT_WAIT);
    expect(outlet.hidden).toBe(true);
  }, 15_000);
});
