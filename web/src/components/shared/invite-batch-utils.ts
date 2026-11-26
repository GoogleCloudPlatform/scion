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
 * Shared types, parsing helpers, and human-readable formatters for batch user
 * invites (`POST /api/v1/admin/users/invite/bulk`, ptone/scion#4952).
 */

/** Server-enforced batch caps (pkg/hub/admin_user_invite_batch.go). */
export const INVITE_BULK_MAX_EMAILS = 1000;
export const INVITE_BULK_MAX_PROJECTS = 20;
export const INVITE_BULK_MAX_ROLES_PER_PROJECT = 10;
export const INVITE_BULK_MAX_MEMBERSHIPS = 2000;

export interface InviteBatchEmailEntry {
  email: string;
  note?: string;
}

export interface InviteBatchProjectEntry {
  projectId: string;
  roleDefinitionIds: string[];
}

export interface InviteBatchRequest {
  emails: InviteBatchEmailEntry[];
  projects?: InviteBatchProjectEntry[];
}

export interface InviteBatchCounts {
  created: number;
  already_invited: number;
  already_active: number;
  suspended: number;
  exists?: number;
  invalid: number;
  duplicate: number;
  failed: number;
  membership_failed: number;
}

export type InviteBatchMembershipStatus =
  | 'applied'
  | 'already_member'
  | 'conflict'
  | 'failed'
  | 'skipped';

export interface InviteBatchMembership {
  projectId: string;
  status: InviteBatchMembershipStatus;
  code?: string;
  cause?: string;
  currentRoleDefinitionIds?: string[];
}

export type InviteBatchResultStatus =
  | 'created'
  | 'already_invited'
  | 'already_active'
  | 'suspended'
  | 'exists'
  | 'invalid'
  | 'duplicate'
  | 'failed';

export interface InviteBatchResult {
  index: number;
  email?: string;
  input?: string;
  status: InviteBatchResultStatus;
  reason?: string;
  userId?: string;
  duplicateOf?: number;
  memberships?: InviteBatchMembership[];
}

export interface InviteBatchResponse {
  batchId: string;
  invited: number;
  skipped: number;
  total: number;
  complete: boolean;
  counts: InviteBatchCounts;
  results: InviteBatchResult[];
}

export interface InviteBatchProjectRefusal {
  index: number;
  projectId: string;
  reason: string;
  roleDefinitionId?: string;
  denialCode?: string;
  details?: Record<string, unknown>;
}

export interface ParsedAddressInput {
  /** Deduplicated trimmed addresses in first-seen order. */
  addresses: string[];
  /** Total non-empty tokens before deduplication. */
  totalTokens: number;
  /** Number of duplicate tokens removed (case-insensitive). */
  duplicateCount: number;
}

export interface ParsedCsvInput {
  /** Deduplicated email+note entries in first-seen order. */
  entries: InviteBatchEmailEntry[];
  /** Total non-empty data rows before deduplication. */
  totalRows: number;
  /** Number of duplicate rows removed (case-insensitive on email). */
  duplicateCount: number;
}

export type ShoelaceTagVariant = 'primary' | 'success' | 'neutral' | 'warning' | 'danger';

/**
 * Parse a free-form textarea of email addresses separated by newlines, commas,
 * or semicolons. Blank entries and `#` comment lines are skipped. Case-insensitive
 * duplicates are removed while preserving the first occurrence's order.
 */
export function parseInviteAddressInput(raw: string): ParsedAddressInput {
  const seen = new Set<string>();
  const addresses: string[] = [];
  let totalTokens = 0;
  let duplicateCount = 0;

  for (const line of raw.split(/\r?\n/)) {
    const trimmedLine = line.trim();
    if (!trimmedLine || trimmedLine.startsWith('#')) continue;

    for (const token of trimmedLine.split(/[,;]+/)) {
      const addr = token.trim();
      if (!addr) continue;
      totalTokens++;
      const key = addr.toLowerCase();
      if (seen.has(key)) {
        duplicateCount++;
        continue;
      }
      seen.add(key);
      addresses.push(addr);
    }
  }

  return { addresses, totalTokens, duplicateCount };
}

/**
 * Parse a single CSV line into fields, supporting RFC 4180 double-quoted fields
 * with escaped `""` quotes.
 */
function parseCsvLine(line: string): string[] {
  const fields: string[] = [];
  let current = '';
  let inQuotes = false;

  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (inQuotes) {
      if (ch === '"') {
        if (i + 1 < line.length && line[i + 1] === '"') {
          current += '"';
          i++;
        } else {
          inQuotes = false;
        }
      } else {
        current += ch;
      }
    } else if (ch === '"') {
      inQuotes = true;
    } else if (ch === ',') {
      fields.push(current);
      current = '';
    } else {
      current += ch;
    }
  }
  fields.push(current);
  return fields;
}

