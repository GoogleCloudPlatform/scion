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
 * Shared groups contract — types and capability helpers.
 *
 * This module is the single source of truth for group-related types shared
 * between the API adapter, UI components, and tests. It reuses AdminGroup,
 * GroupMember, and GroupType from shared/types.ts — those types are NOT
 * duplicated here.
 */

import type { AdminGroup, Capabilities, GroupMember, GroupType } from './types.js';

/* -------------------------------------------------------------------------- */
/* Capability actions                                                         */
/* -------------------------------------------------------------------------- */

/** Actions that can be performed on a specific group resource. */
export type GroupCapabilityAction = 'read' | 'update' | 'delete' | 'addMember' | 'removeMember';

/** Actions that can be performed at the group scope (not resource-specific). */
export type GroupScopeCapabilityAction = 'create' | 'list';

/**
 * Fail-closed capability check for group actions.
 *
 * Returns `true` only when `caps` is defined AND its `actions` array
 * includes the requested action. Missing or undefined capabilities
 * always deny — this mirrors {@link canAccessBoundary} from the access
 * boundaries contract.
 */
export function canGroup(
  caps: Capabilities | undefined,
  action: GroupCapabilityAction | GroupScopeCapabilityAction
): boolean {
  return caps?.actions?.includes(action) ?? false;
}

/* -------------------------------------------------------------------------- */
/* Request / response interfaces                                              */
/* -------------------------------------------------------------------------- */

/** Response envelope for GET /api/v1/groups. */
export interface ListGroupsResponse {
  groups: AdminGroup[];
  nextCursor?: string;
  totalCount: number;
  /** Scope-level capabilities (what the caller can do at the groups scope). */
  _capabilities?: Capabilities;
}

/** Query parameters accepted by GET /api/v1/groups. */
export interface GroupsFilter {
  search?: string;
  groupType?: GroupType;
  ownerId?: string;
  projectId?: string;
  parentId?: string;
  limit?: number;
  cursor?: string;
}

/** Request body for POST /api/v1/groups. */
export interface CreateGroupRequest {
  name: string;
  slug?: string;
  description?: string;
  groupType?: GroupType;
  parentId?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  ownerId?: string;
}

/** Request body for PATCH /api/v1/groups/:id. */
export interface UpdateGroupRequest {
  name?: string;
  description?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  ownerId?: string;
}

/** Request body for POST /api/v1/groups/:id/members. */
export interface AddMemberRequest {
  memberType: 'user' | 'group' | 'agent';
  memberId: string;
  role?: 'member' | 'admin' | 'owner';
}

/**
 * Discriminated result for removeMember.
 *
 * - `ok` — member was removed successfully.
 * - `security_review` — removal triggered a security review.
 * - `lockout` — removal would lock out the caller.
 *
 * The `rawBody` field carries the full parsed JSON response body so that
 * consumers can extract richer structured detail (e.g. SecurityReviewDetail,
 * LockoutConflict) without duplicating the raw fetch call.
 */
export type RemoveMemberResult =
  | { outcome: 'ok' }
  | { outcome: 'security_review'; detail: string; rawBody: Record<string, unknown> }
  | { outcome: 'lockout'; detail: string; rawBody: Record<string, unknown> };

/* Re-export types that downstream consumers commonly need alongside groups. */
export type { AdminGroup, Capabilities, GroupMember, GroupType };
