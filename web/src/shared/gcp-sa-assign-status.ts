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
 * The service-account picker's mapping state for a Kubernetes target
 * (ptone/scion#4391).
 *
 * The hub can annotate each account in a project's service-account list with
 * whether it is mapped on one broker profile (assignStatus). The picker asks
 * for it only when the target is a known Kubernetes broker with a chosen
 * profile, labels every account, and lists not-mapped accounts last. Nothing
 * is filtered out. "Mapped" is the strongest claim: the hub cannot see the
 * Workload Identity IAM binding, so the label never says "ready".
 *
 * A hub that predates assignStatus returns accounts without it; they get no
 * label and keep their order, so the picker behaves as it did before.
 */

import type { GCPServiceAccount, GCPServiceAccountAssignStatus } from './types.js';

/** The broker and profile the picker asks the hub about. */
export interface AssignStatusTarget {
  brokerId: string;
  profile: string;
}

/**
 * The target to ask about, or null when it is not known well enough: the
 * target must be reliably Kubernetes, with both a broker and an explicitly
 * chosen profile (with no profile the hub could only answer "unknown").
 */
export function assignStatusTarget(
  targetKubernetesOnly: boolean,
  brokerId: string,
  profile: string
): AssignStatusTarget | null {
  if (!targetKubernetesOnly || !brokerId || !profile) return null;
  return { brokerId, profile };
}

/** A stable key for a target, for comparing targets across awaits. */
export function assignStatusTargetKey(target: AssignStatusTarget | null): string {
  return target ? `${target.brokerId}\u0000${target.profile}` : '';
}

/**
 * The project's service-account list URL, including hub-scoped accounts,
 * asking for each account's mapping state on the target when there is one.
 */
export function projectServiceAccountListUrl(
  projectId: string,
  target: AssignStatusTarget | null
): string {
  const base = `/api/v1/projects/${encodeURIComponent(projectId)}/gcp-service-accounts?includeHubScoped=true`;
  if (!target) return base;
  return (
    `${base}&assignStatus=true` +
    `&profile=${encodeURIComponent(target.profile)}` +
    `&broker=${encodeURIComponent(target.brokerId)}`
  );
}

/** Short words for each unknown reason, shown in the option label. */
const UNKNOWN_REASON_LABELS: Record<string, string> = {
  no_broker: 'no broker',
  no_profile: 'no profile',
  no_account: 'no account email',
  profile_not_on_broker: 'profile not on broker',
  runtime_unrecognized: 'runtime not recognised',
  report_missing: 'no broker report',
  report_incomplete: 'report incomplete',
  report_old_version: 'older broker',
  report_stale: 'report stale',
  ambiguous_mapping: 'ambiguous mapping',
};

/**
 * The short label for an account's mapping state, or '' when the account
 * carries none (not asked for, or an older hub). Unrecognised states and
 * reasons from a newer hub still get a neutral label rather than a guess.
 */
export function assignStatusLabel(status: GCPServiceAccountAssignStatus | undefined): string {
  if (!status) return '';
  switch (status.state) {
    case 'mapped':
      return 'mapped';
    case 'not_mapped':
      return 'not mapped on this profile';
    case 'not_required':
      return 'no mapping needed';
    case 'unknown': {
      const reason = status.reason ? UNKNOWN_REASON_LABELS[status.reason] : undefined;
      return reason ? `mapping unknown: ${reason}` : 'mapping unknown';
    }
    default:
      return 'mapping unknown';
  }
}

/**
 * The accounts in picker order: not-mapped accounts move to the end, and
 * every other account keeps the hub's order. Nothing is removed.
 */
export function orderByAssignStatus<T extends Pick<GCPServiceAccount, 'assignStatus'>>(
  accounts: T[]
): T[] {
  const notMapped = (sa: T) => sa.assignStatus?.state === 'not_mapped';
  return [...accounts.filter((sa) => !notMapped(sa)), ...accounts.filter(notMapped)];
}

/** The accounts without any mapping state, for when the target is no longer known. */
export function withoutAssignStatus(accounts: GCPServiceAccount[]): GCPServiceAccount[] {
  if (!accounts.some((sa) => sa.assignStatus)) return accounts;
  return accounts.map((sa) => {
    if (!sa.assignStatus) return sa;
    const { assignStatus, ...rest } = sa;
    return rest;
  });
}
