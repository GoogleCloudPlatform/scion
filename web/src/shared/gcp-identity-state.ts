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
 * The GCP identity picker's state and rules, shared by the agent config form
 * (create page) and the configure page (ptone/scion#3974).
 *
 * GcpIdentityState is plain state, owned by the page and rendered by the
 * form, so the page can validate and build its request from it before and
 * after the form exists. normalizeGcpModeForTarget is the single
 * implementation of the Kubernetes Block rule for both pages.
 */

import { orderByAssignStatus } from './gcp-sa-assign-status.js';
import type { GCPServiceAccount, GCPServiceAccountAssignStatus } from './types.js';

export type GcpMetadataMode = 'block' | 'passthrough' | 'assign';

/** GCP Identity hint while no mode is chosen and no applied default is the outcome. */
export const NO_IDENTITY_MODE_HINT =
  "No mode chosen: the server applies this project's per-profile or project default, " +
  'then the hub-wide default, then the runtime default.';

/** Appended to an applied project default's hint; see showProfileDefaultPrecedence. */
export const PROFILE_DEFAULT_PRECEDENCE_HINT =
  'A per-profile default for the chosen profile, if set, takes precedence.';

/** The fields normalizeGcpModeForTarget reads and corrects. */
export interface GcpModeFields {
  gcpMetadataMode: GcpMetadataMode;
  gcpServiceAccountId: string;
  /** The user explicitly picked a mode or account this session. */
  gcpIdentityUserSet: boolean;
  /** An explicit Block pick suspended by the Kubernetes rule; see below. */
  gcpUserBlockSuspended: boolean;
  /** The mode that applies with no explicit pick: a default, before substitution. */
  defaultGcpMetadataMode: GcpMetadataMode;
  /** The account that goes with defaultGcpMetadataMode === 'assign'. */
  defaultGcpServiceAccountId: string;
}

/**
 * Keeps the *displayed* mode correct for the target. Block is never valid to
 * send on a known-Kubernetes target (the dispatch rejects it).
 *
 * With fromStorage (an existing agent's stored decision, configure page),
 * the mode is never rewritten: a stored value is not migrated. Without it
 * (create semantics):
 * - an explicit Block pick on a Kubernetes target is suspended, not
 *   discarded, and gcpIdentityUserSet is cleared, so the substituted display
 *   value is not sent as if the user chose it;
 * - the suspended Block is reinstated as soon as the target stops being
 *   Kubernetes-only;
 * - with no explicit pick standing, the mode is recomputed from the default,
 *   with a default of Block shown as Passthrough on a Kubernetes target, and
 *   the account kept in step with an Assign default.
 *
 * Recomputing from the default on every call, instead of remembering "the
 * current value is a substitution", means there is nothing to go stale.
 * Idempotent; returns true when it changed anything.
 */
export function normalizeGcpModeForTarget(
  s: GcpModeFields,
  targetKubernetesOnly: boolean,
  fromStorage = false
): boolean {
  if (fromStorage) return false;
  const before = JSON.stringify(s);
  if (s.gcpUserBlockSuspended && !targetKubernetesOnly) {
    s.gcpMetadataMode = 'block';
    s.gcpServiceAccountId = '';
    s.gcpIdentityUserSet = true;
    s.gcpUserBlockSuspended = false;
    return true;
  }
  if (s.gcpIdentityUserSet) {
    if (s.gcpMetadataMode === 'block' && targetKubernetesOnly) {
      s.gcpIdentityUserSet = false;
      s.gcpUserBlockSuspended = true;
    } else {
      return false;
    }
  }
  const effective: GcpMetadataMode =
    targetKubernetesOnly && s.defaultGcpMetadataMode === 'block'
      ? 'passthrough'
      : s.defaultGcpMetadataMode;
  s.gcpMetadataMode = effective;
  if (effective === 'assign') {
    s.gcpServiceAccountId = s.defaultGcpServiceAccountId;
  } else {
    s.gcpServiceAccountId = '';
  }
  return JSON.stringify(s) !== before;
}

/** The project's GCP identity defaults, from its settings. */
export interface GcpProjectDefaults {
  defaultGCPIdentityMode?: string;
  defaultGCPIdentityServiceAccountID?: string;
  defaultGCPIdentityServiceAccountIDByProfile?: Record<string, string>;
}

/**
 * The create form's GCP identity picker. The page owns one instance, feeds
 * it the target (targetKubernetesOnly, profile) and the project's accounts
 * and defaults, and builds gcp_identity from it; the form renders it and
 * records picks. Every mutation notifies the subscribers so both can
 * re-render.
 */
export class GcpIdentityState implements GcpModeFields {
  gcpMetadataMode: GcpMetadataMode = 'block';
  gcpServiceAccountId = '';
  /**
   * True once the user has explicitly picked in the GCP Identity picker
   * (either select) this session. Cleared when defaults are recomputed
   * (reset, e.g. on a project change) and when normalizeGcpModeForTarget
   * suspends an explicit Block. Gates whether gcp_identity is sent at all:
   * with no explicit choice the request omits it, so the server resolves the
   * identity from its own precedence.
   */
  gcpIdentityUserSet = false;
  gcpUserBlockSuspended = false;
  defaultGcpMetadataMode: GcpMetadataMode = 'block';
  defaultGcpServiceAccountId = '';
  gcpServiceAccounts: GCPServiceAccount[] = [];
  /** The project's own default mode, or '' when it has none. */
  projectGCPIdentityDefaultMode = '';
  /**
   * The project default was applied to the default mode (a block or
   * passthrough default, or an assign default whose account is verified).
   */
  projectGCPIdentityDefaultApplied = false;
  /** The project's per-profile defaults (profile name to account ID). */
  projectGCPIdentityProfileDefaults: Record<string, string> = {};

  /** The target is reliably known to be Kubernetes; set by the page. */
  targetKubernetesOnly = false;
  /** The explicitly selected runtime profile; set by the page. */
  profile = '';

  private readonly listeners = new Set<() => void>();

  /** Calls fn after every change; returns the unsubscribe function. */
  subscribe(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  /** Notifies the subscribers; for a caller that changed the fields directly. */
  notify(): void {
    for (const fn of this.listeners) fn();
  }

  get verifiedGCPServiceAccounts(): GCPServiceAccount[] {
    return this.gcpServiceAccounts.filter((sa) => sa.verified);
  }

  /**
   * The verified accounts in picker order: not mapped on the chosen
   * Kubernetes profile last, otherwise as listed (ptone/scion#4391).
   */
  get pickerServiceAccounts(): GCPServiceAccount[] {
    return orderByAssignStatus(this.verifiedGCPServiceAccounts);
  }

  /** The selected account's mapping state on the chosen profile, when the hub reported one. */
  get selectedAssignStatus(): GCPServiceAccountAssignStatus | undefined {
    if (!this.gcpServiceAccountId) return undefined;
    return this.gcpServiceAccounts.find((sa) => sa.id === this.gcpServiceAccountId)?.assignStatus;
  }

  /** Applies the target and re-normalizes the displayed mode. */
  setTarget(targetKubernetesOnly: boolean, profile: string): void {
    const targetChanged =
      this.targetKubernetesOnly !== targetKubernetesOnly || this.profile !== profile;
    this.targetKubernetesOnly = targetKubernetesOnly;
    this.profile = profile;
    if (this.normalize() || targetChanged) this.notify();
  }

  /** Re-applies normalizeGcpModeForTarget; returns true when it changed anything. */
  normalize(): boolean {
    return normalizeGcpModeForTarget(this, this.targetKubernetesOnly);
  }

  /**
   * Recomputes from scratch, before a project's accounts and defaults load:
   * whatever is applied afterwards is a default, not a user choice, and a
   * suspended Block pick belonged to the previous project.
   */
  reset(): void {
    this.gcpServiceAccounts = [];
    this.gcpServiceAccountId = '';
    this.gcpMetadataMode = 'block';
    this.defaultGcpMetadataMode = 'block';
    this.defaultGcpServiceAccountId = '';
    this.gcpIdentityUserSet = false;
    this.gcpUserBlockSuspended = false;
    this.projectGCPIdentityDefaultMode = '';
    this.projectGCPIdentityDefaultApplied = false;
    this.projectGCPIdentityProfileDefaults = {};
    this.normalize();
    this.notify();
  }

  /**
   * Replaces the accounts without touching the mode, the chosen account or
   * the defaults: used both for the project's first load and to refresh the
   * mapping state when the target changes.
   */
  setAccounts(accounts: GCPServiceAccount[]): void {
    this.gcpServiceAccounts = accounts;
    this.notify();
  }

  /**
   * Records the project's defaults. The current value is seeded from the
   * default only when the user has not picked while the load was in flight:
   * an arriving default never overwrites a user choice (ptone/scion#2548).
   */
  applyProjectDefaults(settings: GcpProjectDefaults | null): void {
    this.projectGCPIdentityProfileDefaults =
      settings?.defaultGCPIdentityServiceAccountIDByProfile ?? {};
    if (settings?.defaultGCPIdentityMode) {
      this.projectGCPIdentityDefaultMode = settings.defaultGCPIdentityMode;
      const mode = settings.defaultGCPIdentityMode as GcpMetadataMode;
      const applyToCurrent = !this.gcpIdentityUserSet;
      if (mode === 'assign' && settings.defaultGCPIdentityServiceAccountID) {
        const match = this.verifiedGCPServiceAccounts.find(
          (sa) => sa.id === settings.defaultGCPIdentityServiceAccountID
        );
        if (match) {
          this.defaultGcpMetadataMode = 'assign';
          this.defaultGcpServiceAccountId = match.id;
          this.projectGCPIdentityDefaultApplied = true;
          if (applyToCurrent) {
            this.gcpMetadataMode = 'assign';
            this.gcpServiceAccountId = match.id;
          }
        }
      } else if (mode === 'passthrough' || mode === 'block') {
        this.defaultGcpMetadataMode = mode;
        this.projectGCPIdentityDefaultApplied = true;
        if (applyToCurrent) this.gcpMetadataMode = mode;
      }
    }
    this.normalize();
    this.notify();
  }

  /** The user picked a mode. Any pick, including the shown one, is a choice. */
  pickMode(mode: GcpMetadataMode): void {
    this.gcpMetadataMode = mode;
    this.gcpIdentityUserSet = true;
    this.gcpUserBlockSuspended = false;
    if (mode !== 'assign') this.gcpServiceAccountId = '';
    this.normalize();
    this.notify();
  }

  /** The user picked a service account. */
  pickServiceAccount(id: string): void {
    this.gcpServiceAccountId = id;
    this.gcpIdentityUserSet = true;
    this.gcpUserBlockSuspended = false;
    this.notify();
  }

  /** The selected profile has a per-profile default, which outranks the project default. */
  get selectedProfileHasPerProfileDefault(): boolean {
    return !!this.profile && !!this.projectGCPIdentityProfileDefaults[this.profile];
  }

  /** The applied project default is what the server resolves for an untouched picker. */
  get projectDefaultIsOutcome(): boolean {
    return this.projectGCPIdentityDefaultApplied && !this.selectedProfileHasPerProfileDefault;
  }

  /**
   * Omitting gcp_identity would resolve to the project's own default of
   * "block" on a known-Kubernetes target, which the dispatch rejects: the
   * picker shows no value and submit waits for an explicit pick.
   */
  get blockDefaultNeedsExplicitChoice(): boolean {
    return (
      this.targetKubernetesOnly &&
      this.projectGCPIdentityDefaultMode === 'block' &&
      !this.selectedProfileHasPerProfileDefault &&
      !this.gcpIdentityUserSet
    );
  }

  /**
   * Nothing chosen and no applied project default is the outcome, on any
   * target: the picker renders blank (so any pick is a real change) with
   * NO_IDENTITY_MODE_HINT. Submitting with nothing chosen is allowed.
   */
  get noIdentityModeChosen(): boolean {
    return !this.gcpIdentityUserSet && !this.projectDefaultIsOutcome;
  }

  /** The picker shows no value. */
  get pickerBlank(): boolean {
    return this.blockDefaultNeedsExplicitChoice || this.noIdentityModeChosen;
  }

  /** The Kubernetes-specific portion of the hint. */
  get kubernetesIdentityHintSuffix(): string {
    if (this.blockDefaultNeedsExplicitChoice) {
      return (
        "This project's default GCP identity is Block, which the Kubernetes runtime rejects at " +
        'dispatch; creating this agent is blocked until you explicitly choose Passthrough' +
        (this.verifiedGCPServiceAccounts.length > 0 ? ' or Assign Service Account.' : '.')
      );
    }
    return 'Block is not available for a Kubernetes runtime target.';
  }

  /** The hint should note that a per-profile default outranks the applied project default. */
  get showProfileDefaultPrecedence(): boolean {
    return (
      !this.gcpIdentityUserSet &&
      this.projectDefaultIsOutcome &&
      !this.profile &&
      Object.keys(this.projectGCPIdentityProfileDefaults).length > 0 &&
      !this.blockDefaultNeedsExplicitChoice
    );
  }

  /** The picker's hint text. */
  get hint(): string {
    const base = this.blockDefaultNeedsExplicitChoice
      ? 'No GCP identity is selected yet.'
      : this.noIdentityModeChosen
        ? NO_IDENTITY_MODE_HINT
        : this.gcpMetadataMode === 'block'
          ? 'Prevents the agent from accessing any GCP identity. Token requests are denied.'
          : this.gcpMetadataMode === 'assign'
            ? 'Assigns a registered GCP service account. GCP client libraries will authenticate automatically.'
            : "No metadata interception. The agent inherits the broker's GCP identity. Requires broker ownership.";
    const precedence = this.showProfileDefaultPrecedence
      ? ` ${PROFILE_DEFAULT_PRECEDENCE_HINT}`
      : '';
    const k8s = this.targetKubernetesOnly ? ` ${this.kubernetesIdentityHintSuffix}` : '';
    return base + precedence + k8s;
  }

  /**
   * A validation error that blocks submit, or null. Nothing chosen is still
   * submittable (the request omits gcp_identity) unless the project's Block
   * default would be rejected on a Kubernetes target. The Block check also
   * guards a displayed Block that normalization has not corrected yet.
   */
  validate(): string | null {
    if (this.gcpMetadataMode === 'assign' && !this.gcpServiceAccountId) {
      return 'Please select a service account for GCP identity assignment.';
    }
    if (this.gcpMetadataMode === 'block' && this.targetKubernetesOnly) {
      return 'Block is not available for a Kubernetes runtime target. Choose Passthrough or Assign Service Account.';
    }
    if (this.blockDefaultNeedsExplicitChoice) {
      return (
        "This project's default GCP identity is Block, which the Kubernetes runtime rejects at " +
        'dispatch. Choose Passthrough' +
        (this.verifiedGCPServiceAccounts.length > 0 ? ' or Assign Service Account' : '') +
        ' before creating this agent.'
      );
    }
    return null;
  }

  /** gcp_identity for the create request: only an explicit choice, else undefined. */
  toRequest(): { metadata_mode: GcpMetadataMode; service_account_id?: string } | undefined {
    if (!this.gcpIdentityUserSet) return undefined;
    if (this.gcpMetadataMode === 'assign') {
      return this.gcpServiceAccountId
        ? { metadata_mode: 'assign', service_account_id: this.gcpServiceAccountId }
        : undefined;
    }
    return { metadata_mode: this.gcpMetadataMode };
  }
}
