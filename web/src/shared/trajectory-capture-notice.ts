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

import type { TrajectoryCapturePolicyStatus } from './types.js';

/**
 * Canonical notice version matching telemetrycontract.TrajectoryNoticeVersion.
 * Verified against pkg/telemetrycontract/testdata/trajectory_capture_notices.json.
 */
export const TRAJECTORY_NOTICE_VERSION = 'v1';

/**
 * Reports whether policy results in trajectory data or metadata being captured
 * ("full" or "metadata_only"). Returns false for "off" and unknown/empty values.
 */
export function isTrajectoryCaptureEffective(policy: string | undefined): boolean {
  return policy === 'full' || policy === 'metadata_only';
}

/**
 * Returns the canonical user-visible trajectory capture notice line matching
 * telemetrycontract.TrajectoryCaptureNotice in Go. Returns "" when status is
 * absent or policy is unrecognized.
 */
export function formatTrajectoryCaptureNotice(
  status: TrajectoryCapturePolicyStatus | undefined
): string {
  if (!status) return '';
  switch (status.policy) {
    case 'full':
      return status.enforced_by_hub
        ? 'Session transcripts for this agent are captured verbatim to the Hub audit trail (policy: full, enforced by Hub). Access is restricted and logged.'
        : 'Session transcripts for this agent are captured verbatim to the Hub audit trail (policy: full). Access is restricted and logged.';
    case 'metadata_only':
      return status.enforced_by_hub
        ? 'Session metadata for this agent is recorded to the Hub audit trail without raw transcripts (policy: metadata_only, enforced by Hub).'
        : 'Session metadata for this agent is recorded to the Hub audit trail without raw transcripts (policy: metadata_only).';
    case 'off':
      return status.enforced_by_hub
        ? 'Trajectory capture is off for this agent (policy: off, enforced by Hub).'
        : 'Trajectory capture is off for this agent (policy: off).';
    default:
      return '';
  }
}
