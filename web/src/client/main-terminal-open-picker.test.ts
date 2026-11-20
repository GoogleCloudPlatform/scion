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
 * Dashboard "Open Terminal" quick action navigates to /terminals and asks the
 * terminal workspace to auto-open the "Jump to agent" picker only after saved
 * workspace restoration settles with zero open terminals, whereas non-zero
 * restored/open sessions, bare /terminals navigations (such as the header mode
 * switch), and cross-window "terminals open elsewhere" states do not
 * (ptone/scion#8468).
 */

import { afterAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt, json } from './__fixtures__/main-boot.js';
import {
  FakeTerminalWorkspacePersistence,
  fakeTerminalRecord,
  fakeTerminalWorkspaceModule,
} from './__fixtures__/terminal-workspace-fake.js';
import type { TerminalWorkspaceModule } from './terminal-workspace-loader.js';

let elsewhereOverride = false;
let restoreHook: (() => Promise<void>) | null = null;

class ControlledPersistence extends FakeTerminalWorkspacePersistence {
  override async restore(): Promise<void> {
    if (restoreHook) await restoreHook();
  }
}

const loadTerminalWorkspace = vi.fn(() =>
  Promise.resolve({
    ...fakeTerminalWorkspaceModule,
    TerminalWorkspacePersistence: ControlledPersistence,
    terminalsOpenElsewhere: () => Promise.resolve(elsewhereOverride),
  } as unknown as TerminalWorkspaceModule)
);
vi.mock('./terminal-workspace-loader.js', () => ({
  loadTerminalWorkspace: (): Promise<TerminalWorkspaceModule> => loadTerminalWorkspace(),
}));

function findOpenTerminalCard(): HTMLAnchorElement | null {
  const home = document.querySelector('scion-page-home');
  const cards = Array.from(
    home?.shadowRoot?.querySelectorAll<HTMLAnchorElement>('a.action-card') ?? []
  );
  return (
    cards.find((card) => card.querySelector('h4')?.textContent?.trim() === 'Open Terminal') ?? null
  );
}

describe('main.ts dashboard Open Terminal quick action (ptone/scion#8468)', () => {
  beforeEach(() => {
    elsewhereOverride = false;
    restoreHook = null;
  });

  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('auto-opens picker after empty restore, skips when sessions exist or open elsewhere, and leaves plain /terminals unchanged', async () => {
    const main = await bootMainAt('/', (pathname) => {
      if (pathname === '/api/v1/projects') return Promise.resolve(json({ projects: [] }));
      if (pathname === '/api/v1/agents')
        return Promise.resolve(json({ agents: [], complete: true }));
      return undefined;
    });

    await vi.waitFor(() => expect(findOpenTerminalCard()).not.toBeNull(), BOOT_WAIT);
    const card = findOpenTerminalCard()!;
    expect(card.getAttribute('href')).toBe('/terminals');

    // Hold restore() open so we prove openPaletteIfEmpty waits until after
    // saved workspace restoration finishes.
    let finishRestore!: () => void;
    restoreHook = (): Promise<void> =>
      new Promise<void>((resolve) => {
        finishRestore = resolve;
      });

    card.click();

    await vi.waitFor(() => expect(fakeTerminalRecord.workspaces).toHaveLength(1), BOOT_WAIT);
    const workspace = fakeTerminalRecord.workspaces[0]!;
    expect(window.location.pathname).toBe('/terminals');
    expect(workspace.openPaletteIfEmptyCalls).toBe(0);

    // Finish restore -> main.ts invokes openPaletteIfEmpty (the empty/non-empty
    // session guard itself is unit-tested in terminal-workspace-root.test.ts).
    finishRestore();
    await vi.waitFor(() => expect(workspace.openPaletteIfEmptyCalls).toBe(1), BOOT_WAIT);

    // Plain navigateTo('/terminals') (as the header Terminals mode button does)
    // does NOT invoke openPaletteIfEmpty.
    restoreHook = null;
    main.navigateTo('/');
    await vi.waitFor(() => expect(window.location.pathname).toBe('/'), BOOT_WAIT);
    main.navigateTo('/terminals');
    await vi.waitFor(() => expect(window.location.pathname).toBe('/terminals'), BOOT_WAIT);
    await new Promise((r) => setTimeout(r, 20));
    expect(workspace.openPaletteIfEmptyCalls).toBe(1);

    // When terminals are open in another window, clicking Dashboard Open Terminal
    // shows the move state instead of calling openPaletteIfEmpty.
    elsewhereOverride = true;
    main.navigateTo('/');
    await vi.waitFor(() => expect(findOpenTerminalCard()).not.toBeNull(), BOOT_WAIT);
    findOpenTerminalCard()!.click();
    await vi.waitFor(() => expect(window.location.pathname).toBe('/terminals'), BOOT_WAIT);
    await new Promise((r) => setTimeout(r, 20));
    expect(workspace.openPaletteIfEmptyCalls).toBe(1);
  }, 20_000);
});