/**
 * Parse CSV text with `email` or `email,note` columns.
 * Skips blank lines, `#` comment lines, and an optional `email`/`e-mail` header row.
 * Deduplicates case-insensitively on email (keeping the first row's note).
 */
export function parseInviteCsv(text: string): ParsedCsvInput {
  const seen = new Set<string>();
  const entries: InviteBatchEmailEntry[] = [];
  let totalRows = 0;
  let duplicateCount = 0;
  let firstDataLineSeen = false;

  // Strip optional UTF-8 BOM
  const clean = text.startsWith('\uFEFF') ? text.slice(1) : text;

  for (const rawLine of clean.split(/\r?\n/)) {
    const trimmed = rawLine.trim();
    if (!trimmed || trimmed.startsWith('#')) continue;

    const fields = parseCsvLine(rawLine);
    const email = (fields[0] ?? '').trim();
    if (!email) continue;

    if (!firstDataLineSeen) {
      firstDataLineSeen = true;
      const lower = email.toLowerCase();
      if (lower === 'email' || lower === 'e-mail') {
        continue;
      }
    }

    totalRows++;
    const key = email.toLowerCase();
    if (seen.has(key)) {
      duplicateCount++;
      continue;
    }
    seen.add(key);

    const note = (fields[1] ?? '').trim();
    const entry: InviteBatchEmailEntry = note ? { email, note } : { email };
    entries.push(entry);
  }

  return { entries, totalRows, duplicateCount };
}

/** Parse 403 `batch_projects_not_grantable` refusal details from an API error payload. */
export function parseBatchRefusalDetails(details: Record<string, unknown> | undefined): {
  isProjectRefusal: boolean;
  isUserReadRequired: boolean;
  refusals: InviteBatchProjectRefusal[];
} {
  if (!details) {
    return { isProjectRefusal: false, isUserReadRequired: false, refusals: [] };
  }
  const reason = typeof details.reason === 'string' ? details.reason : '';
  const isUserReadRequired = reason === 'user_read_required';
  const isProjectRefusal = reason === 'batch_projects_not_grantable';

  const rawProjects = Array.isArray(details.projects) ? details.projects : [];
  const refusals: InviteBatchProjectRefusal[] = rawProjects
    .filter((item): item is Record<string, unknown> => !!item && typeof item === 'object')
    .map((item) => {
      const refusal: InviteBatchProjectRefusal = {
        index: typeof item.index === 'number' ? item.index : 0,
        projectId: typeof item.projectId === 'string' ? item.projectId : '',
        reason: typeof item.reason === 'string' ? item.reason : '',
      };
      if (typeof item.roleDefinitionId === 'string' && item.roleDefinitionId) {
        refusal.roleDefinitionId = item.roleDefinitionId;
      }
      if (typeof item.denialCode === 'string' && item.denialCode) {
        refusal.denialCode = item.denialCode;
      }
      if (item.details && typeof item.details === 'object') {
        refusal.details = item.details as Record<string, unknown>;
      }
      return refusal;
    });

  return {
    isProjectRefusal: isProjectRefusal || refusals.length > 0,
    isUserReadRequired:
      isUserReadRequired || refusals.some((r) => r.reason === 'user_read_required'),
    refusals,
  };
}

export function formatAddressStatusLabel(status: string): string {
  switch (status) {
    case 'created':
      return 'Created';
    case 'already_invited':
      return 'Already invited';
    case 'already_active':
      return 'Already active';
    case 'suspended':
      return 'Suspended';
    case 'exists':
      return 'Already exists';
    case 'invalid':
      return 'Invalid';
    case 'duplicate':
      return 'Duplicate';
    case 'failed':
      return 'Failed';
    default:
      return status;
  }
}

export function formatAddressStatusVariant(status: string): ShoelaceTagVariant {
  switch (status) {
    case 'created':
      return 'success';
    case 'already_invited':
      return 'primary';
    case 'already_active':
    case 'exists':
    case 'duplicate':
      return 'neutral';
    case 'suspended':
      return 'warning';
    case 'invalid':
    case 'failed':
      return 'danger';
    default:
      return 'neutral';
  }
}

