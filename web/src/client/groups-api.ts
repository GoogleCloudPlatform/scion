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
 * Groups API adapter — transport layer for group CRUD and membership.
 *
 * This module owns URL building and error normalization for every group
 * endpoint. Backend error messages are string-matched HERE and nowhere else;
 * consumers switch on {@link GroupsApiError.kind} only.
 *
 * `deleteGroup` and `removeMember` use raw `fetch(credentials:'include')`
 * instead of `apiFetch` to bypass the global `scion:access-denied` 403 toast,
 * since both have purpose-built 403 UX.
 */

import { apiFetch, parseApiError } from './api.js';
import type {
  AddMemberRequest,
  AdminGroup,
  CreateGroupRequest,
  GroupMember,
  GroupsFilter,
  ListGroupsResponse,
  RemoveMemberResult,
  UpdateGroupRequest,
} from '../shared/groups.js';

/* -------------------------------------------------------------------------- */
/* Error types                                                                */
/* -------------------------------------------------------------------------- */

/**
 * Discriminated error kinds for group API operations.
 *
 * Each kind maps to a specific backend error condition. The mapping from
 * HTTP status + message text lives exclusively in {@link classifyError}.
 */
export type GroupsApiErrorKind =
  | 'conflict_slug'
  | 'conflict_member'
  | 'cycle'
  | 'quota'
  | 'last_owner'
  | 'constraint_gate'
  | 'delegation'
  | 'hierarchy'
  | 'not_found'
  | 'validation'
  | 'forbidden'
  | 'http';

/**
 * Normalized error thrown by all groups API functions.
 *
 * UI code switches on `kind` — never on raw HTTP status or message text.
 */
export class GroupsApiError extends Error {
  readonly kind: GroupsApiErrorKind;
  readonly httpStatus: number;

  constructor(kind: GroupsApiErrorKind, message: string, httpStatus: number) {
    super(message);
    this.name = 'GroupsApiError';
    this.kind = kind;
    this.httpStatus = httpStatus;
  }
}

/* -------------------------------------------------------------------------- */
/* Error classification — ALL string matching lives here                      */
/* -------------------------------------------------------------------------- */

/**
 * Classify an HTTP error response into a discriminated {@link GroupsApiErrorKind}.
 *
 * This is the SINGLE PLACE that matches on backend message text.
 * A backend copy change should break at most one test file.
 */
async function classifyError(res: Response): Promise<GroupsApiError> {
  const status = res.status;
  const info = await parseApiError(res, 'Unknown error');
  const msg = info.message;

  // 409 — distinguish slug vs. member conflict
  if (status === 409) {
    if (msg.includes('slug already exists')) {
      return new GroupsApiError('conflict_slug', msg, status);
    }
    if (msg.includes('Member already exists')) {
      return new GroupsApiError('conflict_member', msg, status);
    }
  }

  // 400 — cycle detection, last-owner protection
  if (status === 400) {
    if (msg.includes('cycle in the group hierarchy')) {
      return new GroupsApiError('cycle', msg, status);
    }
    if (msg.includes('last owner')) {
      return new GroupsApiError('last_owner', msg, status);
    }
    return new GroupsApiError('validation', msg, status);
  }

  // 429 — quota exceeded
  if (status === 429) {
    return new GroupsApiError('quota', msg, status);
  }

  // 403 — constraint gate, delegation, role hierarchy, generic forbidden
  if (status === 403) {
    if (msg.includes('access constraint') || msg.includes('access_constraint')) {
      return new GroupsApiError('constraint_gate', msg, status);
    }
    if (msg.includes('Cannot grant authority')) {
      return new GroupsApiError('delegation', msg, status);
    }
    if (msg.includes('Only group owners')) {
      return new GroupsApiError('hierarchy', msg, status);
    }
    return new GroupsApiError('forbidden', msg, status);
  }

  // 404 — not found
  if (status === 404) {
    return new GroupsApiError('not_found', msg, status);
  }

  // 422 — validation
  if (status === 422) {
    return new GroupsApiError('validation', msg, status);
  }

  // Fallback
  return new GroupsApiError('http', msg, status);
}

/* -------------------------------------------------------------------------- */
/* URL building                                                               */
/* -------------------------------------------------------------------------- */

const BASE_PATH = '/api/v1/groups';

/** Build the query string for a GroupsFilter. */
export function buildGroupsQuery(filter: GroupsFilter): string {
  const params = new URLSearchParams();
  if (filter.search) params.set('search', filter.search);
  if (filter.groupType) params.set('groupType', filter.groupType);
  if (filter.ownerId) params.set('ownerId', filter.ownerId);
  if (filter.projectId) params.set('projectId', filter.projectId);
  if (filter.parentId) params.set('parentId', filter.parentId);
  if (filter.limit != null) params.set('limit', String(filter.limit));
  if (filter.cursor) params.set('cursor', filter.cursor);
  const qs = params.toString();
  return qs ? `${BASE_PATH}?${qs}` : BASE_PATH;
}

/* -------------------------------------------------------------------------- */
/* API functions                                                              */
/* -------------------------------------------------------------------------- */

