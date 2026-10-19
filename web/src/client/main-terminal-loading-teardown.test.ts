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
 * An account teardown (logout or auth expiry) during the first terminal
 * workspace load removes the loading placeholder at once (ptone/scion#5306).
 * Teardown is final for the entry module, so it is its own file.
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

  it('account teardown during the first load removes the loading state', async () => {
    const infoSpy = vi.spyOn(console, 'info');
    const main = await bootMainAt('/admin/users');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-users')).not.toBeNull(),
      BOOT_WAIT
    );
    // main.ts registers its teardown listener once the first page has rendered.
    await vi.waitFor(
      () =>
        expect(
          infoSpy.mock.calls.some((c) => String(c[0]).includes('initialization complete'))
        ).toBe(true),
      BOOT_WAIT
    );

    main.navigateTo('/terminals');
    await vi.waitFor(
      () => expect(document.getElementById('terminal-workspace-loading')).not.toBeNull(),
      BOOT_WAIT
    );

    window.dispatchEvent(
      new CustomEvent('scion:account-teardown', { detail: { reason: 'logout' } })
    );
    expect(document.getElementById('terminal-workspace-loading')).toBeNull();

    // The load settling later mounts nothing and shows nothing again.
    resolveLoad!(fakeTerminalWorkspaceModule);
    await new Promise((r) => setTimeout(r, 0));
    expect(fakeTerminalRecord.workspaces).toHaveLength(0);
    expect(document.getElementById('terminal-workspace-loading')).toBeNull();
    infoSpy.mockRestore();
  }, 15_000);
});
