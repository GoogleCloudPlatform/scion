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
 * A failed terminal workspace chunk load (for example after a deploy removed
 * the chunk) brings the previous page back (it is hidden while the chunk
 * loads), shows a toast, mounts nothing, and the
 * next /terminals visit loads again (ptone/scion#5306). main.ts has no route
 * error view; this is the same as a failed page chunk plus the toast. A fresh
 * entry module (no coordinator yet) is needed, so it is its own file.
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';
import {
  fakeTerminalRecord,
  fakeTerminalWorkspaceModule,
} from './__fixtures__/terminal-workspace-fake.js';
import type { TerminalWorkspaceModule } from './terminal-workspace-loader.js';
import { deepActiveElement } from '../components/shared/deep-active-element.js';

vi.mock('../components/pages/admin-users.js', () => {
  if (!customElements.get('scion-page-admin-users')) {
    customElements.define('scion-page-admin-users', class extends HTMLElement {});
  }
  return {};
});

const showToast = vi.fn();
vi.mock('../utils/toast.js', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../utils/toast.js')>()),
  showToast: (...args: unknown[]): void => {
    showToast(...args);
  },
}));

let rejectLoad: ((err: Error) => void) | null = null;
const loadTerminalWorkspace = vi
  .fn<() => Promise<TerminalWorkspaceModule>>()
  .mockImplementationOnce(
    () =>
      new Promise<TerminalWorkspaceModule>((_resolve, reject) => {
        rejectLoad = reject;
      })
  )
  .mockResolvedValue(fakeTerminalWorkspaceModule);
vi.mock('./terminal-workspace-loader.js', () => ({
  loadTerminalWorkspace: (): Promise<TerminalWorkspaceModule> => loadTerminalWorkspace(),
}));

describe('main.ts terminal route', () => {
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('a failed workspace load brings the page back with focus, shows a toast, and the next visit retries', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const main = await bootMainAt('/admin/users');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-users')).not.toBeNull(),
      BOOT_WAIT
    );
    const outlet = document.getElementById('route-outlet')!;
    // Something on the page has focus when the user opens /terminals. Pages
    // are Lit elements, so it sits inside a shadow root, where
    // document.activeElement only sees the host.
    const host = document.createElement('div');
    document.querySelector('scion-page-admin-users')!.appendChild(host);
    const focused = document.createElement('button');
    host.attachShadow({ mode: 'open' }).appendChild(focused);
    focused.focus();
    expect(document.activeElement).toBe(host);
    expect(deepActiveElement()).toBe(focused);

    main.navigateTo('/terminals');
    await vi.waitFor(() => expect(loadTerminalWorkspace).toHaveBeenCalledTimes(1), BOOT_WAIT);
    // Hidden while the chunk loads; the failure brings the previous page back.
    expect(outlet.hidden).toBe(true);
    // A browser blurs an element whose ancestor becomes hidden; jsdom does
    // not, so do it here.
    focused.blur();
    expect(document.activeElement).toBe(document.body);
    rejectLoad!(new Error('Failed to fetch dynamically imported module'));
    await vi.waitFor(() => expect(showToast).toHaveBeenCalledTimes(1), BOOT_WAIT);
    expect(showToast).toHaveBeenCalledWith(
      'Could not load terminals. Reload the page to try again.',
      'danger'
    );
    expect(errorSpy).toHaveBeenCalledWith(
      '[Scion] Failed to load the terminal workspace:',
      expect.any(Error)
    );
    expect(loadTerminalWorkspace).toHaveBeenCalledTimes(1);
    expect(fakeTerminalRecord.workspaces).toHaveLength(0);
    expect(fakeTerminalRecord.coordinators).toHaveLength(0);
    expect(outlet.hidden).toBe(false);
    expect(document.querySelector('scion-page-admin-users')).not.toBeNull();
    // Focus goes back to where it was before the outlet was hidden.
    expect(deepActiveElement()).toBe(focused);

    // The failure was not cached: the next visit loads again and mounts.
    main.navigateTo('/terminals');
    await vi.waitFor(() => expect(fakeTerminalRecord.workspaces).toHaveLength(1), BOOT_WAIT);
    expect(loadTerminalWorkspace).toHaveBeenCalledTimes(2);
    expect(fakeTerminalRecord.workspaces[0]!.element.isConnected).toBe(true);
    await vi.waitFor(() => expect(outlet.hidden).toBe(true), BOOT_WAIT);
    expect(showToast).toHaveBeenCalledTimes(1);
    errorSpy.mockRestore();
  }, 15_000);
});
