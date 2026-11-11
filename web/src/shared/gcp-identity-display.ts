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
 * Display rules for an agent's applied GCP identity (ptone/scion#4017).
 *
 * The agent payload carries only the applied mode plus the denormalized email
 * and project of the assigned account. Display name and verification status
 * live on the registered service account record, which the caller fetches
 * separately and passes in. When that record is missing (not fetched yet,
 * refused, or deleted), verification is "unknown": the page cannot tell, and
 * must not claim either answer.
 */

import type { GCPIdentityConfig, GCPServiceAccount } from './types.js';

export type GCPMetadataMode = GCPIdentityConfig['metadataMode'];

export type GCPIdentityVerification = 'verified' | 'not-verified' | 'unknown';

export interface GCPIdentityVerificationDisplay {
  state: GCPIdentityVerification;
  label: string;
  variant: 'success' | 'warning' | 'danger' | 'neutral';
  /** Explanatory text for a tooltip, when there is something to explain. */
  detail?: string;
}

const MODE_LABELS: Record<GCPMetadataMode, string> = {
  assign: 'Assign',
  passthrough: 'Passthrough',
  block: 'Block',
};

/** Human label for a metadata mode. Unknown modes are shown as sent. */
export function gcpModeLabel(mode: string): string {
  return MODE_LABELS[mode as GCPMetadataMode] ?? mode;
}

/** Badge variant for a metadata mode. */
export function gcpModeVariant(mode: string): 'primary' | 'warning' | 'neutral' {
  if (mode === 'assign') return 'primary';
  if (mode === 'passthrough') return 'warning';
  return 'neutral';
}

/**
 * Returns the registered record only when it is the record for this identity.
 * A record fetched for an earlier assignment must not lend its display name or
 * verification to a different account.
 */
export function matchingAccount(
  identity: GCPIdentityConfig,
  account: GCPServiceAccount | null | undefined
): GCPServiceAccount | null {
  if (!account || !identity.serviceAccountId) return null;
  return account.id === identity.serviceAccountId ? account : null;
}

/**
 * Verification status of the assigned account, from its registered record.
 * While the record is still loading the status is unknown with no
 * explanation, rather than one claiming the load failed.
 */
export function gcpVerificationDisplay(
  account: GCPServiceAccount | null,
  loading = false
): GCPIdentityVerificationDisplay {
  if (!account && loading) {
    return { state: 'unknown', label: 'Unknown', variant: 'neutral' };
  }
  if (!account) {
    return {
      state: 'unknown',
      label: 'Unknown',
      variant: 'neutral',
      detail: 'The registered service account record could not be loaded.',
    };
  }
  const status = account.verificationStatus ?? (account.verified ? 'verified' : 'unverified');
  if (status === 'verified') {
    return { state: 'verified', label: 'Verified', variant: 'success' };
  }
  if (status === 'failed') {
    return {
      state: 'not-verified',
      label: 'Not verified',
      variant: 'danger',
      detail: account.verificationError || 'The last verification attempt failed.',
    };
  }
  return {
    state: 'not-verified',
    label: 'Not verified',
    variant: 'warning',
    detail: 'This service account has not been verified yet.',
  };
}
