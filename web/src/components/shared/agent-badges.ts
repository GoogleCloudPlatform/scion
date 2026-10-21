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
 * Agent badges (ptone/scion#5503): a harness vendor mark plus a model size
 * chip, shown in the upper-left of an agent card, table row or graph node,
 * behind the `web.agent_badges` experiment. It renders nothing while the
 * experiment is off.
 *
 * Kept cheap for long lists: no fetches (it reads only fields already on the
 * list row), no sl-tooltip (a native title carries the tooltip), one
 * cached image per vendor (a content-hashed file, not a data URI), and hosts
 * create it only while the experiment is on (see {@link agentBadges}). The
 * row is a flex container, so a second badge (a model vendor) can be added
 * after the size chip later.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import type { Agent } from '../../shared/types.js';
import { agentBadgeView, agentHarnessName } from '../../shared/agent-badges.js';
import { AGENT_BADGES_FLAG, isFeatureEnabled } from '../../utils/feature-flags.js';

@customElement('scion-agent-badges')
export class ScionAgentBadges extends LitElement {
  /** The hub's `harnessVendor`; empty for a custom or unresolved harness. */
  @property()
  vendor = '';

  /** The hub's `modelSize` alias; empty when the agent has none. */
  @property()
  size = '';

  /** The harness name, when the row carries it (full rows only). */
  @property()
  harness = '';

  static override styles = css`
    :host {
      display: inline-flex;
      flex: none;
      vertical-align: middle;
    }

    .badges {
      display: inline-flex;
      align-items: center;
      gap: 3px;
      line-height: 1;
    }

    /* 16 CSS px; the image is 32x32 for HiDPI screens. */
    .mark {
      display: block;
      width: 16px;
      height: 16px;
      flex: none;
    }

    .size {
      box-sizing: border-box;
      display: inline-flex;
      align-items: center;
      height: 16px;
      padding: 0 4px;
      border-radius: 4px;
      border: 1px solid var(--sl-color-neutral-300, #d4d4d8);
      background: var(--sl-color-neutral-100, #f4f4f5);
      color: var(--sl-color-neutral-700, #3f3f46);
      font-size: 10px;
      font-weight: 600;
      font-family: var(--sl-font-sans, sans-serif);
    }
  `;

  override render(): TemplateResult | typeof nothing {
    if (!isFeatureEnabled(AGENT_BADGES_FLAG)) return nothing;
    const view = agentBadgeView(this.vendor, this.size, this.harness);
    const { mark } = view;
    return html`<span class="badges" role="group" aria-label=${view.title} title=${view.title}>
      <img class="mark" src=${mark.src} width="16" height="16" alt=${mark.label} />${view.size
        ? html`<span class="size">${view.size.chip}</span>`
        : nothing}
    </span>`;
  }
}

/**
 * The badges for `agent`. Hosts call this only while the experiment is on
 * (read once per host render), so a list with the experiment off creates no
 * badge elements at all.
 */
export function agentBadges(agent: Agent): TemplateResult {
  return html`<scion-agent-badges
    vendor=${agent.harnessVendor ?? ''}
    size=${agent.modelSize ?? ''}
    harness=${agentHarnessName(agent) ?? ''}
  ></scion-agent-badges>`;
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-badges': ScionAgentBadges;
  }
}
