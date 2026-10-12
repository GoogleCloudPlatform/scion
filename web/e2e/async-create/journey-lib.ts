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
 * Pure helpers of the async create journey, kept apart so the
 * harness-selftest suite can check them without a hub or a browser.
 */

/** Parses a positive millisecond timeout from the environment; throws otherwise. */
export function parseTimeoutMs(raw: string | undefined, fallback: number, name: string): number {
  if (raw === undefined || raw === '') return fallback;
  const value = Number(raw);
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`${name} must be a positive number of milliseconds, got "${raw}"`);
  }
  return value;
}

/** What a direct GET of a resource says about its existence. */
export type ReadState = 'not_found' | 'soft_deleted' | 'present' | 'unknown';

/**
 * Classifies a direct GET of one resource (agent, project, template). Only
 * a 404, or a 200 whose record carries a deletedAt (the hub keeps
 * soft-deleted rows readable by id), counts as gone. Any other 200 is
 * present, including a resource the hub is still deleting; any other
 * status is unknown, never gone. wrapperKey names the key the record may
 * be wrapped in (e.g. "agent").
 */
export function classifyRead(status: number, body: unknown, wrapperKey?: string): ReadState {
  if (status === 404) return 'not_found';
  if (status !== 200) return 'unknown';
  const outer = (body ?? {}) as Record<string, unknown>;
  const inner = wrapperKey ? (outer[wrapperKey] as Record<string, unknown> | undefined) : undefined;
  const deletedAt = (inner ?? outer).deletedAt;
  if (typeof deletedAt === 'string' && deletedAt !== '' && !deletedAt.startsWith('0001-01-01')) {
    return 'soft_deleted';
  }
  return 'present';
}

/** Whether a classified read means the resource is gone. */
export function isGone(state: ReadState): boolean {
  return state === 'not_found' || state === 'soft_deleted';
}
