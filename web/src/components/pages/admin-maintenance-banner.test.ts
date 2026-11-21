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

import { describe, expect, it } from 'vitest';
import {
  ALL_BANNER_ROWS,
  bannerRowMatches,
  classifyBannerRow,
  runBanner,
  type BannerRowId,
  type BannerState,
  type MaintenanceRunLike,
} from './admin-maintenance-banner.js';

/**
 * Reachability invariants from design-delta.r5.md section 5 (R4-2).
 */
function reachable(s: BannerState): boolean {
  const { status, R, O, OA, FR, FN, AB, IN, BR } = s;
  const completed = status === 'completed';
  const failed = status === 'failed';

  if (OA && !O) return false;
  if (O && !R) return false;
  if (FR && !(R && !O && failed)) return false;
  if (FN && !(R && !O && !FR)) return false;
  if (AB && !failed) return false;
  if (R && BR && !O) return false;
  if (IN && O && !OA) return false;
  if (IN && FR) return false;
  if (R && completed && !(O || FN)) return false;
  if (O && !OA && completed) return false;
  return true;
}

describe('admin-maintenance-banner state table (768 combinations)', () => {
  it('is exhaustive, mutually exclusive, and reaches all 19 rows over 54 reachable states', () => {
    const statuses: BannerState['status'][] = ['running', 'completed', 'failed'];
    const bools = [false, true];
    let combinations = 0;
    let reachableCount = 0;
    const hitRows = new Set<BannerRowId>();

    for (const status of statuses) {
      for (const R of bools) {
        for (const O of bools) {
          for (const OA of bools) {
            for (const FR of bools) {
              for (const FN of bools) {
                for (const AB of bools) {
                  for (const IN of bools) {
                    for (const BR of bools) {
                      combinations++;
                      const state: BannerState = { status, R, O, OA, FR, FN, AB, IN, BR };
                      if (!reachable(state)) continue;
                      reachableCount++;

                      const matching = ALL_BANNER_ROWS.filter((row) =>
                        bannerRowMatches(row, state)
                      );
                      expect(matching, `state=${JSON.stringify(state)}`).toHaveLength(1);
                      const row = matching[0];
                      expect(classifyBannerRow(state)).toBe(row);
                      hitRows.add(row);
                    }
                  }
                }
              }
            }
          }
        }
      }
    }

    expect(combinations).toBe(768);
    expect(reachableCount).toBe(54);
    expect([...hitRows].sort()).toEqual([...ALL_BANNER_ROWS].sort());
  });

  const sampleOverride = {
    actorId: 'user-42',
    actorEmail: 'admin@example.com',
    at: '2026-11-21T12:00:00Z',
    rerunOf: 'run-failed-1',
    commit: '0123456789abcdef0123456789abcdef01234567',
    branch: 'main',
    findings: [{ code: 'settings.profiles.missing', message: 'missing', agents: 2 }],
    failedRunFindings: 1,
  };

  function makeRun(
    id: string,
    status: string,
    result?: Record<string, unknown>
  ): MaintenanceRunLike {
    return {
      id,
      operationKey: 'rebuild-server',
      status,
      result: result ? JSON.stringify(result) : undefined,
    };
  }

  function makeFailedWithBackRef(forcedRunId: string): MaintenanceRunLike {
    return makeRun('run-failed-1', 'failed', {
      forcedBy: { ...sampleOverride, runId: forcedRunId },
    });
  }

  it('covers all 19 named rows and maps restarting to in-progress (Option A)', () => {
    const cases: Array<{
      row: BannerRowId;
      run: MaintenanceRunLike;
      loaded?: MaintenanceRunLike[];
      expectText: string | null;
    }> = [
      {
        row: 'F1',
        run: makeRun('r2', 'running', { rerunOf: 'run-failed-1' }),
        expectText: 'Force re-run in progress',
      },
      {
        row: 'F2',
        run: makeRun('r2', 'failed', { error: 'aborted' }),
        loaded: [makeFailedWithBackRef('r2')],
        expectText:
          "Force applied (recorded on run run-failed-1); this run's own record was replaced",
      },
      {
        row: 'F3',
        run: makeRun('r2', 'completed', {
          rerunOf: 'run-failed-1',
          override: sampleOverride,
          overrideApplied: true,
        }),
        expectText: 'Preflight overridden by force by admin@example.com',
      },
      {
        row: 'F4',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          override: sampleOverride,
          overrideApplied: true,
          installed: true,
          aborted: true,
        }),
        expectText:
          'Force applied and installed; the server restarted before this run recorded its end',
      },
      {
        row: 'F5',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          override: sampleOverride,
          overrideApplied: true,
          installed: true,
          aborted: false,
        }),
        expectText:
          'Force applied and installed; the restart could not be started. Restart the service',
      },
      {
        row: 'F6',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          override: sampleOverride,
          overrideApplied: true,
          installed: false,
          aborted: true,
        }),
        expectText: 'Force applied; the hub stopped before the install was confirmed',
      },
      {
        row: 'F7',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          override: sampleOverride,
          overrideApplied: true,
          installed: false,
          error: 'stage failed',
        }),
        expectText: 'Force applied; the install was not confirmed: stage failed',
      },
      {
        row: 'F8',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          override: sampleOverride,
          overrideApplied: false,
          aborted: true,
        }),
        expectText: 'Force not applied: the hub stopped before the override was recorded',
      },
      {
        row: 'F8b',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          override: sampleOverride,
          overrideApplied: false,
          error: 'write 1b failed',
        }),
        loaded: [makeFailedWithBackRef('r2')],
        expectText: 'Override recorded on run run-failed-1; nothing was installed: write 1b failed',
      },
      {
        row: 'F9',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          override: sampleOverride,
          overrideApplied: false,
          error: 'write 2 failed',
        }),
        expectText: 'Force not applied: write 2 failed',
      },
      {
        row: 'F10',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          forceRejected: 'findings changed',
        }),
        expectText: 'Force rejected: the findings changed since run run-failed-1',
      },
      {
        row: 'F11',
        run: makeRun('r2', 'completed', {
          rerunOf: 'run-failed-1',
          forceNotNeeded: true,
        }),
        expectText: 'Force not needed: the re-run found no blocking findings',
      },
      {
        row: 'F12a',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          forceNotNeeded: true,
          installed: true,
          aborted: true,
        }),
        expectText:
          'Force not needed; installed, and the server restarted before this run recorded its end',
      },
      {
        row: 'F12b',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          forceNotNeeded: true,
          installed: true,
          aborted: false,
        }),
        expectText: 'Force not needed; installed, but the restart could not be started',
      },
      {
        row: 'F12c',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          forceNotNeeded: true,
          installed: false,
          error: 'snapshot failed',
        }),
        expectText: 'Force not needed (no blocking findings); the run then failed: snapshot failed',
      },
      {
        row: 'F13',
        run: makeRun('r2', 'failed', {
          rerunOf: 'run-failed-1',
          aborted: true,
          error: 'aborted on restart',
        }),
        expectText:
          'Force not applied: the run ended before the preflight decision: aborted on restart',
      },
      {
        row: 'U1',
        run: makeRun('r2', 'failed', { installed: true, error: 'restart failed' }),
        expectText: 'Installed; the restart could not be started. Restart the service',
      },
      {
        row: 'U2',
        run: makeRun('r2', 'failed', { installed: true, aborted: true }),
        expectText: 'Installed; the server restarted before this run recorded its end',
      },
      {
        row: 'U0',
        run: makeRun('r2', 'completed', { branch: 'main' }),
        expectText: null,
      },
    ];

    for (const tc of cases) {
      const b = runBanner(tc.run, tc.loaded ?? []);
      if (tc.expectText === null) {
        expect(b, tc.row).toBeNull();
      } else {
        expect(b, tc.row).not.toBeNull();
        expect(b?.row, tc.row).toBe(tc.row);
        expect(b?.text, tc.row).toContain(tc.expectText);
      }
    }

    // Option A: "restarting" maps to the same in-progress banner as "running"
    // (F1 for forced, U0/null for unforced).
    const forcedRestarting = runBanner(
      makeRun('r2', 'restarting', {
        rerunOf: 'run-failed-1',
        override: sampleOverride,
        overrideApplied: true,
        installed: true,
      }),
      []
    );
    expect(forcedRestarting?.row).toBe('F1');

    const unforcedRestarting = runBanner(
      makeRun('r2', 'restarting', { installed: true, branch: 'main' }),
      []
    );
    expect(unforcedRestarting).toBeNull();

    // R1/R2: reconciled failure paths (rolled_back, version mismatch, stale restarting)
    // set aborted: true alongside installed: true, so they land on U2/F4/F12a rather than
    // U1/F5/F12b ("the restart could not be started"), and branch on verification so
    // rolled-back/mismatched updates never render a green "Installed" success banner.
    const unforcedRollback = runBanner(
      makeRun('r2', 'failed', {
        installed: true,
        aborted: true,
        verification: 'rolled_back',
        error: '"git-0123456789ab" failed to start; the boot-failure guard rolled back',
      }),
      []
    );
    expect(unforcedRollback?.row).toBe('U2');
    expect(unforcedRollback?.variant).toBe('warning');
    expect(unforcedRollback?.text).toContain('Installed, but the new binary did not stay up:');
    expect(unforcedRollback?.text).not.toContain('could not be started');

    const forcedRollback = runBanner(
      makeRun('r2', 'failed', {
        rerunOf: 'run-failed-1',
        override: sampleOverride,
        overrideApplied: true,
        installed: true,
        aborted: true,
        verification: 'rolled_back',
        error: 'boot-failure guard rolled back',
      }),
      []
    );
    expect(forcedRollback?.row).toBe('F4');
    expect(forcedRollback?.variant).toBe('danger');
    expect(forcedRollback?.text).toContain(
      'Force applied and installed, but the new binary did not stay up: boot-failure guard rolled back'
    );

    const qdMismatch = runBanner(
      makeRun('r2', 'failed', {
        rerunOf: 'run-failed-1',
        forceNotNeeded: true,
        installed: true,
        aborted: true,
        verification: 'failed',
        error: 'the hub restarted on "v0.1.0", not the installed "git-0123456789ab"',
      }),
      []
    );
    expect(qdMismatch?.row).toBe('F12a');
    expect(qdMismatch?.variant).toBe('warning');
    expect(qdMismatch?.text).toContain(
      'Force not needed; installed, but the new binary did not stay up:'
    );

    // Unforced U2 without verification is also warning (never success).
    const unforcedAborted = runBanner(
      makeRun('r2', 'failed', { installed: true, aborted: true }),
      []
    );
    expect(unforcedAborted?.row).toBe('U2');
    expect(unforcedAborted?.variant).toBe('warning');
  });
});
