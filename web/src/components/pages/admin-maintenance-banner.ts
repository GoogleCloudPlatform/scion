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

export type PreflightSeverity = 'block' | 'warn' | 'info';

export interface PreflightFinding {
  check: string;
  code: string;
  severity: PreflightSeverity;
  message: string;
  remediation?: string;
  subjects?: string[];
  subjectCount?: number;
  data?: Record<string, unknown>;
}

export interface PreflightReport {
  version: number;
  checks: string[];
  findings: PreflightFinding[];
  blocking: number;
  warnings: number;
}

export interface CouldNotRunDetail {
  kind: string;
  class: string;
  reason: string;
}

export interface OverrideFinding {
  code: string;
  message: string;
  subjectCount?: number;
  data?: Record<string, unknown>;
  agents?: number;
  failedRunAgents?: number;
}

export interface OverrideRecord {
  actorId: string;
  actorEmail: string;
  at: string;
  rerunOf: string;
  commit: string;
  branch: string;
  couldNotRun?: CouldNotRunDetail;
  findings?: OverrideFinding[];
  failedRunFindings: number;
}

export interface ForcedByRecord extends OverrideRecord {
  runId: string;
}

export interface RebuildResult {
  log?: string;
  error?: string;
  resultError?: string;
  dryRun?: boolean;
  branch?: string;
  detached?: boolean;
  branchPinnable?: boolean;
  installed?: boolean;
  forceRejected?: string;
  forceNotNeeded?: boolean;
  commit?: string;
  preflight?: PreflightReport;
  couldNotRun?: CouldNotRunDetail;
  forced?: boolean;
  overrideApplied?: boolean;
  rerunOf?: string;
  override?: OverrideRecord;
  forcedBy?: ForcedByRecord;
  aborted?: boolean;
}

export interface MaintenanceRunLike {
  id: string;
  operationKey?: string;
  status: string;
  result?: string;
}

export type BannerRowId =
  | 'F1'
  | 'F2'
  | 'F3'
  | 'F4'
  | 'F5'
  | 'F6'
  | 'F7'
  | 'F8'
  | 'F8b'
  | 'F9'
  | 'F10'
  | 'F11'
  | 'F12a'
  | 'F12b'
  | 'F12c'
  | 'F13'
  | 'U1'
  | 'U2'
  | 'U0';

export type BannerVariant = 'primary' | 'success' | 'warning' | 'danger' | 'neutral';

export interface BannerState {
  status: 'running' | 'completed' | 'failed';
  R: boolean;
  O: boolean;
  OA: boolean;
  FR: boolean;
  FN: boolean;
  AB: boolean;
  IN: boolean;
  BR: boolean;
}

export interface RunBanner {
  row: BannerRowId;
  variant: BannerVariant;
  text: string;
  /** Optional linked failed/target run ID (for F2, F8b, F10). */
  linkedRunId?: string | undefined;
  /** True when linkedRunId is present in loadedRuns (renders as a clickable link). */
  linkedRunLoaded?: boolean | undefined;
}

export const ALL_BANNER_ROWS: readonly BannerRowId[] = [
  'F1',
  'F2',
  'F3',
  'F4',
  'F5',
  'F6',
  'F7',
  'F8',
  'F8b',
  'F9',
  'F10',
  'F11',
  'F12a',
  'F12b',
  'F12c',
  'F13',
  'U1',
  'U2',
  'U0',
];

export function parseRebuildResult(resultStr: string | undefined): RebuildResult | null {
  if (!resultStr) return null;
  try {
    const parsed: unknown = JSON.parse(resultStr);
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed;
    }
    return null;
  } catch {
    return null;
  }
}

/**
 * Normalizes a maintenance run's status to the 3-status banner domain.
 * "restarting" maps to "running" (Option A: keeps the verified 19-row /
 * 768-state banner table intact).
 */
