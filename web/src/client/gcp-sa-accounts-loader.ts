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
 * Loads a project's service accounts for a picker, with each account's
 * mapping state on the picker's Kubernetes target (ptone/scion#4391).
 *
 * Shared by every picker that labels accounts, so they keep the same rules:
 *  - the mapping state is asked for only when the target is known
 *    (assignStatusTarget), and asked again when the target changes;
 *  - only the newest fetch's accounts are applied: a slow response for an
 *    earlier project or target never overwrites a newer one;
 *  - when the target is no longer known, or the request for the mapping
 *    state fails, the previous target's labels are dropped rather than shown
 *    against the new one;
 *  - asking for the mapping state adds a project-read check on the hub, so a
 *    failed first load falls back to the plain list; a hub that predates
 *    assignStatus returns accounts without it, and the picker works as before.
 */

import { apiFetch } from './api.js';
import {
  assignStatusTargetKey,
  projectServiceAccountListUrl,
  withoutAssignStatus,
  type AssignStatusTarget,
} from '../shared/gcp-sa-assign-status.js';
import type { GCPServiceAccount } from '../shared/types.js';

/** What the loader reads from, and writes to, the page that owns the picker. */
export interface GCPServiceAccountsHost {
  /** The project the page shows now ('' for none). */
  projectId(): string;
  /** The Kubernetes target to ask the mapping state for now, or null. */
  target(): AssignStatusTarget | null;
  /** The accounts the picker shows now. */
  accounts(): GCPServiceAccount[];
  /** Replaces the accounts the picker shows. */
  setAccounts(accounts: GCPServiceAccount[]): void;
}

/** Fetches the project's accounts, with their mapping state on target when given. */
export async function fetchProjectServiceAccounts(
  projectId: string,
  target: AssignStatusTarget | null
): Promise<GCPServiceAccount[] | null> {
  try {
    const res = await apiFetch(projectServiceAccountListUrl(projectId, target));
    if (!res.ok) return null;
    const data = (await res.json()) as { items?: GCPServiceAccount[] } | GCPServiceAccount[];
    return Array.isArray(data) ? data : data.items || [];
  } catch {
    return null;
  }
}

export class GCPServiceAccountsLoader {
  /**
   * Incremented by every load. Each load drops its results if a newer one
   * has started since, or the project changed under it (ptone/scion#2548).
   */
  private loadSeq = 0;

  /**
   * Incremented by every fetch of the accounts (each load and each
   * mapping-state refresh), so only the newest fetch's mapping state is kept.
   */
  private assignSeq = 0;

  /** The assignSeq of the last refresh whose accounts were applied. */
  private assignAppliedSeq = 0;

  /** The key of the target the current accounts' mapping state was asked for. */
  private targetKey = '';

  constructor(private readonly host: GCPServiceAccountsHost) {}

  /**
   * Loads the current project's accounts (the first load, or after a project
   * change). Resolves to a staleness check the caller can keep using for its
   * own follow-up fetches, or null when there is no project or a newer load
   * has already superseded this one.
   */
  async load(): Promise<(() => boolean) | null> {
    const seq = ++this.loadSeq;
    const assignSeq = ++this.assignSeq;
    const projectId = this.host.projectId();
    const isStale = (): boolean => seq !== this.loadSeq || this.host.projectId() !== projectId;
    if (!projectId) return null;

    const target = this.host.target();
    this.targetKey = assignStatusTargetKey(target);
    let fetched = await fetchProjectServiceAccounts(projectId, target);
    if (isStale()) return null;
    if (fetched === null && target) {
      fetched = await fetchProjectServiceAccounts(projectId, null);
      if (isStale()) return null;
    }
    // Non-critical: a failed fetch leaves the picker without accounts.
    const accounts = fetched ?? [];
    // The target changed while this load was in flight, and a refresh for
    // the new one has started. If that refresh has already landed, its
    // accounts are the current ones: keep them. Otherwise show this load's
    // accounts without its old-target labels; the refresh fills them in.
    if (assignSeq !== this.assignSeq) {
      if (this.assignAppliedSeq !== this.assignSeq) {
        this.host.setAccounts(withoutAssignStatus(accounts));
      }
    } else {
      this.host.setAccounts(accounts);
    }
    return isStale;
  }

  /**
   * Call whenever the target may have changed. Asks the mapping state again
   * when the target differs from the one last asked about; does nothing
   * before the first load has started. Resolves when that refresh is done.
   */
  targetChanged(): Promise<void> {
    if (this.loadSeq === 0) return Promise.resolve();
    if (assignStatusTargetKey(this.host.target()) === this.targetKey) return Promise.resolve();
    return this.refresh();
  }

  /**
   * Re-asks the accounts' mapping state after the target changed, keeping
   * whatever the page has chosen. With no target known any more, or when the
   * fetch fails, the previous target's labels are dropped.
   */
  private async refresh(): Promise<void> {
    const projectId = this.host.projectId();
    if (!projectId) return;
    const loadSeq = this.loadSeq;
    const seq = ++this.assignSeq;
    const target = this.host.target();
    this.targetKey = assignStatusTargetKey(target);
    const isStale = (): boolean =>
      seq !== this.assignSeq || loadSeq !== this.loadSeq || this.host.projectId() !== projectId;
    if (!target) {
      this.host.setAccounts(withoutAssignStatus(this.host.accounts()));
      return;
    }
    const accounts = await fetchProjectServiceAccounts(projectId, target);
    if (isStale()) return;
    if (accounts) {
      this.assignAppliedSeq = seq;
      this.host.setAccounts(accounts);
    } else {
      this.host.setAccounts(withoutAssignStatus(this.host.accounts()));
    }
  }
}
