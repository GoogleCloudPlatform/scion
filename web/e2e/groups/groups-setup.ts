// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Shared setup helpers for group E2E specs.
 *
 * All specs under web/e2e/groups/ use these helpers for consistent
 * environment access, API calls, and test data management.
 */

import { getE2EEnv } from '../harness/env.js';
import { createSession, type AuthSession } from '../harness/auth.js';
import {
  createGroup,
  addGroupMember,
  createRoleBinding,
  findRoleDefinition,
  setMaxMembersPerGroupQuota,
  createAccessBoundary,
  type SeedGroup,
} from '../harness/seed.js';

export { getE2EEnv, createSession, type AuthSession };
export {
  createGroup,
  addGroupMember,
  createRoleBinding,
  findRoleDefinition,
  setMaxMembersPerGroupQuota,
  createAccessBoundary,
  type SeedGroup,
};

/**
 * Make a raw authenticated API request (for seeding / assertions).
 */
export async function apiRequest(
  baseURL: string,
  token: string,
  method: string,
  path: string,
  body?: unknown,
): Promise<Response> {
  const headers: Record<string, string> = {
    Authorization: `Bearer ${token}`,
  };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }
  return fetch(`${baseURL}${path}`, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });
}

/**
 * Delete a group via the API (cleanup helper).
 */
export async function deleteGroupAPI(
  baseURL: string,
  token: string,
  groupId: string,
): Promise<void> {
  await apiRequest(baseURL, token, 'DELETE', `/api/v1/groups/${groupId}`);
}

/**
 * Fetch group details via the API.
 */
export async function getGroupAPI(
  baseURL: string,
  token: string,
  groupId: string,
): Promise<Record<string, unknown>> {
  const res = await apiRequest(
    baseURL,
    token,
    'GET',
    `/api/v1/groups/${groupId}`,
  );
  return (await res.json()) as Record<string, unknown>;
}

/**
 * List group members via the API.
 */
export async function listMembersAPI(
  baseURL: string,
  token: string,
  groupId: string,
): Promise<Array<Record<string, unknown>>> {
  const res = await apiRequest(
    baseURL,
    token,
    'GET',
    `/api/v1/groups/${groupId}/members`,
  );
  const data = (await res.json()) as { members: Array<Record<string, unknown>> };
  return data.members ?? [];
}

/**
 * Generate a unique slug for test isolation.
 */
export function uniqueSlug(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
}

/**
 * Generate a unique name for test isolation.
 */
export function uniqueName(prefix: string): string {
  return `${prefix} ${Date.now().toString(36)}`;
}

/**
 * Fill a Shoelace `sl-input` element.
 *
 * Shoelace web components are custom elements with shadow DOM.
 * Playwright's `fill()` doesn't work directly on `sl-input` because
 * it's not a native `<input>`. This helper sets the value programmatically
 * and dispatches the `sl-input` event so the component reacts.
 */
export async function fillSlInput(
  locator: import('@playwright/test').Locator,
  value: string,
): Promise<void> {
  await locator.evaluate((el: HTMLElement, val: string) => {
    (el as any).value = val;
    el.dispatchEvent(new Event('sl-input', { bubbles: true }));
    el.dispatchEvent(new Event('sl-change', { bubbles: true }));
  }, value);
}

/**
 * Clear and fill a Shoelace `sl-input` element.
 */
export async function clearAndFillSlInput(
  locator: import('@playwright/test').Locator,
  value: string,
): Promise<void> {
  await locator.evaluate((el: HTMLElement, val: string) => {
    (el as any).value = val;
    el.dispatchEvent(new Event('sl-input', { bubbles: true }));
    el.dispatchEvent(new Event('sl-change', { bubbles: true }));
  }, value);
}
