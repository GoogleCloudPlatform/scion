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

import { describe, expect, it, vi, type Mock } from 'vitest';
import type { TerminalOpenResult } from './terminal-coordinator.js';
import {
  PALETTE_OPEN_ATTEMPTS,
  TERMINALS_OPEN_ELSEWHERE_HELP,
  TERMINALS_OPEN_ELSEWHERE_HELP_LABEL,
  TERMINALS_OPEN_ELSEWHERE_STATUS,
  nonOwnerOpenStatus,
  offersMove,
  openPalettePickedAgent,
  type PaletteOpenCoordinator,
  type PaletteOpenOptions,
} from './terminal-palette-open.js';

const AGENT = '11111111-1111-4111-8111-111111111111';
const REQUEST = 'request-1';
const NAVIGATION = 7;

function result(status: TerminalOpenResult['status']): TerminalOpenResult {
  return { status, requestId: REQUEST, agentId: AGENT, generation: null, focus: 'not-confirmed' };
}

/** A coordinator whose open() resolves `pending` `pendingCount` times, then `last` (or rejects with it). */
function stubCoordinator(
  pendingCount: number,
  last: TerminalOpenResult['status'] | Error,
  { supported = true, isOwner = true } = {}
): Omit<PaletteOpenCoordinator, 'open'> & { open: Mock<PaletteOpenCoordinator['open']> } {
  let calls = 0;
  return {
    supported,
    isOwner,
    open: vi.fn<PaletteOpenCoordinator['open']>(() => {
      calls++;
      if (calls <= pendingCount) return Promise.resolve(result('pending'));
      if (last instanceof Error) return Promise.reject(last);
      return Promise.resolve(result(last));
    }),
  };
}

function setup(
  coordinator: PaletteOpenCoordinator,
  overrides: Partial<PaletteOpenOptions> = {}
): {
  navigations: Map<string, number>;
  workspace: { cancelPalettePlacement: Mock<(agentId: string) => void> };
  notify: Mock<(message: string) => void>;
  options: PaletteOpenOptions;
} {
  const navigations = new Map<string, number>();
  const workspace = { cancelPalettePlacement: vi.fn<(agentId: string) => void>() };
  const notify = vi.fn<(message: string) => void>();
  const options: PaletteOpenOptions = {
    coordinator,
    workspace,
    agentId: AGENT,
    navigations,
    navigationId: NAVIGATION,
    currentNavigationId: () => NAVIGATION,
    notify,
    createRequestId: () => REQUEST,
    ...overrides,
  };
  return { navigations, workspace, notify, options };
}

