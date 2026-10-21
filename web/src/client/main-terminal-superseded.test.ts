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
 * A navigation away from /terminals while the terminal workspace chunk is
 * still loading wins: nothing is built or mounted (ptone/scion#5306). This
 * needs a fresh entry module (no coordinator yet), so it is its own file.
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
vi.mock('../components/pages/admin-groups.js', () => {
  if (!customElements.get('scion-page-admin-groups')) {
    customElements.define('scion-page-admin-groups', class extends HTMLElement {});
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

  it('navigating away during workspace load does not mount', async () => {
    const main = await bootMainAt('/admin/users');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-users')).not.toBeNull(),
      BOOT_WAIT
    );
    const outlet = document.getElementById('route-outlet')!;

    main.navigateTo('/terminals');
    await vi.waitFor(() => expect(loadTerminalWorkspace).toHaveBeenCalledTimes(1), BOOT_WAIT);
    // While the chunk loads, the previous page is hidden: the URL already
    // says /terminals, so the old page must not stay interactive under it.
    expect(outlet.hidden).toBe(true);
    // The loading placeholder appears after its delay ...
    await vi.waitFor(
      () => expect(document.getElementById('terminal-workspace-loading')).not.toBeNull(),
      BOOT_WAIT
    );

    main.navigateTo('/admin/groups');
    // ... and the newer navigation removes it at once, before the load settles.
    expect(document.getElementById('terminal-workspace-loading')).toBeNull();
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-groups')).not.toBeNull(),
      BOOT_WAIT
    );
    resolveLoad!(fakeTerminalWorkspaceModule);
    // Let the superseded render observe the resolved load.
    await new Promise((r) => setTimeout(r, 0));

    expect(fakeTerminalRecord.workspaces).toHaveLength(0);
    expect(fakeTerminalRecord.coordinators).toHaveLength(0);
    expect(document.querySelector('[data-fake-terminal-workspace]')).toBeNull();
    expect(document.getElementById('terminal-workspace-loading')).toBeNull();
    // The newer navigation un-hid the outlet; the superseded load left it alone.
    expect(outlet.hidden).toBe(false);
  }, 15_000);
});
