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
 * Status tones shared by the health dashboard sections (ptone/scion#3595).
 *
 * A tone maps a status word to one of the theme's badge token pairs
 * (--scion-badge-*-bg / --scion-badge-*-text). Those pairs pass WCAG AA in
 * the light and dark themes (see health-status.test.ts), so a pill that
 * uses healthPillStyles never needs a hardcoded colour.
 */

import { css } from 'lit';

export type HealthTone = 'ok' | 'warn' | 'bad' | 'neutral';

/**
 * The tone of a status value. Hub check values can carry a fixed reason
 * after a colon ("unhealthy: mount not available"); only the word before
 * the colon decides the tone.
 */
export function healthTone(status: string | null | undefined): HealthTone {
  const word = (status ?? '').split(':', 1)[0]!.trim().toLowerCase();
  switch (word) {
    case 'healthy':
    case 'online':
    case 'pass':
    case 'available':
      return 'ok';
    case 'degraded':
    case 'warn':
    case 'warning':
      return 'warn';
    case 'unhealthy':
    case 'offline':
    case 'fail':
    case 'error':
    case 'critical':
    case 'unavailable':
      return 'bad';
    default:
      return 'neutral';
  }
}

/** Pill styles: one badge token pair per tone, no fallbacks. */
export const healthPillStyles = css`
  .pill {
    display: inline-flex;
    align-items: center;
    gap: 0.375rem;
    padding: 0.0625rem 0.5rem;
    border-radius: 9999px;
    font-size: 0.75rem;
    font-weight: 600;
    white-space: nowrap;
  }

  .tone-ok {
    background: var(--scion-badge-success-bg);
    color: var(--scion-badge-success-text);
  }

  .tone-warn {
    background: var(--scion-badge-warning-bg);
    color: var(--scion-badge-warning-text);
  }

  .tone-bad {
    background: var(--scion-badge-danger-bg);
    color: var(--scion-badge-danger-text);
  }

  .tone-neutral {
    background: var(--scion-badge-neutral-bg);
    color: var(--scion-badge-neutral-text);
  }
`;