export function normalizeBannerStatus(status: string): 'running' | 'completed' | 'failed' {
  if (status === 'running' || status === 'restarting') return 'running';
  if (status === 'completed') return 'completed';
  return 'failed';
}

/**
 * Pure row predicate table (design-delta.r5.md section 5). Over all reachable
 * states, predicates are mutually exclusive and exhaustive (0 overlaps, 0 gaps).
 */
export function bannerRowMatches(row: BannerRowId, s: BannerState): boolean {
  const running = s.status === 'running';
  const completed = s.status === 'completed';
  const failed = s.status === 'failed';
  const { R, O, OA, FR, FN, AB, IN, BR } = s;

  switch (row) {
    case 'F1':
      return running && (R || BR);
    case 'F2':
      return !running && BR && !O;
    case 'F3':
      return O && OA && completed;
    case 'F4':
      return O && OA && failed && IN && AB;
    case 'F5':
      return O && OA && failed && IN && !AB;
    case 'F6':
      return O && OA && failed && !IN && AB;
    case 'F7':
      return O && OA && failed && !IN && !AB;
    case 'F8':
      return !running && O && !OA && AB && !BR;
    case 'F8b':
      return !running && O && !OA && BR;
    case 'F9':
      return !running && O && !OA && !AB && !BR;
    case 'F10':
      return !running && R && FR;
    case 'F11':
      return !running && R && FN && completed;
    case 'F12a':
      return R && FN && failed && IN && AB;
    case 'F12b':
      return R && FN && failed && IN && !AB;
    case 'F12c':
      return R && FN && failed && !IN;
    case 'F13':
      return !running && R && !O && !FR && !FN;
    case 'U1':
      return !R && !BR && failed && IN && !AB;
    case 'U2':
      return !R && !BR && failed && IN && AB;
    case 'U0':
      return !R && !BR && !(failed && IN);
  }
}

export function classifyBannerRow(s: BannerState): BannerRowId {
  for (const row of ALL_BANNER_ROWS) {
    if (bannerRowMatches(row, s)) {
      return row;
    }
  }
  return 'U0';
}

/**
 * Derives the run banner for a maintenance run from its stored Result and
 * the currently loaded history runs (for BR back-references).
 * Returns null for row U0 (no banner).
 */
