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

import type { ToastVariant } from './toast.js';

/** The stop-all endpoint response fields the UI reads. */
export interface StopAllResult {
  stopped: number;
  failed: number;
  /** Agents whose start was in flight: the stop is recorded, the start is not interrupted. */
  stopRecorded?: number;
  scope?: string;
}

export interface StopAllNotice {
  message: string;
  variant: ToastVariant;
}

/** The toasts to show after a stop-all request, in display order. */
export function stopAllNotices(result: StopAllResult): StopAllNotice[] {
  const notices: StopAllNotice[] = [];
  if (result.failed > 0) {
    notices.push({
      message: `Stopped ${result.stopped} agents, ${result.failed} failed.`,
      variant: 'warning',
    });
  }
  const recorded = result.stopRecorded ?? 0;
  if (recorded > 0) {
    const agents = recorded === 1 ? '1 agent' : `${recorded} agents`;
    notices.push({
      message: `Stop queued for ${agents} still starting. The start was not interrupted; stop again if it comes up running.`,
      variant: 'neutral',
    });
  }
  return notices;
}
