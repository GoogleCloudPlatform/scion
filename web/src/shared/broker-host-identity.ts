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

import { can } from './types.js';
import type { RuntimeBroker } from './types.js';

/** A host identity state that needs attention, as the UI shows it. */
export interface HostIdentityWarning {
  message: string;
  /** The configured GCP host service account; set only for callers allowed to update. */
  configured?: string;
  /** The account the host reports; set only for callers allowed to update. */
  observed?: string;
}

/**
 * The warning for the two host identity states that need attention
 * (mismatch and pinned_absent), with the same messages as the existing
 * broker detail page; null for every other state or when the hub sent
 * none. The account names are returned only when the broker's
 * capabilities allow update: the hub already sends the observed account
 * only to those callers, and the configured account is otherwise shown
 * only on the Configure tab, which needs update or delete.
 */
export function hostIdentityWarning(
  broker: RuntimeBroker | null | undefined
): HostIdentityWarning | null {
  const hostIdentity = broker?.hostIdentity;
  if (!broker || !hostIdentity) return null;
  let message: string;
  if (hostIdentity.state === 'mismatch') {
    message = "Pinned host service account differs from the host's identity";
  } else if (hostIdentity.state === 'pinned_absent') {
    message = 'Pinned host service account is not present on the host';
  } else {
    return null;
  }
  if (!can(broker._capabilities, 'update')) return { message };
  const warning: HostIdentityWarning = { message };
  const configured = broker.gcp_host_service_account_email;
  if (configured) warning.configured = configured;
  if (hostIdentity.observedEmail) warning.observed = hostIdentity.observedEmail;
  return warning;
}