export function runBanner(
  run: MaintenanceRunLike,
  loadedRuns: readonly MaintenanceRunLike[]
): RunBanner | null {
  const result = parseRebuildResult(run.result);
  const status = normalizeBannerStatus(run.status);

  // Back-reference: some loaded run's forcedBy.runId equals this run's id.
  let backRefRunId: string | undefined;
  for (const candidate of loadedRuns) {
    const cr = parseRebuildResult(candidate.result);
    if (cr?.forcedBy?.runId === run.id) {
      backRefRunId = candidate.id;
      break;
    }
  }

  const state: BannerState = {
    status,
    R: Boolean(result?.rerunOf),
    O: Boolean(result?.override),
    OA: Boolean(result?.overrideApplied),
    FR: Boolean(result?.forceRejected),
    FN: Boolean(result?.forceNotNeeded),
    AB: Boolean(result?.aborted),
    IN: Boolean(result?.installed),
    BR: Boolean(backRefRunId),
  };

  const row = classifyBannerRow(state);
  if (row === 'U0') {
    return null;
  }

  const errText = result?.error || 'no error recorded';
  const ov = result?.override;
  const rerunOf = result?.rerunOf;
  const rerunOfLoaded = Boolean(rerunOf && loadedRuns.some((r) => r.id === rerunOf));

  switch (row) {
    case 'F1':
      return { row, variant: 'primary', text: 'Force re-run in progress' };
    case 'F2':
      return {
        row,
        variant: 'warning',
        // NT5-f: drop "at restart" because RunResult-panic can also replace it.
        text: `Force applied (recorded on run ${backRefRunId}); this run's own record was replaced`,
        linkedRunId: backRefRunId,
        linkedRunLoaded: true,
      };
    case 'F3': {
      const blocking = ov?.couldNotRun
        ? `could not run: ${ov.couldNotRun.kind}`
        : `${ov?.findings?.length ?? result?.preflight?.blocking ?? 0} blocking`;
      return {
        row,
        variant: 'warning',
        text: `Preflight overridden by force by ${ov?.actorEmail ?? 'unknown'} at ${ov?.at ?? ''} (${blocking})`,
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    }
    case 'F4':
      return {
        row,
        variant: 'warning',
        text: 'Force applied and installed; the server restarted before this run recorded its end',
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F5':
      return {
        row,
        variant: 'warning',
        text: 'Force applied and installed; the restart could not be started. Restart the service',
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F6':
      return {
        row,
        variant: 'warning',
        text: 'Force applied; the hub stopped before the install was confirmed. Check the running version',
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F7':
      return {
        row,
        variant: 'danger',
        text: `Force applied; the install was not confirmed: ${errText}`,
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F8':
      return {
        row,
        variant: 'danger',
        text: 'Force not applied: the hub stopped before the override was recorded',
      };
    case 'F8b':
      return {
        row,
        variant: 'warning',
        text: `Override recorded on run ${backRefRunId}; nothing was installed: ${result?.error || 'the hub stopped'}`,
        linkedRunId: backRefRunId,
        linkedRunLoaded: true,
      };
    case 'F9':
      return {
        row,
        variant: 'danger',
        text: `Force not applied: ${errText}`,
      };
    case 'F10':
      return {
        row,
        variant: 'danger',
        text: `Force rejected: the findings changed since run ${rerunOf}; review them and re-run`,
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F11':
      return {
        row,
        variant: 'success',
        text: 'Force not needed: the re-run found no blocking findings',
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F12a':
      return {
        row,
        variant: 'success',
        text: 'Force not needed; installed, and the server restarted before this run recorded its end',
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F12b':
      return {
        row,
        variant: 'warning',
        text: 'Force not needed; installed, but the restart could not be started. Restart the service',
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F12c':
      return {
        row,
        variant: 'danger',
        text: `Force not needed (no blocking findings); the run then failed: ${errText}`,
        linkedRunId: rerunOf,
        linkedRunLoaded: rerunOfLoaded,
      };
    case 'F13':
      return {
        row,
        variant: 'danger',
        text: `Force not applied: the run ended before the preflight decision: ${errText}`,
      };
    case 'U1':
      return {
        row,
        variant: 'warning',
        text: 'Installed; the restart could not be started. Restart the service',
      };
    case 'U2':
      return {
        row,
        variant: 'success',
        text: 'Installed; the server restarted before this run recorded its end',
      };
  }
}

/**
 * Reports whether the "Re-run with force" action is eligible for run
 * (design-delta.r5.md section 5, CN-f, H2).
 *
 * latestRunId should be loadedHistory[0]?.id, falling back to
 * operation.lastRun?.id while history is loading (H2).
 */
export function canForceRerun(
  run: MaintenanceRunLike,
  latestRunId: string | undefined
): { allowed: boolean; unpinnableReason?: string } {
  if (run.operationKey !== 'rebuild-server' || run.status !== 'failed') {
    return { allowed: false };
  }
  const result = parseRebuildResult(run.result);
  if (!result) return { allowed: false };

  const hasBlocks = (result.preflight?.blocking ?? 0) > 0;
  const hasCouldNotRun = Boolean(result.couldNotRun?.kind);
  if (!hasBlocks && !hasCouldNotRun) {
    return { allowed: false };
  }
  if (result.forcedBy || result.forced === true) {
    return { allowed: false };
  }
  if (!latestRunId || run.id !== latestRunId) {
    return { allowed: false };
  }
  if (result.branchPinnable === false) {
    return {
      allowed: false,
      unpinnableReason: "this run's branch name cannot be pinned; start a fresh run",
    };
  }
  return { allowed: true };
}