export function formatAddressReason(reason?: string, duplicateOf?: number): string {
  if (!reason && duplicateOf !== undefined) {
    return `Duplicate of entry #${duplicateOf + 1}`;
  }
  switch (reason) {
    case undefined:
    case '':
      return '';
    case 'invalid_email':
      return 'Invalid email address';
    case 'note_too_long':
      return 'Note exceeds maximum length';
    case 'note_invalid_characters':
      return 'Note contains invalid control characters';
    case 'duplicate_in_batch':
      return duplicateOf !== undefined
        ? `Duplicate of entry #${duplicateOf + 1}`
        : 'Duplicate address in batch';
    case 'email_not_allowed':
      return 'Email address is not allowed';
    case 'reserved_test_identity_domain':
      return 'Reserved test identity domain is not allowed';
    case 'pending_user_exists':
      return 'Already invited';
    case 'user_exists':
      return 'Active user already exists';
    case 'user_suspended_exists':
      return 'Account is suspended';
    case 'internal_error':
      return 'Internal server error';
    case 'not_processed':
      return 'Not processed before batch timeout; safe to retry';
    default:
      return reason.replace(/_/g, ' ');
  }
}

export function formatMembershipStatusLabel(status: string): string {
  switch (status) {
    case 'applied':
      return 'Applied';
    case 'already_member':
      return 'Already member';
    case 'conflict':
      return 'Conflict';
    case 'failed':
      return 'Failed';
    case 'skipped':
      return 'Skipped';
    default:
      return status;
  }
}

export function formatMembershipStatusVariant(status: string): ShoelaceTagVariant {
  switch (status) {
    case 'applied':
      return 'success';
    case 'already_member':
      return 'primary';
    case 'conflict':
      return 'warning';
    case 'failed':
      return 'danger';
    case 'skipped':
    default:
      return 'neutral';
  }
}

export function formatMembershipDetail(
  membership: InviteBatchMembership,
  roleLabels?: Record<string, string>
): string {
  if (membership.status === 'conflict' && membership.code === 'member_with_other_roles') {
    const current = (membership.currentRoleDefinitionIds ?? []).map((id) => roleLabels?.[id] || id);
    return current.length > 0
      ? `Already holds other roles (${current.join(', ')})`
      : 'Already holds other roles in this project';
  }
  if (membership.code === 'address_not_invited') {
    return 'Skipped (account is not in invited status)';
  }
  if (membership.code === 'not_processed') {
    return 'Not processed; safe to retry';
  }
  if (membership.cause) {
    return membership.cause;
  }
  if (membership.code) {
    return membership.code.replace(/_/g, ' ');
  }
  return '';
}

export function formatProjectRefusalLine(
  refusal: InviteBatchProjectRefusal,
  projectLabels?: Record<string, string>,
  roleLabels?: Record<string, string>
): string {
  if (refusal.reason === 'user_read_required') {
    return 'Assigning project memberships on invite requires user.read permission.';
  }
  const projectName =
    (refusal.projectId && projectLabels?.[refusal.projectId]) || refusal.projectId || 'Project';
  const roleName =
    (refusal.roleDefinitionId && roleLabels?.[refusal.roleDefinitionId]) ||
    refusal.roleDefinitionId ||
    '';

  switch (refusal.reason) {
    case 'project_not_manageable':
      return `${projectName}: you do not have permission to manage members on this project.`;
    case 'role_not_assignable':
      return roleName
        ? `${projectName} (${roleName}): role cannot be assigned to project members.`
        : `${projectName}: requested role cannot be assigned.`;
    case 'role_not_grantable':
      return roleName
        ? `${projectName} (${roleName}): you do not have authority to grant this role.`
        : `${projectName}: you do not have authority to grant the requested role.`;
    default:
      return roleName
        ? `${projectName} (${roleName}): ${refusal.reason.replace(/_/g, ' ')}`
        : `${projectName}: ${refusal.reason.replace(/_/g, ' ')}`;
  }
}

/** Returns true if a batch result row represents an issue worth highlighting. */
export function isIssueResult(result: InviteBatchResult): boolean {
  if (result.status === 'invalid' || result.status === 'failed' || result.status === 'suspended') {
    return true;
  }
  return (result.memberships ?? []).some((m) => m.status === 'failed' || m.status === 'conflict');
}

/** Describes an advisory warning of POST /api/v1/users. */
export function provisionWarningText(warning: string): string {
  switch (warning) {
    case 'reserved_identity':
      return 'This email is a reserved platform identity; sign-in will be refused.';
    case 'domain_not_authorized':
      return "This email is outside the hub's authorized domains; sign-in will be refused.";
    case 'sign_in_currently_blocked_by_access_mode':
      return "The hub's access mode currently blocks all sign-ins.";
    default:
      return `Warning: ${warning}.`;
  }
}
