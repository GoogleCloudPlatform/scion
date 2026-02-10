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
 * Status Badge Component
 *
 * Displays status indicators with appropriate colors and icons
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

/**
 * Supported status types
 */
export type StatusType =
  | 'running'
  | 'stopped'
  | 'provisioning'
  | 'starting'
  | 'stopping'
  | 'error'
  | 'healthy'
  | 'unhealthy'
  | 'pending'
  | 'active'
  | 'inactive'
  | 'success'
  | 'warning'
  | 'danger'
  | 'info'
  | 'neutral';

/**
 * Status configuration with variant and icon
 */
interface StatusConfig {
  variant: 'success' | 'warning' | 'danger' | 'primary' | 'neutral';
  icon?: string;
  pulse?: boolean;
}

const STATUS_MAP: Record<StatusType, StatusConfig> = {
  // Agent/Container statuses
  running: { variant: 'success', icon: 'play-circle', pulse: false },
  stopped: { variant: 'neutral', icon: 'stop-circle', pulse: false },
  provisioning: { variant: 'warning', icon: 'hourglass-split', pulse: true },
  starting: { variant: 'warning', icon: 'arrow-repeat', pulse: true },
  stopping: { variant: 'warning', icon: 'arrow-repeat', pulse: true },
  error: { variant: 'danger', icon: 'exclamation-triangle', pulse: false },

  // Health statuses
  healthy: { variant: 'success', icon: 'check-circle', pulse: false },
  unhealthy: { variant: 'danger', icon: 'x-circle', pulse: false },

  // General statuses
  pending: { variant: 'warning', icon: 'clock', pulse: true },
  active: { variant: 'success', icon: 'circle-fill', pulse: false },
  inactive: { variant: 'neutral', icon: 'circle', pulse: false },

  // Semantic statuses
  success: { variant: 'success', pulse: false },
  warning: { variant: 'warning', pulse: false },
  danger: { variant: 'danger', pulse: false },
  info: { variant: 'primary', pulse: false },
  neutral: { variant: 'neutral', pulse: false },
};

@customElement('scion-status-badge')
export class ScionStatusBadge extends LitElement {
  /**
   * The status to display
   */
  @property({ type: String })
  status: StatusType = 'neutral';

  /**
   * Optional custom label (defaults to capitalized status)
   */
  @property({ type: String })
  label = '';

  /**
   * Size variant
   */
  @property({ type: String })
  size: 'small' | 'medium' | 'large' = 'medium';

  /**
   * Whether to show the status icon
   */
  @property({ type: Boolean })
  showIcon = true;

  /**
   * Whether to show a pulsing indicator for active states
   */
  @property({ type: Boolean })
  showPulse = true;

  static override styles = css`
    :host {
      display: inline-flex;
    }

    .badge {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0.625rem;
      border-radius: 9999px;
      font-weight: 500;
      text-transform: capitalize;
      white-space: nowrap;
    }

    /* Size variants */
    .badge.small {
      font-size: 0.6875rem;
      padding: 0.125rem 0.5rem;
      gap: 0.25rem;
    }

    .badge.medium {
      font-size: 0.75rem;
    }

    .badge.large {
      font-size: 0.875rem;
      padding: 0.375rem 0.75rem;
    }

    .badge sl-icon {
      font-size: 0.875em;
    }

    .badge.small sl-icon {
      font-size: 0.75em;
    }

    .badge.large sl-icon {
      font-size: 1em;
    }

    /* Variant colors */
    .badge.success {
      background: var(--scion-success-100, #dcfce7);
      color: var(--scion-success-700, #15803d);
    }

    .badge.warning {
      background: var(--scion-warning-100, #fef3c7);
      color: var(--scion-warning-700, #b45309);
    }

    .badge.danger {
      background: var(--scion-danger-100, #fee2e2);
      color: var(--scion-danger-700, #b91c1c);
    }

    .badge.primary {
      background: var(--scion-primary-100, #dbeafe);
      color: var(--scion-primary-700, #1d4ed8);
    }

    .badge.neutral {
      background: var(--scion-neutral-100, #f1f5f9);
      color: var(--scion-neutral-600, #475569);
    }

    /* Pulse indicator */
    .pulse {
      position: relative;
    }

    .pulse::before {
      content: '';
      position: absolute;
      left: 0.5rem;
      width: 0.375rem;
      height: 0.375rem;
      border-radius: 50%;
      animation: pulse 2s infinite;
    }

    .pulse.success::before {
      background: var(--scion-success-500, #22c55e);
      box-shadow: 0 0 0 0 var(--scion-success-400, #4ade80);
    }

    .pulse.warning::before {
      background: var(--scion-warning-500, #f59e0b);
      box-shadow: 0 0 0 0 var(--scion-warning-400, #fbbf24);
    }

    .pulse.danger::before {
      background: var(--scion-danger-500, #ef4444);
      box-shadow: 0 0 0 0 var(--scion-danger-400, #f87171);
    }

    @keyframes pulse {
      0% {
        box-shadow:
          0 0 0 0 rgba(34, 197, 94, 0.5),
          0 0 0 0 rgba(34, 197, 94, 0.3);
      }
      70% {
        box-shadow:
          0 0 0 6px rgba(34, 197, 94, 0),
          0 0 0 10px rgba(34, 197, 94, 0);
      }
      100% {
        box-shadow:
          0 0 0 0 rgba(34, 197, 94, 0),
          0 0 0 0 rgba(34, 197, 94, 0);
      }
    }

    /* Dark mode adjustments */
    @media (prefers-color-scheme: dark) {
      .badge.success {
        background: rgba(34, 197, 94, 0.2);
        color: #86efac;
      }

      .badge.warning {
        background: rgba(245, 158, 11, 0.2);
        color: #fcd34d;
      }

      .badge.danger {
        background: rgba(239, 68, 68, 0.2);
        color: #fca5a5;
      }

      .badge.primary {
        background: rgba(59, 130, 246, 0.2);
        color: #93c5fd;
      }

      .badge.neutral {
        background: rgba(100, 116, 139, 0.2);
        color: #cbd5e1;
      }
    }
  `;

  override render() {
    const config = STATUS_MAP[this.status] || STATUS_MAP.neutral;
    const displayLabel = this.label || this.status;
    const shouldPulse = this.showPulse && config.pulse;

    return html`
      <span class="badge ${config.variant} ${this.size} ${shouldPulse ? 'pulse' : ''}">
        ${this.showIcon && config.icon ? html`<sl-icon name="${config.icon}"></sl-icon>` : ''}
        ${displayLabel}
      </span>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-status-badge': ScionStatusBadge;
  }
}
