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
 * The first /terminals load shows "Loading terminals…" after a short delay
 * (the route outlet, with the whole app shell, is hidden meanwhile), and the
 * placeholder goes once the workspace mounts (ptone/scion#5306). Needs a
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

const AGENT = '0b6f8a52-1c1e-4b6a-9d55-7e2a8c3f4d10';
const loading = (): HTMLElement | null => document.getElementById('terminal-workspace-loading');

describe('main.ts terminal route', () => {
  afterEach(() => {
    vi.useRealTimers();
  });
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('terminal route shows a loading state while the workspace loads', async () => {
    const main = await bootMainAt('/admin/users');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-users')).not.toBeNull(),
      BOOT_WAIT
    );

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    main.navigateTo('/terminals');
    // renderRoute reaches the loader synchronously.
    expect(loadTerminalWorkspace).toHaveBeenCalledTimes(1);

    // Nothing for the first 150 ms, so a cached chunk does not flash it.
    vi.advanceTimersByTime(149);
    expect(loading()).toBeNull();
    vi.advanceTimersByTime(1);
    const el = loading();
    expect(el).not.toBeNull();
    expect(el!.getAttribute('role')).toBe('status');
    expect(el!.getAttribute('aria-live')).toBe('polite');
    expect(el!.textContent).toContain('Loading terminals…');
    expect(el!.querySelector('sl-spinner')).not.toBeNull();
    expect(el!.parentElement?.id).toBe('app');
    vi.useRealTimers();

    // The workspace mounts and the placeholder goes.
    resolveLoad!(fakeTerminalWorkspaceModule);
    await vi.waitFor(() => expect(fakeTerminalRecord.workspaces).toHaveLength(1), BOOT_WAIT);
    expect(loading()).toBeNull();

    // Later terminal navigations reuse the workspace: no loading state.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    main.navigateTo(`/terminals/${AGENT}`);
    vi.advanceTimersByTime(1000);
    expect(loading()).toBeNull();
    vi.useRealTimers();
    await vi.waitFor(() => expect(fakeTerminalRecord.opened).toEqual([AGENT]), BOOT_WAIT);
    expect(loadTerminalWorkspace).toHaveBeenCalledTimes(1);
  }, 15_000);
});