/** List groups with optional filtering and pagination. */
export async function listGroups(
  filter: GroupsFilter = {},
  signal?: AbortSignal
): Promise<ListGroupsResponse> {
  const url = buildGroupsQuery(filter);
  const res = await apiFetch(url, signal ? { signal } : undefined);
  if (!res.ok) throw await classifyError(res);
  return (await res.json()) as ListGroupsResponse;
}

/** Get a single group by ID or slug. */
export async function getGroup(idOrSlug: string): Promise<AdminGroup> {
  const res = await apiFetch(`${BASE_PATH}/${encodeURIComponent(idOrSlug)}`);
  if (!res.ok) throw await classifyError(res);
  return (await res.json()) as AdminGroup;
}

/** Create a new group. */
export async function createGroup(req: CreateGroupRequest): Promise<AdminGroup> {
  const res = await apiFetch(BASE_PATH, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  });
  if (!res.ok) throw await classifyError(res);
  return (await res.json()) as AdminGroup;
}

/** Update an existing group (partial patch). */
export async function updateGroup(id: string, patch: UpdateGroupRequest): Promise<AdminGroup> {
  const res = await apiFetch(`${BASE_PATH}/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  });
  if (!res.ok) throw await classifyError(res);
  return (await res.json()) as AdminGroup;
}

/**
 * Delete a group.
 *
 * Uses raw `fetch` (not `apiFetch`) to bypass the global 403 toast —
 * the delete confirmation dialog provides its own constraint-gate UX.
 */
export async function deleteGroup(id: string): Promise<void> {
  const res = await fetch(`${BASE_PATH}/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    credentials: 'include',
  });
  if (!res.ok) throw await classifyError(res);
}

/** List groups that the current user is a member of. */
export async function listMyGroups(): Promise<AdminGroup[]> {
  const res = await apiFetch('/api/v1/users/me/groups');
  if (!res.ok) throw await classifyError(res);
  const data = (await res.json()) as { groups: AdminGroup[] };
  return data.groups;
}

/** List members of a group. */
export async function listMembers(groupId: string): Promise<GroupMember[]> {
  const res = await apiFetch(`${BASE_PATH}/${encodeURIComponent(groupId)}/members`);
  if (!res.ok) throw await classifyError(res);
  const data = (await res.json()) as { members: GroupMember[] };
  return data.members;
}

/** Add a member to a group. */
export async function addMember(groupId: string, req: AddMemberRequest): Promise<GroupMember> {
  const res = await apiFetch(`${BASE_PATH}/${encodeURIComponent(groupId)}/members`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  });
  if (!res.ok) throw await classifyError(res);
  return (await res.json()) as GroupMember;
}

/**
 * Remove a member from a group.
 *
 * Uses raw `fetch` (not `apiFetch`) to bypass the global 403 toast —
 * the remove-member flow provides its own constraint-gate UX.
 *
 * Returns a discriminated result: `ok`, `security_review`, or `lockout`.
 */
export async function removeMember(
  groupId: string,
  type: string,
  id: string
): Promise<RemoveMemberResult> {
  const res = await fetch(
    `${BASE_PATH}/${encodeURIComponent(groupId)}/members/${encodeURIComponent(type)}/${encodeURIComponent(id)}`,
    {
      method: 'DELETE',
      credentials: 'include',
    }
  );

  if (res.status === 204) {
    return { outcome: 'ok' };
  }

  // Non-204 success with a body may indicate security_review or lockout.
  if (res.ok) {
    const body = (await res.json()) as Record<string, unknown>;
    const sr = body.security_review as { detail?: string; message?: string } | undefined;
    const lo = body.lockout as { detail?: string; message?: string } | undefined;
    if (sr) {
      return {
        outcome: 'security_review',
        detail: sr.detail ?? sr.message ?? '',
        rawBody: body,
      };
    }
    if (lo) {
      return {
        outcome: 'lockout',
        detail: lo.detail ?? lo.message ?? '',
        rawBody: body,
      };
    }
    return { outcome: 'ok' };
  }

  // Non-ok responses may also carry structured lockout or security-review
  // payloads (e.g. 403 CONSTRAINT_ADMIN_LOCKOUT, 403 SECURITY_REVIEW_REQUIRED).
  // Surface these as result outcomes instead of throwing, so that UI code can
  // display purpose-built dialogs rather than a generic error toast.
  const clonedRes = res.clone();
  const errorBody = (await res.json().catch(() => null)) as Record<string, unknown> | null;
  if (errorBody) {
    const error = errorBody.error as Record<string, unknown> | undefined;
    if (error?.code === 'CONSTRAINT_ADMIN_LOCKOUT') {
      return {
        outcome: 'lockout',
        detail: (error.message as string) ?? '',
        rawBody: errorBody,
      };
    }
    if (error?.code === 'SECURITY_REVIEW_REQUIRED') {
      return {
        outcome: 'security_review',
        detail: (error.message as string) ?? '',
        rawBody: errorBody,
      };
    }
  }

  // Use the cloned response for classifyError since we already consumed the
  // original body above.
  throw await classifyError(clonedRes);
}
