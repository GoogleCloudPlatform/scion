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
 * Shared badges, tags, and approval PATCH helper for the admin-managed
 * Runtime Brokers policy (hub.admin_managed_brokers, ptone/scion#7829 §4.6).
 *
 * Everything rendered here is hidden unless experiment
 * `hub.admin_managed_brokers` is enabled.
 */

import { css, html, nothing, type TemplateResult } from 'lit';
import { can, type RuntimeBroker } from '../../shared/types.js';
import { ADMIN_MANAGED_BROKERS_FLAG, isFeatureEnabled } from '../../utils/feature-flags.js';
import { apiFetch, extractApiError } from '../../client/api.js';

export const brokerPolicyStyles = css`
  .admin-managed-badge {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    font-size: 0.6875rem;
    font-weight: 600;
    padding: 0.125rem 0.45rem;
    border-radius: 9999px;
    background: var(--sl-color-success-100, #dcfce7);
    color: var(--sl-color-success-700, #15803d);
    white-space: nowrap;
    vertical-align: middle;
  }

  .admin-managed-badge sl-icon {
    font-size: 0.75rem;
  }

  .policy-blocked-tag {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    font-size: 0.6875rem;
    font-weight: 600;
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
    background: var(--sl-color-warning-100, #fef3c7);
    color: var(--sl-color-warning-800, #92400e);
    white-space: nowrap;
    vertical-align: middle;
  }

  .policy-blocked-tag sl-icon {
    font-size: 0.75rem;
  }

  .broker-approval-actions {
    display: inline-flex;
    align-items: center;
    gap: 0.375rem;
  }

  .broker-card-wrap {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .broker-card-wrap > .resource-card {
    flex: 1;
  }

  .broker-card-actions {
    display: flex;
    justify-content: flex-end;
    padding-top: 0.5rem;
  }
`;

export function isAdminManagedBrokersExperimentOn(): boolean {
  return isFeatureEnabled(ADMIN_MANAGED_BROKERS_FLAG);
}

export function canManageBrokerApproval(broker: RuntimeBroker | null | undefined): boolean {
  if (!broker || !isAdminManagedBrokersExperimentOn()) return false;
  return can(broker._capabilities, 'manage');
}

export function renderAdminManagedBadge(
  broker: Pick<RuntimeBroker, 'adminManaged'> | null | undefined
): TemplateResult | typeof nothing {
  if (!broker || !isAdminManagedBrokersExperimentOn() || !broker.adminManaged) {
    return nothing;
  }
  return html`<span class="admin-managed-badge" data-testid="admin-managed-badge"
    ><sl-icon name="shield-check"></sl-icon> Admin-managed</span
  >`;
}

export function renderPolicyBlockedTag(
  broker: Pick<RuntimeBroker, 'policyEligible'> | null | undefined
): TemplateResult | typeof nothing {
  if (!broker || !isAdminManagedBrokersExperimentOn() || broker.policyEligible !== false) {
    return nothing;
  }
  return html`<span class="policy-blocked-tag" data-testid="policy-blocked-tag"
    ><sl-icon name="exclamation-triangle"></sl-icon> Not approved — new agents blocked</span
  >`;
}

export async function updateBrokerAdminManaged(
  brokerId: string,
  adminManaged: boolean
): Promise<RuntimeBroker> {
  const res = await apiFetch(`/api/v1/runtime-brokers/${encodeURIComponent(brokerId)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ adminManaged }),
  });
  if (!res.ok) {
    throw new Error(
      await extractApiError(
        res,
        adminManaged ? 'Failed to approve runtime broker' : 'Failed to revoke broker approval'
      )
    );
  }
  return (await res.json()) as RuntimeBroker;
}
