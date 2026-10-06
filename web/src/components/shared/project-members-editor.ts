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
 * Project Members Editor (multi-role, ptone/scion#2529)
 *
 * One row per principal. A principal holds at most one built-in project
 * role (owner, admin or member) plus any number of custom project roles.
 *
 *  - List:   GET    /api/v1/projects/{id}/members?groupBy=principal (all pages)
 *  - Roles:  GET    /api/v1/projects/{id}/members/assignable-roles
 *  - Save:   PUT    /api/v1/projects/{id}/members/principals/{type}/{id}
 *            One request per save, carrying the full desired role set and
 *            the role set the dialog started from (expectedRoleDefinitionIds,
 *            [] in Add mode). The server applies it atomically.
 *  - Remove: DELETE /api/v1/projects/{id}/members/principals/{type}/{id}
 *            Removes every direct binding the principal holds.
 *
 * Authority comes from the server: `_capabilities` decides which tiers the
 * actor can manage, assignable-roles reports per-role grantability, and
 * error `code`/`details` drive the dialog's recovery paths. Role names are
 * only used to place a built-in role in its tier.
 *
 * Parts of this module (custom-role checkboxes, describeCustomRoleError,
 * the test harness) are ported from miller79/scion PR #127 by Anthony Lofton.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError, parseApiError } from '../../client/api.js';
import type { ApiErrorInfo } from '../../client/api.js';
import { dispatchMembershipChanged } from '../../utils/membership-events.js';
import type {
  AssignableProjectRole,
  MembershipCapabilities,
  ProjectMemberBinding,
  ProjectMemberGroup,
} from '../../shared/types.js';
import type { PrincipalChangeDetail } from './principal-picker.js';
import { showConfirm } from './confirm-dialog.js';
import './principal-picker.js';
import {
  BUILT_IN_PROJECT_MEMBERSHIP_ROLES,
  getPrincipalIcon,
  getRoleTier,
  type RoleTier,
} from './role-binding-utils.js';

// ---------------------------------------------------------------------------
// Pure helpers (exported for tests)
// ---------------------------------------------------------------------------

/** Radio value for "no built-in project role". */
export const NO_PROJECT_ROLE = '__none__';

export type MemberPrincipalType = 'user' | 'agent' | 'group';

export type MemberDialogMode = 'add' | 'edit';

/** Page size for the grouped members list. */
export const MEMBERS_PAGE_SIZE = 500;

/** Upper bound on pages fetched, as a guard against a misbehaving server. */
const MAX_MEMBER_PAGES = 50;

export const EMPTY_SELECTION_MESSAGE = 'A member needs at least one role.';
export const TIER_REASON = 'Only project owners can assign admin/owner';
export const CUSTOM_TIER_CAPTION = 'Only project owners can change custom roles';
export const OWNER_ONLY_REMOVE_REASON = 'Holds roles only an owner can remove';
export const ROW_LOCKED_REASON = "Only project owners can change this member's roles.";
export const CATALOG_UNAVAILABLE_REASON = "Couldn't load the list of roles; close and try again.";
export const LAST_OWNER_REASON = 'Last direct owner. Transfer ownership before changing this role.';
export const LAST_OWNER_REMOVE_REASON =
  'Last direct owner. Transfer ownership before removing this member.';
export const CHANGED_WHILE_EDITING_MESSAGE =
  'This member changed while you were editing; review and save again.';
export const CHANGED_WHILE_EDITING_LOCKED_MESSAGE = 'This member changed while you were editing.';
export const AUTHORITY_CHANGED_MESSAGE =
  'Your permissions on this project changed while saving. The options were refreshed; review and save again.';
export const AUTHORITY_REDUCED_MESSAGE =
  'Your permissions on this project changed while saving. Nothing was saved.';
export const REMOVES_ALL_ROLES = 'This removes all of their project roles.';
export const CATALOG_REFRESHED_MESSAGE =
  'The list of roles was refreshed; review your selection and save again.';

export interface OptionState {
  disabled: boolean;
  reason: string;
}

/** True for project roles other than the built-in membership roles. */
export function isCustomProjectRole(roleName: string): boolean {
  return !BUILT_IN_PROJECT_MEMBERSHIP_ROLES.includes(roleName);
}

/** True for a binding to a custom role; prefers the server's roleKind. */
export function isCustomBinding(b: Pick<ProjectMemberBinding, 'roleName' | 'roleKind'>): boolean {
  if (b.roleKind) return b.roleKind === 'custom';
  return isCustomProjectRole(b.roleName);
}

/** Turns the server's delegation-ceiling refusal into guidance; other errors
 *  pass through unchanged. Ported from miller79/scion PR #127. */
export function describeCustomRoleError(message: string): string {
  const m = /lacks permission for delegation: ([\w.:-]+)/.exec(message);
  if (!m) return message;
  return `You can't assign this role: it includes the permission "${m[1]}", which you don't hold yourself. Ask a hub admin to assign it.`;
}

/** The full desired role set: the built-in role (unless None) plus customs. */
export function selectionToRoleIds(builtInId: string, customIds: readonly string[]): string[] {
  const ids: string[] = [];
  if (builtInId && builtInId !== NO_PROJECT_ROLE) ids.push(builtInId);
  for (const id of customIds) {
    if (id && !ids.includes(id)) ids.push(id);
  }
  return ids;
}

export function isEmptySelection(builtInId: string, customIds: readonly string[]): boolean {
  return selectionToRoleIds(builtInId, customIds).length === 0;
}

/** Order-insensitive comparison of two role-ID sets. */
export function sameRoleSet(a: readonly string[], b: readonly string[]): boolean {
  const sa = new Set(a);
  const sb = new Set(b);
  if (sa.size !== sb.size) return false;
  for (const id of sa) if (!sb.has(id)) return false;
  return true;
}

/** Per-type eligibility of a built-in role; returns the reason it is
 *  ineligible, or '' when the principal type may hold it. */
export function builtInIneligibleReason(roleName: string, principalType: string): string {
  const tier = getRoleTier(roleName);
  if (tier === 'owner' && principalType !== 'user') return 'Owner is only available for users';
  if (tier === 'admin' && principalType === 'agent') return 'Admin is not available for agents';
  return '';
}

/** Whether the actor's capabilities cover a built-in tier. */
export function tierAllowed(tier: RoleTier, caps: MembershipCapabilities | null): boolean {
  if (!caps) return false;
  switch (tier) {
    case 'owner':
      return caps.canManageOwners;
    case 'admin':
      return caps.canManageAdmins;
    case 'member':
      return caps.canManageMembers;
  }
}

export interface BuiltInOptionContext {
  mode: MemberDialogMode;
  caps: MembershipCapabilities | null;
  principalType: string;
  /** The row being edited is the project's last direct owner. */
  isLastOwner: boolean;
  /** The built-in role the principal holds now (NO_PROJECT_ROLE if none). */
  currentBuiltInId: string;
}

/**
 * Enabled/disabled state of one built-in radio. Pass `null` for the None
 * option.
 *
 * assignable-roles is principal-agnostic and evaluated as an add, so its
 * `grantable` flag only disables a built-in role in Add mode. In Edit mode
 * the PUT decides and the dialog shows its error.
 */
export function builtInOptionState(
  role: AssignableProjectRole | null,
  ctx: BuiltInOptionContext
): OptionState {
  const enabled = { disabled: false, reason: '' };
  if (role === null) {
    if (ctx.isLastOwner) return { disabled: true, reason: LAST_OWNER_REASON };
    return enabled;
  }
  if (ctx.mode === 'edit' && role.id === ctx.currentBuiltInId) return enabled;
  const ineligible = builtInIneligibleReason(role.name, ctx.principalType);
  if (ineligible) return { disabled: true, reason: ineligible };
  const tier = getRoleTier(role.name);
  if (ctx.isLastOwner && tier !== 'owner') return { disabled: true, reason: LAST_OWNER_REASON };
  if (!tierAllowed(tier, ctx.caps)) {
    return { disabled: true, reason: builtInTierReason(role, ctx.mode) };
  }
  if (ctx.mode === 'add' && !role.grantable) {
    return { disabled: true, reason: describeCustomRoleError(role.reason || 'Not assignable') };
  }
  return enabled;
}

/**
 * Hub denial codes for a governance refusal: the actor's own tier may not
 * grant the role (pkg/hub checkBuiltInChangeGovernance, ErrCodeRoleAssignmentForbidden
 * and ErrCodeTargetRoleProtected). The hub's reason text for these is a
 * machine string such as
 * `actor role "project-admin" cannot add target role "project-owner"`.
 */
const GOVERNANCE_DENIAL_CODES: readonly string[] = [
  'role_assignment_forbidden',
  'target_role_protected',
];

/**
 * The hub's custom-role authority refusal: role_assignment_forbidden with
 * details.requiredPermission. The custom-roles section caption already states
 * this cause.
 */
function isCustomRoleAuthorityDenial(role: AssignableProjectRole): boolean {
  return role.denialCode === 'role_assignment_forbidden' && !!role.details?.requiredPermission;
}

/**
 * The assignable-roles reason for a role the actor can't newly grant, or ''
 * when none applies. The catalog is evaluated as an add, so its reason only
 * describes Add mode; in Edit mode the PUT decides.
 */
export function addModeRoleReason(role: AssignableProjectRole, mode: MemberDialogMode): string {
  if (mode !== 'add' || role.grantable || !role.reason) return '';
  return describeCustomRoleError(role.reason);
}

/**
 * Reason for a built-in role above the actor's tier. A governance refusal
 * (or one without a denial code) reads as the readable tier text; any other
 * code (such as the credential gate) shows the hub's reason, mapped by
 * describeCustomRoleError.
 *
 * The hub's delegation ceiling (canDelegateRefusalFor) shares
 * target_role_protected, so a ceiling refusal here would also read as the
 * tier text. That is safe only because the hub runs governance before
 * CanDelegate, so a tier-blocked built-in carries the governance code. A
 * ceiling refusal on a tier-allowed role goes through builtInOptionState's
 * !grantable branch instead.
 */
export function builtInTierReason(role: AssignableProjectRole, mode: MemberDialogMode): string {
  // The direct-owner refusal ("only direct project owners can manage ...") is
  // collapsed to TIER_REASON on purpose; D3 makes it effectively unreachable.
  if (!role.denialCode || GOVERNANCE_DENIAL_CODES.includes(role.denialCode)) return TIER_REASON;
  return addModeRoleReason(role, mode) || TIER_REASON;
}

/**
 * Enabled/disabled state of one custom-role checkbox. Without
 * canManageCustomRoles every checkbox is read-only (the section caption
 * explains why); in Add mode an unheld one also carries its assignable-roles
 * reason when that differs from the caption's cause. A held role can always
 * be unchecked; removal needs no delegation ceiling.
 */
export function customRoleState(
  role: AssignableProjectRole,
  opts: { caps: MembershipCapabilities | null; held: boolean; mode?: MemberDialogMode }
): OptionState {
  if (!opts.caps?.canManageCustomRoles) {
    // The section caption states the custom-role authority cause once, so a
    // row repeats only a different cause and otherwise keeps its description.
    if (opts.held || isCustomRoleAuthorityDenial(role)) return { disabled: true, reason: '' };
    return { disabled: true, reason: addModeRoleReason(role, opts.mode ?? 'edit') };
  }
  if (opts.held) return { disabled: false, reason: '' };
  if (!role.grantable) {
    return { disabled: true, reason: describeCustomRoleError(role.reason || 'Not assignable') };
  }
  return { disabled: false, reason: '' };
}

/** Built-in roles from the catalog, in tier order (owner, admin, member). */
export function builtInCatalog(
  assignable: readonly AssignableProjectRole[]
): AssignableProjectRole[] {
  const rank: Record<RoleTier, number> = { owner: 0, admin: 1, member: 2 };
  return assignable
    .filter((r) => (r.roleKind ? r.roleKind === 'builtin' : !isCustomProjectRole(r.name)))
    .sort((a, b) => rank[getRoleTier(a.name)] - rank[getRoleTier(b.name)]);
}

/** Custom roles from the catalog, sorted by name. */
export function customCatalog(
  assignable: readonly AssignableProjectRole[]
): AssignableProjectRole[] {
  return assignable
    .filter((r) => (r.roleKind ? r.roleKind === 'custom' : isCustomProjectRole(r.name)))
    .sort((a, b) => a.name.localeCompare(b.name));
}

/** Add-mode default: Admin when the actor may grant it to this principal
 *  type, else Member, else None. */
export function defaultBuiltInForAdd(
  caps: MembershipCapabilities | null,
  principalType: string,
  assignable: readonly AssignableProjectRole[]
): string {
  const ctx: BuiltInOptionContext = {
    mode: 'add',
    caps,
    principalType,
    isLastOwner: false,
    currentBuiltInId: NO_PROJECT_ROLE,
  };
  const builtIns = builtInCatalog(assignable);
  for (const tier of ['admin', 'member'] as const) {
    const role = builtIns.find((r) => getRoleTier(r.name) === tier);
    if (role && !builtInOptionState(role, ctx).disabled) return role.id;
  }
  return NO_PROJECT_ROLE;
}

/**
 * Whether the "None" built-in radio is offered. None means "custom roles
 * only", so it is shown only when it can be meaningful. In Add mode: the
 * catalog has custom roles and the principal is not an agent (agents get no
 * custom roles in this dialog). In Edit mode: the catalog has custom roles,
 * or the principal holds no built-in role now or holds custom roles.
 */
export function showNoProjectRoleOption(opts: {
  mode: MemberDialogMode;
  principalType: MemberPrincipalType;
  assignable: readonly AssignableProjectRole[];
  currentBuiltInId: string;
  heldCustomCount: number;
}): boolean {
  const hasCustom = customCatalog(opts.assignable).length > 0;
  if (opts.mode === 'add') return hasCustom && opts.principalType !== 'agent';
  return hasCustom || opts.currentBuiltInId === NO_PROJECT_ROLE || opts.heldCustomCount > 0;
}

/** Whether any binding in the view comes from somewhere other than a direct
 *  grant. The Source column is shown only then; while every binding is
 *  direct it would read "Direct" on every row. */
export function hasNonDirectSource(groups: readonly ProjectMemberGroup[]): boolean {
  return groups.some((g) => g.bindings.some(isNonDirectSource));
}

/** Whether a binding comes from somewhere other than a direct grant. An
 *  empty source counts as direct, matching the hub's default. */
export function isNonDirectSource(b: Pick<ProjectMemberBinding, 'source'>): boolean {
  return (b.source || 'direct') !== 'direct';
}

/** The row's built-in binding, if any. */
export function builtInBinding(group: ProjectMemberGroup): ProjectMemberBinding | undefined {
  return group.bindings.find((b) => !isCustomBinding(b));
}

export function customBindings(group: ProjectMemberGroup): ProjectMemberBinding[] {
  return group.bindings.filter((b) => isCustomBinding(b));
}

/** Tier of the row's built-in role; rows with none count as member tier. */
export function rowTier(group: ProjectMemberGroup): RoleTier {
  const b = builtInBinding(group);
  return b ? getRoleTier(b.roleName) : 'member';
}

function isDirectOwnerRow(group: ProjectMemberGroup): boolean {
  return (
    group.principalType === 'user' &&
    group.bindings.some(
      (b) => b.source === 'direct' && !isCustomBinding(b) && getRoleTier(b.roleName) === 'owner'
    )
  );
}

export function isLastDirectOwner(
  group: ProjectMemberGroup,
  groups: readonly ProjectMemberGroup[]
): boolean {
  if (!isDirectOwnerRow(group)) return false;
  return groups.filter(isDirectOwnerRow).length <= 1;
}

/**
 * Tier of a principal known only by its current role IDs (e.g. addressed by
 * email, so no loaded row matches). The built-in role found in the catalog
 * decides; with none, an ID the catalog does not know might be a built-in
 * role, so the result fails closed to the owner tier. Callers check
 * `roleIdsUnclassifiable` first; the owner fallback is a backstop.
 */
export function tierFromRoleIds(
  roleIds: readonly string[],
  assignable: readonly AssignableProjectRole[]
): RoleTier {
  const builtIn = builtInCatalog(assignable).find((r) => roleIds.includes(r.id));
  if (builtIn) return getRoleTier(builtIn.name);
  const known = new Set(assignable.map((r) => r.id));
  return roleIds.some((id) => !known.has(id)) ? 'owner' : 'member';
}

/** Last-owner status for a principal known only by its tier: a direct user
 *  owner is the last one when the loaded rows hold at most one direct owner. */
export function isLastOwnerByTier(
  principalType: string,
  tier: RoleTier,
  groups: readonly ProjectMemberGroup[]
): boolean {
  if (principalType !== 'user' || tier !== 'owner') return false;
  return groups.filter(isDirectOwnerRow).length <= 1;
}

/**
 * Whether a principal's role IDs can't be sorted into built-in and custom
 * roles: the catalog failed to load, or an ID the catalog does not know
 * might be the built-in role.
 */
export function roleIdsUnclassifiable(
  roleIds: readonly string[],
  assignable: readonly AssignableProjectRole[],
  catalogUnavailable: boolean
): boolean {
  if (catalogUnavailable) return true;
  if (builtInCatalog(assignable).some((r) => roleIds.includes(r.id))) return false;
  const known = new Set(assignable.map((r) => r.id));
  return roleIds.some((id) => !known.has(id));
}

export interface DialogLockInput {
  principalType: string;
  /** The loaded row for the target, when there is one. */
  row: ProjectMemberGroup | undefined;
  /** The target's current role IDs (used when there is no loaded row). */
  roleIds: readonly string[];
  caps: MembershipCapabilities | null;
  groups: readonly ProjectMemberGroup[];
  assignable: readonly AssignableProjectRole[];
  catalogUnavailable: boolean;
}

export interface DialogLock {
  /** Why every control is read-only, or null when the target is editable. */
  lockedReason: string | null;
  isLastOwner: boolean;
}

/**
 * Lock and last-owner state of the Edit dialog. A loaded row follows the
 * row pencil rule. A target known only by its role IDs takes its tier from
 * the built-in role among them; when those IDs can't be classified, the
 * dialog is locked for every actor and no last-owner status is inferred.
 */
export function deriveDialogLock(input: DialogLockInput): DialogLock {
  const { row, caps, groups } = input;
  if (row) {
    return {
      lockedReason: canEditRow(row, caps) ? null : ROW_LOCKED_REASON,
      isLastOwner: isLastDirectOwner(row, groups),
    };
  }
  if (roleIdsUnclassifiable(input.roleIds, input.assignable, input.catalogUnavailable)) {
    return { lockedReason: CATALOG_UNAVAILABLE_REASON, isLastOwner: false };
  }
  const tier = tierFromRoleIds(input.roleIds, input.assignable);
  return {
    lockedReason: tierAllowed(tier, caps) ? null : ROW_LOCKED_REASON,
    isLastOwner: isLastOwnerByTier(input.principalType, tier, groups),
  };
}

/** Row Edit: the actor must manage the tier of the row's built-in role.
 *  Custom bindings on the row are carried as Keep when the actor cannot
 *  change them. */
export function canEditRow(
  group: ProjectMemberGroup,
  caps: MembershipCapabilities | null
): boolean {
  return tierAllowed(rowTier(group), caps);
}

/** Row Delete removes every binding, so the actor needs authority over all
 *  of them; the last direct owner can never be removed. */
export function canRemoveRow(
  group: ProjectMemberGroup,
  caps: MembershipCapabilities | null,
  groups: readonly ProjectMemberGroup[]
): OptionState {
  if (isLastDirectOwner(group, groups)) return { disabled: true, reason: LAST_OWNER_REMOVE_REASON };
  if (!tierAllowed(rowTier(group), caps)) {
    return { disabled: true, reason: OWNER_ONLY_REMOVE_REASON };
  }
  if (customBindings(group).length > 0 && !caps?.canManageCustomRoles) {
    return { disabled: true, reason: OWNER_ONLY_REMOVE_REASON };
  }
  return { disabled: false, reason: '' };
}

/** Finds the loaded row for a principal (IDs compare case-insensitively). */
export function findMemberGroup(
  groups: readonly ProjectMemberGroup[],
  principalType: string,
  principalId: string
): ProjectMemberGroup | undefined {
  const id = principalId.trim().toLowerCase();
  if (!id) return undefined;
  return groups.find(
    (g) => g.principalType === principalType && g.principalId.toLowerCase() === id
  );
}

function principalLabel(group: Pick<ProjectMemberGroup, 'principalDisplayName' | 'principalId'>) {
  return group.principalDisplayName || group.principalId;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-project-members-editor')
export class ScionProjectMembersEditor extends LitElement {
  /** The project ID. */
  @property() projectId = '';

  /** Whether the editor is read-only. */
  @property({ type: Boolean }) readOnly = false;

  /** Whether to render in compact card layout. */
  @property({ type: Boolean }) compact = false;

  /** Section title override. */
  @property() sectionTitle = 'Members';

  /** Section description override. */
  @property() sectionDescription = '';

  @state() private loading = true;
  @state() private groups: ProjectMemberGroup[] = [];
  @state() private assignableRoles: AssignableProjectRole[] = [];
  @state() private catalogError: string | null = null;
  @state() private error: string | null = null;

  // Membership capabilities returned by the server — drives per-row
  // visibility of edit/remove buttons and the transfer ownership UI.
  @state() private capabilities: MembershipCapabilities | null = null;

  // Member dialog (Add and Edit modes)
  @state() private dialogOpen = false;
  @state() private dialogMode: MemberDialogMode = 'add';
  @state() private dlgPrincipalType: MemberPrincipalType = 'user';
  @state() private dlgPrincipalId = '';
  @state() private dlgDisplayName = '';
  @state() private dlgBuiltIn = NO_PROJECT_ROLE;
  @state() private dlgCurrentBuiltIn = NO_PROJECT_ROLE;
  /** Set once the user picks a built-in radio; until then Add mode keeps
   *  re-computing its default as the catalog and capabilities arrive. */
  private dlgBuiltInTouched = false;
  /** Role name of dlgCurrentBuiltIn, so it can be shown without a catalog. */
  @state() private dlgCurrentBuiltInName = '';
  @state() private dlgCustomIds: string[] = [];
  @state() private dlgHeldCustom: ProjectMemberBinding[] = [];
  @state() private dlgExpectedIds: string[] = [];
  @state() private dlgIsLastOwner = false;
  /** Set when the dialog switched to Edit for a member the actor may not
   *  edit (same tier rule as the row pencil): every control is read-only. */
  @state() private dlgLockedReason: string | null = null;
  /** Info text shown in place of dlgInfo while the dialog is locked, so it
   *  never invites an edit the locked controls forbid. */
  @state() private dlgLockedInfo: string | null = null;
  @state() private dlgSaving = false;
  @state() private dlgError: string | null = null;
  @state() private dlgErrorRoleId: string | null = null;
  @state() private dlgInfo: string | null = null;

  // Row removal in progress (principal key)
  @state() private removingKey: string | null = null;

  // Transfer ownership dialog state
  @state() private transferDialogOpen = false;
  @state() private transferNewOwnerId = '';
  @state() private transferLoading = false;
  @state() private transferError: string | null = null;

  // Action feedback
  @state() private actionFeedback: { message: string; variant: 'success' | 'danger' } | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1rem;
      gap: 1rem;
    }

    .section-header-info h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section-header-info p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .section-header-actions {
      display: flex;
      gap: 0.5rem;
      align-items: center;
      flex-shrink: 0;
    }

    .member-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      font-weight: 400;
      margin-left: 0.5rem;
    }

    /* Table */
    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .compact .table-container {
      border: none;
      border-radius: 0;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    tr:last-child td {
      border-bottom: none;
    }

    tr:hover td {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    /* Member identity */
    .member-identity {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .member-icon {
      width: 2rem;
      height: 2rem;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }

    .member-icon.user {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-600, #2563eb);
    }

    .member-icon.group {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-600, #d97706);
    }

    .member-icon.agent {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-600, #16a34a);
    }

    .member-icon sl-icon {
      font-size: 0.875rem;
    }

    .member-info {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .member-name {
      font-weight: 500;
      font-size: 0.875rem;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .member-detail {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Role badge */
    .role-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    /* Provenance badge */
    .provenance-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.6875rem;
    }

    .provenance-badge.direct {
      color: var(--sl-color-primary-600, #2563eb);
    }

    .provenance-badge.group-derived {
      color: var(--sl-color-warning-600, #d97706);
    }

    .provenance-badge sl-icon {
      font-size: 0.6875rem;
    }

    .actions-cell {
      text-align: right;
      white-space: nowrap;
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Empty state */
    .empty-state {
      text-align: center;
      padding: 3rem 2rem;
    }

    .compact .empty-state {
      padding: 2rem 1.5rem;
    }

    .empty-state > sl-icon {
      font-size: 3rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 0.75rem;
    }

    .empty-state h3 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.25rem 0;
      font-size: 0.875rem;
    }

    /* Loading / Error */
    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      gap: 0.75rem;
    }

    .error-state {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }

    .feedback-alert {
      margin-bottom: 1rem;
    }

    /* Dialog */
    .form-group {
      margin-bottom: 1rem;
    }

    .form-group:last-child {
      margin-bottom: 0;
    }

    .dialog-error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.5rem 0.75rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .validation-warning {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      background: var(--sl-color-warning-50, #fffbeb);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-warning-700, #b45309);
      font-size: 0.8125rem;
    }

    .validation-warning sl-icon {
      flex-shrink: 0;
    }

    .role-badges {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
    }

    .role-badge.custom {
      background: var(--sl-color-primary-50, #eff6ff);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .no-role {
      font-style: italic;
    }

    .form-label {
      display: block;
      font-size: 0.875rem;
      font-weight: 500;
      margin-bottom: 0.375rem;
    }

    .form-help {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0.25rem 0 0.5rem;
    }

    .dialog-member {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 1rem;
    }

    .role-option {
      display: block;
      margin-bottom: 0.25rem;
    }

    .option-reason {
      display: block;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .option-error {
      outline: 2px solid var(--sl-color-danger-500, #ef4444);
      outline-offset: 2px;
      border-radius: 0.25rem;
    }

    .custom-role-list {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }

    .dialog-info {
      color: var(--sl-color-primary-700, #1d4ed8);
      font-size: 0.875rem;
      padding: 0.5rem 0.75rem;
      margin-bottom: 1rem;
      background: var(--sl-color-primary-50, #eff6ff);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .dialog-info .locked-reason {
      display: block;
      margin-top: 0.25rem;
      font-weight: 600;
    }

    .dialog-actions-inline {
      margin-top: 0.5rem;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }

      .section {
        padding: 1rem;
      }

      th,
      td {
        padding: 0.5rem 0.75rem;
      }
    }
  `;

  /** Guard to prevent double-fetch when connectedCallback and updated both fire. */
  private _initialLoadDone = false;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.projectId) {
      this._initialLoadDone = true;
      void this.loadData();
    }
  }

  override willUpdate(changed: Map<string, unknown>): void {
    this.syncAddDefault(changed);
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('projectId') && this.projectId) {
      // Skip if connectedCallback already triggered the initial load.
      if (!this._initialLoadDone) {
        void this.loadData();
      }
      this._initialLoadDone = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private get membersUrl(): string {
    return `/api/v1/projects/${encodeURIComponent(this.projectId)}/members`;
  }

  private principalUrl(principalType: string, principalId: string): string {
    return `${this.membersUrl}/principals/${encodeURIComponent(principalType)}/${encodeURIComponent(principalId)}`;
  }

  private async loadData(): Promise<void> {
    if (!this.projectId) return;

    this.loading = true;
    this.error = null;

    try {
      const items: ProjectMemberGroup[] = [];
      let rawCaps: Partial<MembershipCapabilities> | undefined;
      let offset = 0;
      for (let page = 0; page < MAX_MEMBER_PAGES; page++) {
        const res = await apiFetch(
          `${this.membersUrl}?groupBy=principal&limit=${MEMBERS_PAGE_SIZE}&offset=${offset}`
        );
        if (!res.ok) {
          throw new Error(await extractApiError(res, `HTTP ${res.status}`));
        }
        const data = (await res.json()) as {
          items?: ProjectMemberGroup[];
          totalCount?: number;
          _capabilities?: Partial<MembershipCapabilities>;
        };
        if (page === 0) rawCaps = data._capabilities;
        const pageItems = data.items ?? [];
        items.push(...pageItems);
        offset += pageItems.length;
        const total = data.totalCount ?? items.length;
        if (pageItems.length === 0 || items.length >= total) break;
      }

      this.groups = items.map((g) => ({
        ...g,
        bindings: (g.bindings ?? []).map((b) => ({ ...b, source: b.source || 'direct' })),
      }));

      // When _capabilities is absent (older server), default to null
      // which makes effectiveReadOnly true (fail-closed).
      this.capabilities = rawCaps
        ? {
            canManageMembers: rawCaps.canManageMembers ?? false,
            canManageAdmins: rawCaps.canManageAdmins ?? false,
            canManageOwners: rawCaps.canManageOwners ?? false,
            canTransfer: rawCaps.canTransfer ?? false,
            canManageCustomRoles: rawCaps.canManageCustomRoles ?? false,
            actions: rawCaps.actions ?? [],
          }
        : null;
    } catch (err) {
      console.error('Failed to load project members:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load project members';
      this.loading = false;
      return;
    }

    // The role catalog is gated by project.manage, so it is fetched only
    // once the capabilities say the editor is editable (no 403 for viewers).
    if (!this.effectiveReadOnly) {
      await this.loadAssignableRoles();
    } else {
      this.assignableRoles = [];
      this.catalogError = null;
    }
    this.loading = false;
  }

  private async loadAssignableRoles(): Promise<void> {
    try {
      const res = await apiFetch(`${this.membersUrl}/assignable-roles`, {
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as { items?: AssignableProjectRole[] };
      this.assignableRoles = data.items ?? [];
      this.catalogError = null;
    } catch (err) {
      console.error('Failed to load assignable roles:', err);
      this.assignableRoles = [];
      this.catalogError = err instanceof Error ? err.message : 'Failed to load roles';
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  /** Effective read-only: true when the parent says read-only OR the server
   *  advisory indicates the current user cannot manage any membership tier.
   *  canManageCustomRoles alone (a hub-override actor) does not make the
   *  editor editable. */
  private get effectiveReadOnly(): boolean {
    if (this.readOnly) return true;
    if (!this.capabilities) return true; // fail-closed when capabilities absent
    return !(
      this.capabilities.canManageMembers ||
      this.capabilities.canManageAdmins ||
      this.capabilities.canManageOwners
    );
  }

  private get selectedRoleIds(): string[] {
    return selectionToRoleIds(this.dlgBuiltIn, this.dlgCustomIds);
  }

  private get dialogRow(): ProjectMemberGroup | undefined {
    return findMemberGroup(this.groups, this.dlgPrincipalType, this.dlgPrincipalId);
  }

  /** Whether the custom-role checkboxes may change in this dialog. */
  private get customRolesEditable(): boolean {
    return (
      !this.dlgLockedReason &&
      !!this.capabilities?.canManageCustomRoles &&
      this.dlgPrincipalType !== 'agent'
    );
  }

  private builtInContext(): BuiltInOptionContext {
    return {
      mode: this.dialogMode,
      caps: this.capabilities,
      principalType: this.dlgPrincipalType,
      isLastOwner: this.dlgIsLastOwner,
      currentBuiltInId: this.dlgCurrentBuiltIn,
    };
  }

  private get noProjectRoleShown(): boolean {
    return showNoProjectRoleOption({
      mode: this.dialogMode,
      principalType: this.dlgPrincipalType,
      assignable: this.assignableRoles,
      currentBuiltInId: this.dlgCurrentBuiltIn,
      heldCustomCount: this.dlgHeldCustom.length,
    });
  }

  /**
   * Add mode can open before the role catalog has loaded (the Add button
   * appears as soon as the capabilities arrive), when the only possible
   * default is None. While the user has not picked a radio, the default is
   * re-computed whenever the catalog or capabilities change; a hidden None
   * is never left selected.
   */
  private syncAddDefault(changed: Map<string, unknown>): void {
    // The lock check is defensive: Add mode is never locked today.
    if (!this.dialogOpen || this.dialogMode !== 'add' || this.dlgLockedReason) return;
    const inputsChanged = changed.has('assignableRoles') || changed.has('capabilities');
    const hiddenNone = this.dlgBuiltIn === NO_PROJECT_ROLE && !this.noProjectRoleShown;
    if ((inputsChanged && !this.dlgBuiltInTouched) || hiddenNone) {
      this.dlgBuiltIn = defaultBuiltInForAdd(
        this.capabilities,
        this.dlgPrincipalType,
        this.assignableRoles
      );
    }
  }

  private roleIdKind(id: string): 'builtin' | 'custom' | 'unknown' {
    const role = this.assignableRoles.find((r) => r.id === id);
    if (!role) return 'unknown';
    if (role.roleKind) return role.roleKind;
    return isCustomProjectRole(role.name) ? 'custom' : 'builtin';
  }

  // ---------------------------------------------------------------------------
  // Dialog state
  // ---------------------------------------------------------------------------

  private resetDialogMessages(): void {
    this.dlgError = null;
    this.dlgErrorRoleId = null;
    this.dlgInfo = null;
  }

  private openAddDialog(): void {
    this.dialogMode = 'add';
    this.dlgPrincipalType = 'user';
    this.dlgPrincipalId = '';
    this.dlgDisplayName = '';
    this.dlgCurrentBuiltIn = NO_PROJECT_ROLE;
    this.dlgCurrentBuiltInName = '';
    this.dlgBuiltIn = defaultBuiltInForAdd(this.capabilities, 'user', this.assignableRoles);
    this.dlgBuiltInTouched = false;
    this.dlgCustomIds = [];
    this.dlgHeldCustom = [];
    this.dlgExpectedIds = [];
    this.dlgIsLastOwner = false;
    this.dlgLockedReason = null;
    this.dlgLockedInfo = null;
    this.resetDialogMessages();
    this.dialogOpen = true;
  }

  /** Recomputes the Edit dialog's lock and last-owner state from the loaded
   *  rows, capabilities and catalog. While locked, the info text is the
   *  locked variant. */
  private applyDialogLock(row: ProjectMemberGroup | undefined): void {
    const lock = deriveDialogLock({
      principalType: this.dlgPrincipalType,
      row,
      roleIds: this.dlgExpectedIds,
      caps: this.capabilities,
      groups: this.groups,
      assignable: this.assignableRoles,
      catalogUnavailable: !!this.catalogError,
    });
    this.dlgLockedReason = lock.lockedReason;
    this.dlgIsLastOwner = lock.isLastOwner;
    if (lock.lockedReason) this.dlgInfo = this.dlgLockedInfo;
  }

  /** Switches the dialog to Edit mode for a loaded row, pre-filled with the
   *  principal's current roles. Read-only when the actor may not edit the
   *  row (reachable when Add mode lands on an existing member); lockedInfo
   *  then replaces info. */
  private openEditDialog(
    group: ProjectMemberGroup,
    info: string | null = null,
    lockedInfo: string | null = null
  ): void {
    const builtIn = builtInBinding(group);
    const held = customBindings(group);
    this.dialogMode = 'edit';
    this.dlgPrincipalType = group.principalType as MemberPrincipalType;
    this.dlgPrincipalId = group.principalId;
    this.dlgDisplayName = principalLabel(group);
    this.dlgCurrentBuiltIn = builtIn?.roleDefinitionId ?? NO_PROJECT_ROLE;
    this.dlgCurrentBuiltInName = builtIn?.roleName ?? '';
    this.dlgBuiltIn = this.dlgCurrentBuiltIn;
    this.dlgHeldCustom = held;
    this.dlgCustomIds = held.map((b) => b.roleDefinitionId);
    this.dlgExpectedIds = group.bindings.map((b) => b.roleDefinitionId);
    this.resetDialogMessages();
    this.dlgInfo = info;
    this.dlgLockedInfo = lockedInfo;
    this.applyDialogLock(group);
    this.dialogOpen = true;
  }

  /** Edit mode for a principal the loaded rows do not contain (e.g. it was
   *  addressed by email), built from the server's current role IDs. The tier
   *  and last-owner rules match openEditDialog, with the tier inferred from
   *  the built-in role in roleIds. IDs that can't be classified (no catalog,
   *  or an unknown ID and no built-in role) are kept as the selection but
   *  not shown, and the dialog is locked. */
  private openEditFromRoleIds(
    principalType: MemberPrincipalType,
    principalId: string,
    displayName: string,
    roleIds: string[],
    info: string,
    lockedInfo: string
  ): void {
    const classify = !roleIdsUnclassifiable(roleIds, this.assignableRoles, !!this.catalogError);
    const builtIn = classify ? roleIds.find((id) => this.roleIdKind(id) === 'builtin') : undefined;
    const customIds = roleIds.filter((id) => id !== builtIn);
    this.dialogMode = 'edit';
    this.dlgPrincipalType = principalType;
    this.dlgPrincipalId = principalId;
    this.dlgDisplayName = displayName || principalId;
    this.dlgCurrentBuiltIn = builtIn ?? NO_PROJECT_ROLE;
    this.dlgCurrentBuiltInName = this.assignableRoles.find((r) => r.id === builtIn)?.name ?? '';
    this.dlgBuiltIn = this.dlgCurrentBuiltIn;
    this.dlgHeldCustom = (classify ? customIds : []).map((id) => {
      const role = this.assignableRoles.find((r) => r.id === id);
      return {
        id: '',
        roleDefinitionId: id,
        roleName: role?.name ?? id,
        principalType,
        principalId,
        scopeType: 'project',
        scopeId: this.projectId,
        createdAt: '',
        source: 'direct',
        roleKind: 'custom',
      };
    });
    this.dlgCustomIds = customIds;
    this.dlgExpectedIds = [...roleIds];
    this.resetDialogMessages();
    this.dlgInfo = info;
    this.dlgLockedInfo = lockedInfo;
    this.applyDialogLock(undefined);
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    if (this.dlgSaving) return;
    this.dialogOpen = false;
  }

  private onPrincipalTypeChange(type: MemberPrincipalType): void {
    this.dlgPrincipalType = type;
    this.dlgPrincipalId = '';
    this.dlgDisplayName = '';
    this.dlgBuiltIn = defaultBuiltInForAdd(this.capabilities, type, this.assignableRoles);
    this.dlgBuiltInTouched = false;
    this.dlgCustomIds = [];
    this.resetDialogMessages();
  }

  /** Picking a principal who is already a member switches to Edit. Picker
   *  events that arrive once the dialog is in Edit mode (the picker's
   *  debounced search can fire late) never change the Edit target. */
  private onPrincipalChange(detail: PrincipalChangeDetail): void {
    if (this.dialogMode !== 'add') return;
    this.dlgPrincipalId = detail.principalId;
    this.dlgDisplayName = detail.displayLabel;
    const existing = findMemberGroup(this.groups, this.dlgPrincipalType, detail.principalId);
    if (existing) {
      const label = principalLabel(existing);
      this.openEditDialog(
        existing,
        `${label} is already a member. Editing their roles instead.`,
        `${label} is already a member.`
      );
    }
  }

  private toggleCustomRole(id: string, checked: boolean): void {
    if (checked) {
      if (!this.dlgCustomIds.includes(id)) this.dlgCustomIds = [...this.dlgCustomIds, id];
    } else {
      this.dlgCustomIds = this.dlgCustomIds.filter((x) => x !== id);
    }
  }

  // ---------------------------------------------------------------------------
  // Save (one PUT per save)
  // ---------------------------------------------------------------------------

  private async handleSave(): Promise<void> {
    const roleIds = this.selectedRoleIds;
    const principalType = this.dlgPrincipalType;
    const principalId = this.dlgPrincipalId.trim();
    if (roleIds.length === 0 || !principalId || this.dlgSaving || this.dlgLockedReason) return;

    const mode = this.dialogMode;
    this.dlgSaving = true;
    this.resetDialogMessages();

    try {
      const res = await apiFetch(this.principalUrl(principalType, principalId), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          roleDefinitionIds: roleIds,
          expectedRoleDefinitionIds: mode === 'add' ? [] : this.dlgExpectedIds,
        }),
        suppressAccessDeniedToast: true,
      });

      if (res.ok) {
        dispatchMembershipChanged({ kind: 'project', id: this.projectId });
        this.dialogOpen = false;
        this.actionFeedback = {
          message: mode === 'add' ? 'Member added' : 'Roles updated',
          variant: 'success',
        };
        void this.loadData();
        return;
      }

      const info = await parseApiError(res, `HTTP ${res.status}`);
      await this.handleSaveError(res.status, info, mode, principalType, principalId);
    } catch (err) {
      console.error('Failed to save member roles:', err);
      this.dlgError = err instanceof Error ? err.message : 'Failed to save member roles';
    } finally {
      this.dlgSaving = false;
    }
  }

  private async handleSaveError(
    status: number,
    err: ApiErrorInfo,
    mode: MemberDialogMode,
    principalType: MemberPrincipalType,
    principalId: string
  ): Promise<void> {
    const details = err.details ?? {};
    const detailRoleId =
      typeof details.roleDefinitionId === 'string' ? details.roleDefinitionId : null;

    if (err.code === 'membership_changed') {
      if (details.cause === 'actor_authority_changed') {
        await this.loadData();
        this.rederiveAfterAuthorityChange();
        return;
      }
      // principal_roles_changed (or an older server without a cause): the
      // principal's role set differs from what this dialog started from.
      const displayName = this.dlgDisplayName;
      await this.loadData();
      const name = displayName || principalId;
      const message =
        mode === 'add'
          ? `${name} is already a member. Their current roles are shown; review and save again.`
          : CHANGED_WHILE_EDITING_MESSAGE;
      const lockedMessage =
        mode === 'add' ? `${name} is already a member.` : CHANGED_WHILE_EDITING_LOCKED_MESSAGE;
      const row = findMemberGroup(this.groups, principalType, principalId);
      if (row) {
        this.openEditDialog(row, message, lockedMessage);
      } else {
        const current = Array.isArray(details.currentRoleDefinitionIds)
          ? (details.currentRoleDefinitionIds as unknown[]).filter(
              (x): x is string => typeof x === 'string'
            )
          : [];
        this.openEditFromRoleIds(
          principalType,
          principalId,
          displayName,
          current,
          message,
          lockedMessage
        );
      }
      return;
    }

    if (err.code === 'invalid_role_set') {
      // A role in the set no longer exists (possibly deleted mid-request).
      await this.loadAssignableRoles();
      const known = new Set(this.assignableRoles.map((r) => r.id));
      const held = new Set(this.dlgHeldCustom.map((b) => b.roleDefinitionId));
      this.dlgCustomIds = this.dlgCustomIds.filter((id) => known.has(id) || held.has(id));
      if (this.dlgBuiltIn !== NO_PROJECT_ROLE && !known.has(this.dlgBuiltIn)) {
        this.dlgBuiltIn = this.dlgCurrentBuiltIn;
      }
      this.dlgError = err.message;
      this.dlgInfo = CATALOG_REFRESHED_MESSAGE;
      this.dlgErrorRoleId = detailRoleId;
      return;
    }

    if (status === 403) {
      const reason = typeof details.reason === 'string' ? details.reason : '';
      const mapped = describeCustomRoleError(err.message);
      this.dlgError = mapped !== err.message || !reason ? mapped : describeCustomRoleError(reason);
      this.dlgErrorRoleId = detailRoleId;
      return;
    }

    this.dlgError = err.message;
    this.dlgErrorRoleId = detailRoleId;
  }

  /**
   * After the actor's own authority changed mid-save (capabilities and the
   * catalog are already reloaded), re-derives the dialog: it closes when the
   * editor became read-only; otherwise Edit mode recomputes the lock and
   * last-owner state, and selections the actor can no longer make are
   * dropped. A locked dialog goes back to the principal's current roles.
   */
  private rederiveAfterAuthorityChange(): void {
    if (this.effectiveReadOnly) {
      this.dialogOpen = false;
      this.actionFeedback = { message: AUTHORITY_REDUCED_MESSAGE, variant: 'danger' };
      return;
    }
    if (this.dialogMode === 'edit') {
      this.applyDialogLock(this.dialogRow);
      if (this.dlgLockedReason) {
        this.dlgBuiltIn = this.dlgCurrentBuiltIn;
        this.dlgCustomIds = this.dlgExpectedIds.filter((id) => id !== this.dlgCurrentBuiltIn);
        this.dlgError = AUTHORITY_REDUCED_MESSAGE;
        return;
      }
    }

    const held = new Set(this.dlgHeldCustom.map((b) => b.roleDefinitionId));
    this.dlgCustomIds = this.dlgCustomIds.filter((id) => {
      if (held.has(id)) return true;
      const role = this.assignableRoles.find((r) => r.id === id);
      return (
        !!role &&
        this.dlgPrincipalType !== 'agent' &&
        !customRoleState(role, { caps: this.capabilities, held: false }).disabled
      );
    });

    // A built-in choice is kept while its option is still enabled; one the
    // refreshed catalog no longer lists can't be checked, so it is reset too.
    const builtInRole =
      this.dlgBuiltIn === NO_PROJECT_ROLE
        ? null
        : this.assignableRoles.find((r) => r.id === this.dlgBuiltIn);
    const builtInAllowed =
      builtInRole !== undefined && !builtInOptionState(builtInRole, this.builtInContext()).disabled;
    if (!builtInAllowed) {
      this.dlgBuiltIn =
        this.dialogMode === 'add'
          ? defaultBuiltInForAdd(this.capabilities, this.dlgPrincipalType, this.assignableRoles)
          : this.dlgCurrentBuiltIn;
    }
    this.dlgError = AUTHORITY_CHANGED_MESSAGE;
  }

  // ---------------------------------------------------------------------------
  // Remove (DELETE-all for a principal)
  // ---------------------------------------------------------------------------

  /** Sends one DELETE for the principal; returns an error message or null. */
  private async deletePrincipal(
    principalType: string,
    principalId: string
  ): Promise<string | null> {
    try {
      const res = await apiFetch(this.principalUrl(principalType, principalId), {
        method: 'DELETE',
        suppressAccessDeniedToast: true,
      });
      if (res.ok) {
        dispatchMembershipChanged({ kind: 'project', id: this.projectId });
        return null;
      }
      const info = await parseApiError(res, `HTTP ${res.status}`);
      return describeCustomRoleError(info.message);
    } catch (err) {
      console.error('Failed to remove member:', err);
      return err instanceof Error ? err.message : 'Failed to remove member';
    }
  }

  private async handleRemoveRow(group: ProjectMemberGroup): Promise<void> {
    const state = canRemoveRow(group, this.capabilities, this.groups);
    if (state.disabled) {
      this.actionFeedback = { message: state.reason, variant: 'danger' };
      return;
    }
    const label = principalLabel(group);
    if (
      !(await showConfirm(
        `Remove ${group.principalType} "${label}" from this project? ${REMOVES_ALL_ROLES}`
      ))
    ) {
      return;
    }

    const key = `${group.principalType}:${group.principalId}`;
    this.removingKey = key;
    const failure = await this.deletePrincipal(group.principalType, group.principalId);
    this.removingKey = null;
    if (failure) {
      this.actionFeedback = { message: failure, variant: 'danger' };
      return;
    }
    this.actionFeedback = { message: 'Member removed', variant: 'success' };
    void this.loadData();
  }

  /** Edit mode with an empty selection: remove the member entirely. */
  private async handleRemoveFromDialog(): Promise<void> {
    if (this.dialogMode !== 'edit' || this.dlgSaving || this.dlgLockedReason) return;
    const label = this.dlgDisplayName || this.dlgPrincipalId;
    if (
      !(await showConfirm(
        `Remove ${this.dlgPrincipalType} "${label}" from this project? ${REMOVES_ALL_ROLES}`
      ))
    ) {
      return;
    }
    this.dlgSaving = true;
    this.resetDialogMessages();
    const failure = await this.deletePrincipal(this.dlgPrincipalType, this.dlgPrincipalId.trim());
    this.dlgSaving = false;
    if (failure) {
      this.dlgError = failure;
      return;
    }
    this.dialogOpen = false;
    this.actionFeedback = { message: 'Member removed', variant: 'success' };
    void this.loadData();
  }

  // ---------------------------------------------------------------------------
  // Transfer ownership
  // ---------------------------------------------------------------------------

  private openTransferDialog(): void {
    this.transferNewOwnerId = '';
    this.transferError = null;
    this.transferDialogOpen = true;
  }

  private async handleTransferOwnership(): Promise<void> {
    if (!this.transferNewOwnerId.trim()) {
      this.transferError = 'Please enter a user ID or email';
      return;
    }

    this.transferLoading = true;
    this.transferError = null;

    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/transfer-ownership`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            newOwnerId: this.transferNewOwnerId.trim(),
          }),
          suppressAccessDeniedToast: true,
        }
      );

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      dispatchMembershipChanged({ kind: 'project', id: this.projectId });
      this.transferDialogOpen = false;
      this.actionFeedback = {
        message: 'Ownership transferred successfully',
        variant: 'success',
      };
      void this.loadData();
    } catch (err) {
      console.error('Failed to transfer ownership:', err);
      this.transferError = err instanceof Error ? err.message : 'Failed to transfer ownership';
    } finally {
      this.transferLoading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderStandalone();
  }

  private renderStandalone() {
    return html`
      ${this.renderFeedback()}
      <div class="section-header">
        <div class="section-header-info">
          <h2>
            ${this.sectionTitle}
            <span class="member-count">(${this.groups.length})</span>
          </h2>
          ${this.sectionDescription ? html`<p>${this.sectionDescription}</p>` : nothing}
        </div>
        <div class="section-header-actions">
          ${this.capabilities?.canTransfer
            ? html`
                <sl-button
                  variant="warning"
                  size="small"
                  outline
                  @click=${() => this.openTransferDialog()}
                >
                  <sl-icon slot="prefix" name="arrow-left-right"></sl-icon>
                  Transfer Ownership
                </sl-button>
              `
            : nothing}
          ${!this.effectiveReadOnly
            ? html`
                <sl-button variant="primary" size="small" @click=${() => this.openAddDialog()}>
                  <sl-icon slot="prefix" name="person-plus"></sl-icon>
                  Add Member
                </sl-button>
              `
            : nothing}
        </div>
      </div>
      ${this.renderBody()} ${this.renderMemberDialog()} ${this.renderTransferDialog()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        ${this.renderFeedback()}
        <div class="section-header">
          <div class="section-header-info">
            <h2>
              ${this.sectionTitle}
              <span class="member-count">(${this.groups.length})</span>
            </h2>
            ${this.sectionDescription ? html`<p>${this.sectionDescription}</p>` : nothing}
          </div>
          <div class="section-header-actions">
            ${this.capabilities?.canTransfer
              ? html`
                  <sl-button
                    variant="warning"
                    size="small"
                    outline
                    @click=${() => this.openTransferDialog()}
                  >
                    <sl-icon slot="prefix" name="arrow-left-right"></sl-icon>
                    Transfer Ownership
                  </sl-button>
                `
              : nothing}
            ${!this.effectiveReadOnly
              ? html`
                  <sl-button size="small" variant="default" @click=${() => this.openAddDialog()}>
                    <sl-icon slot="prefix" name="person-plus"></sl-icon>
                    Add Member
                  </sl-button>
                `
              : nothing}
          </div>
        </div>
        ${this.renderBody()} ${this.renderMemberDialog()} ${this.renderTransferDialog()}
      </div>
    `;
  }

  private renderFeedback() {
    if (!this.actionFeedback) return nothing;
    return html`
      <sl-alert
        class="feedback-alert"
        variant=${this.actionFeedback.variant}
        open
        closable
        duration="5000"
        @sl-after-hide=${() => {
          this.actionFeedback = null;
        }}
      >
        <sl-icon
          slot="icon"
          name=${this.actionFeedback.variant === 'success'
            ? 'check-circle'
            : 'exclamation-triangle'}
        ></sl-icon>
        ${this.actionFeedback.message}
      </sl-alert>
    `;
  }

  private renderBody() {
    if (this.loading) {
      return html` <div class="loading-state"><sl-spinner></sl-spinner> Loading members...</div> `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <span>${this.error}</span>
          <sl-button size="small" @click=${() => this.loadData()}> Retry </sl-button>
        </div>
      `;
    }

    if (this.groups.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="people"></sl-icon>
          <h3>No Members</h3>
          <p>Add members to grant access to this project.</p>
          ${!this.effectiveReadOnly
            ? html`
                <sl-button variant="primary" size="small" @click=${() => this.openAddDialog()}>
                  <sl-icon slot="prefix" name="person-plus"></sl-icon>
                  Add Member
                </sl-button>
              `
            : nothing}
        </div>
      `;
    }

    return this.renderMembersTable();
  }

  private renderMembersTable() {
    const showSource = hasNonDirectSource(this.groups);
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Member</th>
              <th>Roles</th>
              ${showSource ? html`<th class="hide-mobile">Source</th>` : nothing}
              ${!this.effectiveReadOnly ? html`<th class="actions-cell">Actions</th>` : nothing}
            </tr>
          </thead>
          <tbody>
            ${this.groups.map((group) => this.renderMemberRow(group, showSource))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRoleBadges(group: ProjectMemberGroup) {
    const builtIn = builtInBinding(group);
    const custom = customBindings(group);
    return html`
      <div class="role-badges">
        ${builtIn
          ? html`<span class="role-badge">${builtIn.roleName}</span>`
          : html`<span class="meta-text no-role">No project role</span>`}
        ${custom.map((b) => html`<span class="role-badge custom">${b.roleName}</span>`)}
      </div>
    `;
  }

  private renderMemberRow(group: ProjectMemberGroup, showSource: boolean) {
    const key = `${group.principalType}:${group.principalId}`;
    const isRemoving = this.removingKey === key;
    const inherited = group.bindings.find(isNonDirectSource);
    const lastOwner = isLastDirectOwner(group, this.groups);
    const editable = canEditRow(group, this.capabilities);
    const remove = canRemoveRow(group, this.capabilities, this.groups);
    // The shield-lock icon already explains the last-owner case.
    const removeTip = lastOwner ? '' : remove.reason;

    return html`
      <tr data-principal=${key}>
        <td>
          <div class="member-identity">
            <div class="member-icon ${group.principalType}">
              <sl-icon name="${getPrincipalIcon(group.principalType)}"></sl-icon>
            </div>
            <div class="member-info">
              <span class="member-name">${principalLabel(group)}</span>
              <span class="member-detail">${group.principalType}</span>
            </div>
          </div>
        </td>
        <td>${this.renderRoleBadges(group)}</td>
        ${showSource
          ? html`<td class="hide-mobile">
              <span class="provenance-badge ${inherited ? 'group-derived' : 'direct'}">
                ${inherited
                  ? html`<sl-icon name="diagram-3"></sl-icon> Via group:
                      ${inherited.sourceGroupName || inherited.source}`
                  : html`<sl-icon name="person-check"></sl-icon> Direct`}
              </span>
            </td>`
          : nothing}
        ${!this.effectiveReadOnly
          ? html`
              <td class="actions-cell">
                ${editable
                  ? html`
                      <sl-icon-button
                        name="pencil"
                        label="Edit roles"
                        ?disabled=${isRemoving}
                        @click=${() => this.openEditDialog(group)}
                      ></sl-icon-button>
                    `
                  : nothing}
                ${editable
                  ? html`
                      <sl-tooltip content=${removeTip} ?disabled=${!removeTip}>
                        <sl-icon-button
                          name="trash"
                          label="Remove member"
                          ?disabled=${isRemoving || remove.disabled}
                          @click=${() => this.handleRemoveRow(group)}
                        ></sl-icon-button>
                      </sl-tooltip>
                    `
                  : nothing}
                ${lastOwner
                  ? html`<sl-tooltip content=${LAST_OWNER_REMOVE_REASON}>
                      <sl-icon
                        name="shield-lock"
                        style="color: var(--sl-color-warning-500)"
                      ></sl-icon>
                    </sl-tooltip>`
                  : nothing}
              </td>
            `
          : nothing}
      </tr>
    `;
  }

  // ---------------------------------------------------------------------------
  // Member dialog
  // ---------------------------------------------------------------------------

  private renderBuiltInRadios() {
    const builtIns = builtInCatalog(this.assignableRoles);
    // A held built-in role missing from the catalog (e.g. the catalog failed
    // to load) is still shown, so the dialog never hides a role it keeps.
    if (
      this.dialogMode === 'edit' &&
      this.dlgCurrentBuiltIn !== NO_PROJECT_ROLE &&
      this.dlgCurrentBuiltInName &&
      !builtIns.some((r) => r.id === this.dlgCurrentBuiltIn)
    ) {
      builtIns.push({
        id: this.dlgCurrentBuiltIn,
        name: this.dlgCurrentBuiltInName,
        description: '',
        roleKind: 'builtin',
        grantable: false,
        reason: '',
      });
    }
    if (builtIns.length === 0) return nothing;
    const ctx = this.builtInContext();
    const locked = !!this.dlgLockedReason;
    const optionState = (role: AssignableProjectRole | null): OptionState =>
      locked ? { disabled: true, reason: '' } : builtInOptionState(role, ctx);
    const labels: Record<RoleTier, string> = { owner: 'Owner', admin: 'Admin', member: 'Member' };
    const option = (value: string, label: string, state: OptionState, description = '') => html`
      <sl-radio
        class="role-option ${this.dlgErrorRoleId === value ? 'option-error' : ''}"
        value=${value}
        ?disabled=${state.disabled}
      >
        ${label}
        ${description && !state.reason
          ? html`<span class="option-reason">${description}</span>`
          : nothing}
        ${state.reason ? html`<span class="option-reason">${state.reason}</span>` : nothing}
      </sl-radio>
    `;
    return html`
      <div class="form-group">
        <sl-radio-group
          label="Project role"
          name="project-role"
          .value=${this.dlgBuiltIn}
          @sl-change=${(e: Event) => {
            this.dlgBuiltIn = (e.target as HTMLInputElement).value;
            this.dlgBuiltInTouched = true;
          }}
        >
          ${builtIns.map((role) =>
            option(role.id, labels[getRoleTier(role.name)], optionState(role), role.description)
          )}
          ${this.noProjectRoleShown
            ? option(
                NO_PROJECT_ROLE,
                'None',
                optionState(null),
                'No built-in role; custom roles only'
              )
            : nothing}
        </sl-radio-group>
      </div>
    `;
  }

  private renderCustomRoles() {
    const held = new Set(this.dlgHeldCustom.map((b) => b.roleDefinitionId));

    // Agents get no new custom roles here. Held ones are shown read-only and
    // kept in the PUT set.
    if (this.dlgPrincipalType === 'agent') {
      if (this.dlgHeldCustom.length === 0) return nothing;
      return html`
        <div class="form-group custom-roles">
          <span class="form-label">Custom roles</span>
          <p class="form-help">
            Custom roles can't be granted to agents here; the ones this agent holds are kept.
          </p>
          <div class="role-badges">
            ${this.dlgHeldCustom.map(
              (b) => html`<span class="role-badge custom">${b.roleName}</span>`
            )}
          </div>
        </div>
      `;
    }

    const catalog = customCatalog(this.assignableRoles);
    const roles: AssignableProjectRole[] = [...catalog];
    for (const b of this.dlgHeldCustom) {
      if (!roles.some((r) => r.id === b.roleDefinitionId)) {
        roles.push({
          id: b.roleDefinitionId,
          name: b.roleName,
          description: '',
          roleKind: 'custom',
          grantable: false,
          reason: '',
        });
      }
    }
    if (roles.length === 0) return nothing;

    const editable = this.customRolesEditable;
    return html`
      <div class="form-group custom-roles">
        <span class="form-label">Custom roles</span>
        ${!editable && !this.dlgLockedReason
          ? html`<p class="form-help">${CUSTOM_TIER_CAPTION}</p>`
          : nothing}
        <div class="custom-role-list">
          ${roles.map((role) => {
            const state: OptionState = this.dlgLockedReason
              ? { disabled: true, reason: '' }
              : customRoleState(role, {
                  caps: this.capabilities,
                  held: held.has(role.id),
                  mode: this.dialogMode,
                });
            return html`
              <sl-checkbox
                class=${this.dlgErrorRoleId === role.id ? 'option-error' : ''}
                data-role-id=${role.id}
                ?checked=${this.dlgCustomIds.includes(role.id)}
                ?disabled=${state.disabled}
                @sl-change=${(e: Event) =>
                  this.toggleCustomRole(role.id, (e.target as HTMLInputElement).checked)}
              >
                ${role.name}
                ${state.reason
                  ? html`<span class="option-reason">${state.reason}</span>`
                  : role.description
                    ? html`<span class="option-reason">${role.description}</span>`
                    : nothing}
              </sl-checkbox>
            `;
          })}
        </div>
      </div>
    `;
  }

  private renderEmptySelection() {
    if (!isEmptySelection(this.dlgBuiltIn, this.dlgCustomIds)) return nothing;
    const row = this.dialogMode === 'edit' ? this.dialogRow : undefined;
    const remove = row ? canRemoveRow(row, this.capabilities, this.groups) : null;
    return html`
      <div class="form-group">
        <div class="validation-warning">
          <sl-icon name="info-circle"></sl-icon>
          ${EMPTY_SELECTION_MESSAGE}
        </div>
        ${this.dialogMode === 'edit'
          ? html`
              <div class="dialog-actions-inline">
                <sl-button
                  class="remove-member"
                  variant="danger"
                  size="small"
                  outline
                  ?disabled=${this.dlgSaving || !!remove?.disabled || !!this.dlgLockedReason}
                  @click=${() => this.handleRemoveFromDialog()}
                  >Remove member</sl-button
                >
                ${remove?.reason
                  ? html`<span class="option-reason">${remove.reason}</span>`
                  : nothing}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderMemberDialog() {
    if (!this.dialogOpen) return nothing;

    const isAdd = this.dialogMode === 'add';
    const empty = isEmptySelection(this.dlgBuiltIn, this.dlgCustomIds);
    const unchanged = !isAdd && sameRoleSet(this.selectedRoleIds, this.dlgExpectedIds);
    const saveDisabled =
      empty || !this.dlgPrincipalId.trim() || unchanged || !!this.dlgLockedReason;

    return html`
      <sl-dialog
        label=${isAdd ? 'Add Project Member' : 'Edit Member Roles'}
        open
        @sl-request-close=${() => this.closeDialog()}
      >
        ${this.dlgInfo || this.dlgLockedReason
          ? html`<div class="dialog-info">
              ${this.dlgInfo ?? ''}
              ${this.dlgLockedReason
                ? html`<span class="locked-reason">${this.dlgLockedReason}</span>`
                : nothing}
            </div>`
          : nothing}
        ${isAdd
          ? html`
              <div class="form-group">
                <sl-select
                  label="Member Type"
                  hoist
                  .value=${this.dlgPrincipalType}
                  @sl-change=${(e: Event) =>
                    this.onPrincipalTypeChange(
                      (e.target as HTMLSelectElement).value as MemberPrincipalType
                    )}
                >
                  <sl-option value="user">
                    <sl-icon slot="prefix" name="person"></sl-icon>
                    User
                  </sl-option>
                  <sl-option value="agent">
                    <sl-icon slot="prefix" name="cpu"></sl-icon>
                    Agent
                  </sl-option>
                  <sl-option value="group">
                    <sl-icon slot="prefix" name="diagram-3"></sl-icon>
                    Group
                  </sl-option>
                </sl-select>
              </div>

              <div class="form-group">
                <scion-principal-picker
                  .principalType=${this.dlgPrincipalType}
                  @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) =>
                    this.onPrincipalChange(e.detail)}
                ></scion-principal-picker>
              </div>
              ${this.dlgPrincipalType === 'group'
                ? html`
                    <div class="form-group validation-warning">
                      <sl-icon name="info-circle"></sl-icon>
                      Group members will inherit this project role. Owner role is not available for
                      groups.
                    </div>
                  `
                : nothing}
            `
          : html`
              <div class="dialog-member">
                <div class="member-icon ${this.dlgPrincipalType}">
                  <sl-icon name="${getPrincipalIcon(this.dlgPrincipalType)}"></sl-icon>
                </div>
                <div class="member-info">
                  <span class="member-name">${this.dlgDisplayName || this.dlgPrincipalId}</span>
                  <span class="member-detail">${this.dlgPrincipalType}</span>
                </div>
                ${this.dlgIsLastOwner
                  ? html`<sl-tooltip content=${LAST_OWNER_REASON}>
                      <sl-icon
                        name="shield-lock"
                        style="color: var(--sl-color-warning-500)"
                      ></sl-icon>
                    </sl-tooltip>`
                  : nothing}
              </div>
            `}
        ${this.catalogError
          ? html`<div class="form-group dialog-error">
              Couldn't load the list of roles: ${this.catalogError}
            </div>`
          : nothing}
        ${this.renderBuiltInRadios()} ${this.renderCustomRoles()} ${this.renderEmptySelection()}
        ${this.dlgError ? html`<div class="dialog-error">${this.dlgError}</div>` : nothing}

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.dlgSaving}
          @click=${() => this.closeDialog()}
          >Cancel</sl-button
        >
        <sl-button
          class="save-member"
          slot="footer"
          variant="primary"
          ?loading=${this.dlgSaving}
          ?disabled=${saveDisabled}
          @click=${() => this.handleSave()}
          >${isAdd ? 'Add Member' : 'Save'}</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderTransferDialog() {
    if (!this.transferDialogOpen) return nothing;

    return html`
      <sl-dialog
        label="Transfer Project Ownership"
        open
        @sl-request-close=${() => {
          if (!this.transferLoading) this.transferDialogOpen = false;
        }}
      >
        <p>
          Transfer ownership of this project to another user. The current owner will retain
          membership but lose owner privileges.
        </p>
        <div class="form-group">
          <sl-input
            label="New Owner (User ID or Email)"
            placeholder="Enter user ID or email address"
            .value=${this.transferNewOwnerId}
            @sl-input=${(e: Event) => {
              this.transferNewOwnerId = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>

        ${this.transferError
          ? html`<div class="dialog-error">${this.transferError}</div>`
          : nothing}

        <div class="validation-warning">
          <sl-icon name="exclamation-triangle"></sl-icon>
          This action cannot be undone. The new owner will have full control of this project.
        </div>

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.transferLoading}
          @click=${() => {
            this.transferDialogOpen = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="warning"
          ?loading=${this.transferLoading}
          ?disabled=${!this.transferNewOwnerId.trim()}
          @click=${() => this.handleTransferOwnership()}
          >Transfer Ownership</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-members-editor': ScionProjectMembersEditor;
  }
}
