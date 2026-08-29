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
 * Messageability Indicator Component
 *
 * Displays the viewer's messaging reachability with an agent as
 * a directional arrow icon with tooltip. Shows whether the viewer
 * can send messages to the agent and whether the agent can reply.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import { getDenialMessage } from '../../shared/message-mode.js';
import type { AgentMessageability } from '../../shared/types.js';

/**
 * Internal descriptor for the indicator's visual state.
 */
interface IndicatorState {
  icon: string;
  color: string;
  tooltip: string;
}

/**
 * Derive the visual state from messageability data.
 */
function getIndicatorState(messageability: AgentMessageability): IndicatorState {
  const { canMessage, canReachViewer, reason } = messageability;

  if (canMessage && canReachViewer) {
    return {
      icon: 'arrow-left-right',
      color: 'var(--sl-color-success-600)',
      tooltip: 'You can message this agent and it can message you',
    };
  }

  if (canMessage && !canReachViewer) {
    return {
      icon: 'arrow-right',
      color: 'var(--sl-color-neutral-500)',
      tooltip: 'You can message this agent but it cannot reply to you',
    };
  }

  if (!canMessage && canReachViewer) {
    return {
      icon: 'arrow-left',
      color: 'var(--sl-color-neutral-500)',
      tooltip: 'This agent can message you but you cannot message it',
    };
  }

  // !canMessage && !canReachViewer
  return {
    icon: 'x-circle',
    color: 'var(--sl-color-neutral-400)',
    tooltip: getDenialMessage(reason),
  };
}

@customElement('scion-messageability-indicator')
export class ScionMessageabilityIndicator extends LitElement {
  /**
   * Messageability data from the API. When absent the component
   * renders nothing (graceful degradation — see design doc Section 7.6).
   */
  @property({ type: Object })
  messageability?: AgentMessageability;

  /**
   * Size variant — matches the badge size semantics.
   */
  @property()
  size: 'small' | 'medium' = 'small';

  static override styles = css`
    :host {
      display: inline-flex;
      align-items: center;
    }

    .indicator {
      display: inline-flex;
      align-items: center;
    }

    .indicator.small sl-icon {
      font-size: 12px;
    }

    .indicator.medium sl-icon {
      font-size: 14px;
    }
  `;

  override render() {
    if (!this.messageability) {
      return nothing;
    }

    const state = getIndicatorState(this.messageability);

    return html`
      <sl-tooltip content="${state.tooltip}">
        <span
          class="indicator ${this.size}"
          aria-label="${state.tooltip}"
          role="img"
        >
          <sl-icon
            name="${state.icon}"
            style="color: ${state.color}"
          ></sl-icon>
        </span>
      </sl-tooltip>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-messageability-indicator': ScionMessageabilityIndicator;
  }
}
