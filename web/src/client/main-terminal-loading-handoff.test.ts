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
 * A second terminal navigation during the first workspace load keeps the
 * loading placeholder (no blank flash, no restarted delay), and the first
 * navigation's load settling does not remove it (ptone/scion#5306). Needs a
 * fresh entry module, so it is its own file.
 */

import { afterAll, afterEach, describe, expect, it, vi } from 'vitest';
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

// One pending load per call, so each navigation's load settles on its own.
const resolvers: Array<(mod: TerminalWorkspaceModule) => void> = [];
const loadTerminalWorkspace = vi.fn(
  () =>
    new Promise<TerminalWorkspaceModule>((resolve) => {
      resolvers.push(resolve);
    })
);
vi.mock('./terminal-workspace-loader.js', () => ({
  loadTerminalWorkspace: (): Promise<TerminalWorkspaceModule> => loadTerminalWorkspace(),
}));

const AGENT = '0b6f8a52-1c1e-4b6a-9d55-7e2a8c3f4d10';
const loading = (): HTMLElement | null => document.getElementById('terminal-workspace-loading');

describe('main.ts terminal route', () => {
  afterEach(() => {
    vi.useRealTimers();
  });
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('a newer terminal navigation during the first load keeps the loading state', async () => {
    const main = await bootMainAt('/admin/users');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-users')).not.toBeNull(),
      BOOT_WAIT
    );

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    main.navigateTo('/terminals');
    vi.advanceTimersByTime(150);
    vi.advanceTimersByTime(1);
    const shown = loading();
    expect(shown).not.toBeNull();

    // A second first-load terminal navigation adopts the same placeholder.
    main.navigateTo(`/terminals/${AGENT}`);
    expect(loadTerminalWorkspace).toHaveBeenCalledTimes(2);
    expect(loading()).toBe(shown);
    vi.useRealTimers();

    // The first navigation's load settles (superseded): the placeholder stays.
    resolvers[0]!(fakeTerminalWorkspaceModule);
    await new Promise((r) => setTimeout(r, 0));
    expect(fakeTerminalRecord.workspaces).toHaveLength(0);
    expect(loading()).toBe(shown);
    expect(shown!.isConnected).toBe(true);

    // The newer navigation's load mounts the workspace and removes it.
    resolvers[1]!(fakeTerminalWorkspaceModule);
    await vi.waitFor(() => expect(fakeTerminalRecord.workspaces).toHaveLength(1), BOOT_WAIT);
    expect(loading()).toBeNull();
    await vi.waitFor(() => expect(fakeTerminalRecord.opened).toEqual([AGENT]), BOOT_WAIT);
  }, 15_000);
});
