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

import { html, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import './status-badge.js';
import type { Agent } from '../../shared/types.js';
import {
  PROVISIONED_ONLY_LABEL,
  isProvisionedOnly,
  provisionedStartHint,
} from '../../shared/agent-state-display.js';

/**
 * "Provisioned, not started" badge with a start hint (ptone/scion#2929),
 * or nothing when the agent is not provision-only.
 */
export function renderProvisionedBadge(
  agent: Pick<Agent, 'name' | 'phase' | 'provisionedOnly'>,
  size: 'small' | 'medium' = 'medium'
): TemplateResult | typeof nothing {
  if (!isProvisionedOnly(agent)) return nothing;
  return html`<scion-status-badge
    class="provisioned-badge"
    status="created"
    label=${PROVISIONED_ONLY_LABEL}
    title=${provisionedStartHint(agent.name)}
    size=${size}
  ></scion-status-badge>`;
}