describe('openPalettePickedAgent', () => {
  it('registers the request for the current navigation while the open runs', async () => {
    let registered: number | undefined;
    const coordinator = stubCoordinator(0, 'selected');
    const { navigations, options } = setup(coordinator);
    coordinator.open.mockImplementation(() => {
      registered = navigations.get(REQUEST);
      return Promise.resolve(result('selected'));
    });

    await openPalettePickedAgent(options);

    expect(registered).toBe(NAVIGATION);
    expect(coordinator.open).toHaveBeenCalledWith(AGENT, REQUEST);
  });

  it('retries a pending open with the same request ID until it settles', async () => {
    const coordinator = stubCoordinator(3, 'selected');
    const { navigations, workspace, options } = setup(coordinator);

    await openPalettePickedAgent(options);

    expect(coordinator.open).toHaveBeenCalledTimes(4);
    for (const call of coordinator.open.mock.calls) expect(call).toEqual([AGENT, REQUEST]);
    expect(navigations.size).toBe(0);
    expect(workspace.cancelPalettePlacement).toHaveBeenCalledTimes(1);
    expect(workspace.cancelPalettePlacement).toHaveBeenCalledWith(AGENT);
  });

  it(`gives up after ${PALETTE_OPEN_ATTEMPTS} pending attempts and clears the registration and the placement hint`, async () => {
    const coordinator = stubCoordinator(Infinity, 'selected');
    const { navigations, workspace, options } = setup(coordinator);

    await openPalettePickedAgent(options);

    expect(coordinator.open).toHaveBeenCalledTimes(PALETTE_OPEN_ATTEMPTS);
    expect(navigations.size).toBe(0);
    expect(workspace.cancelPalettePlacement).toHaveBeenCalledTimes(1);
    expect(workspace.cancelPalettePlacement).toHaveBeenCalledWith(AGENT);
  });

  it('clears the registration and the placement hint when the open throws', async () => {
    const coordinator = stubCoordinator(1, new Error('Terminal requires an agent UUID.'));
    const { navigations, workspace, options } = setup(coordinator);

    await expect(openPalettePickedAgent(options)).resolves.toBeUndefined();

    expect(coordinator.open).toHaveBeenCalledTimes(2);
    expect(navigations.size).toBe(0);
    expect(workspace.cancelPalettePlacement).toHaveBeenCalledTimes(1);
    expect(workspace.cancelPalettePlacement).toHaveBeenCalledWith(AGENT);
  });

  it('registers nothing when the coordinator is unsupported, and still clears the hint', async () => {
    const coordinator = stubCoordinator(0, 'unsupported', { supported: false });
    const { navigations, workspace, options } = setup(coordinator);
    const set = vi.spyOn(navigations, 'set');

    await openPalettePickedAgent(options);

    expect(set).not.toHaveBeenCalled();
    expect(coordinator.open).toHaveBeenCalledWith(AGENT, undefined);
    expect(workspace.cancelPalettePlacement).toHaveBeenCalledTimes(1);
  });

  it('tells nothing to the user in the tab that owns the sessions', async () => {
    const coordinator = stubCoordinator(2, 'selected', { isOwner: true });
    const { notify, options } = setup(coordinator);

    await openPalettePickedAgent(options);

    expect(notify).not.toHaveBeenCalled();
  });

  it('in a tab that does not own the sessions, reports waiting once and then the outcome', async () => {
    const coordinator = stubCoordinator(3, 'selected', { isOwner: false });
    const { notify, options } = setup(coordinator);

    await openPalettePickedAgent(options);

    expect(notify.mock.calls).toEqual([
      ['Waiting for the owning tab to select this terminal.'],
      ['Terminals open in another window'],
    ]);
  });

  it('in a tab that does not own the sessions, reports an open that cannot run', async () => {
    const coordinator = stubCoordinator(0, 'stopped', { isOwner: false });
    const { notify, options } = setup(coordinator);

    await openPalettePickedAgent(options);

    expect(notify.mock.calls).toEqual([['Terminal workspace is unavailable in this tab.']]);
  });

  it('reports nothing once the user has navigated elsewhere', async () => {
    const coordinator = stubCoordinator(1, 'selected', { isOwner: false });
    const { notify, options } = setup(coordinator, { currentNavigationId: () => NAVIGATION + 1 });

    await openPalettePickedAgent(options);

    expect(notify).not.toHaveBeenCalled();
  });
});

describe('move terminals to this window (ptone/scion#3328)', () => {
  it('offers the move only once the owning tab has the terminal selected', () => {
    expect(offersMove('selected')).toBe(true);
    for (const status of [
      'pending',
      'unsupported',
      'stopped',
      'request-id-conflict',
      'selection-failed',
    ] as const)
      expect(offersMove(status)).toBe(false);
  });
});

describe('terminals open in another window: one state text (ptone/scion#4324)', () => {
  it('uses one state phrase, also for an open the owning window selected', () => {
    expect(TERMINALS_OPEN_ELSEWHERE_STATUS).toBe('Terminals open in another window');
    // The status a non-owner open shows, with the move button, and the
    // palette's message in a non-owner window.
    expect(nonOwnerOpenStatus('selected')).toBe(TERMINALS_OPEN_ELSEWHERE_STATUS);
  });

  it('explains in the help that a connection exists in one place and what the button does', () => {
    expect(TERMINALS_OPEN_ELSEWHERE_HELP_LABEL).toBe('About terminals open in another window');
    expect(TERMINALS_OPEN_ELSEWHERE_HELP).toMatch(/only one place at a time/);
    expect(TERMINALS_OPEN_ELSEWHERE_HELP).toMatch(/Move terminals to this window/);
  });
});
