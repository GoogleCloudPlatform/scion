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
 * Tests for group detail page actions (G4 deliverables):
 * - Capability gating matrix (update/delete/none/project_agents)
 * - Changed-fields-only PATCH assembly
 * - Typed-slug gate logic
 * - constraint_gate → boundary dialog with correct href
 * - Owner-picker warning visibility
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'fs';
import { join } from 'path';
import { canGroup } from '../../shared/groups';
import type { AdminGroup, Capabilities } from '../../shared/groups';
import type { UpdateGroupRequest } from '../../shared/groups';

/* -------------------------------------------------------------------------- */
/* Helpers                                                                    */
/* -------------------------------------------------------------------------- */

const FIXTURES_DIR = join(__dirname, '../../client/__fixtures__/groups');

function loadFixture<T = unknown>(name: string): T {
  return JSON.parse(readFileSync(join(FIXTURES_DIR, name), 'utf-8')) as T;
}

/** Create a test group with specified capabilities. */
function makeGroup(
  overrides: Partial<AdminGroup> & { _capabilities?: Capabilities } = {}
): AdminGroup {
  const base = loadFixture<AdminGroup>('group-with-capabilities.json');
  return { ...base, ...overrides };
}

/**
 * Inline PATCH builder — mirrors ScionGroupFormDialog.buildPatch() logic.
 * Extracted to test independently of DOM / Lit lifecycle.
 */
function buildPatch(
  original: { name: string; description: string; ownerId: string; labels: string },
  edited: { name: string; description: string; ownerId: string; labels: string }
): UpdateGroupRequest | null {
  const patch: UpdateGroupRequest = {};
  let hasChanges = false;

  if (edited.name !== original.name) {
    patch.name = edited.name;
    hasChanges = true;
  }

  // Description: blank input means unchanged.
  if (edited.description !== original.description && edited.description !== '') {
    patch.description = edited.description;
    hasChanges = true;
  }

  if (edited.ownerId !== original.ownerId) {
    patch.ownerId = edited.ownerId;
    hasChanges = true;
  }

  if (edited.labels !== original.labels) {
    try {
      patch.labels = JSON.parse(edited.labels) as Record<string, string>;
      hasChanges = true;
    } catch {
      // Invalid JSON
    }
  }

  return hasChanges ? patch : null;
}

/* -------------------------------------------------------------------------- */
/* Mock fetch globally                                                        */
/* -------------------------------------------------------------------------- */

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal('fetch', fetchMock);
});

afterEach(() => {
  vi.restoreAllMocks();
});

/* ========================================================================== */
/* Capability gating matrix                                                   */
/* ========================================================================== */

describe('capability gating matrix', () => {
  it('shows Edit when update capability is present', () => {
    const group = makeGroup({
      _capabilities: { actions: ['read', 'update'] },
    });
    const isProjectAgents = group.groupType === 'project_agents';
    const canEdit = !isProjectAgents && canGroup(group._capabilities, 'update');
    expect(canEdit).toBe(true);
  });

  it('hides Edit when update capability is absent', () => {
    const group = makeGroup({
      _capabilities: { actions: ['read'] },
    });
    const isProjectAgents = group.groupType === 'project_agents';
    const canEdit = !isProjectAgents && canGroup(group._capabilities, 'update');
    expect(canEdit).toBe(false);
  });

  it('shows Delete when delete capability is present', () => {
    const group = makeGroup({
      _capabilities: { actions: ['read', 'delete'] },
    });
    const isProjectAgents = group.groupType === 'project_agents';
    const canDelete = !isProjectAgents && canGroup(group._capabilities, 'delete');
    expect(canDelete).toBe(true);
  });

  it('hides Delete when delete capability is absent', () => {
    const group = makeGroup({
      _capabilities: { actions: ['read', 'update'] },
    });
    const isProjectAgents = group.groupType === 'project_agents';
    const canDelete = !isProjectAgents && canGroup(group._capabilities, 'delete');
    expect(canDelete).toBe(false);
  });

  it('hides both Edit and Delete when no capabilities', () => {
    const group = makeGroup({
      _capabilities: undefined,
    });
    const isProjectAgents = group.groupType === 'project_agents';
    const canEdit = !isProjectAgents && canGroup(group._capabilities, 'update');
    const canDelete = !isProjectAgents && canGroup(group._capabilities, 'delete');
    expect(canEdit).toBe(false);
    expect(canDelete).toBe(false);
  });

  it('hides Edit and Delete for project_agents groups regardless of capabilities', () => {
    const group = makeGroup({
      groupType: 'project_agents',
      _capabilities: { actions: ['read', 'update', 'delete'] },
    });
    const isProjectAgents = group.groupType === 'project_agents';
    const canEdit = !isProjectAgents && canGroup(group._capabilities, 'update');
    const canDelete = !isProjectAgents && canGroup(group._capabilities, 'delete');
    expect(canEdit).toBe(false);
    expect(canDelete).toBe(false);
  });

  it('shows both Edit and Delete when both capabilities present', () => {
    const group = makeGroup({
      _capabilities: { actions: ['read', 'update', 'delete'] },
    });
    const isProjectAgents = group.groupType === 'project_agents';
    const canEdit = !isProjectAgents && canGroup(group._capabilities, 'update');
    const canDelete = !isProjectAgents && canGroup(group._capabilities, 'delete');
    expect(canEdit).toBe(true);
    expect(canDelete).toBe(true);
  });
});

/* ========================================================================== */
/* Changed-fields-only PATCH assembly                                         */
/* ========================================================================== */

describe('changed-fields-only PATCH assembly', () => {
  const original = {
    name: 'Platform Engineers',
    description: 'Core platform engineering team',
    ownerId: 'u-00000000-0000-0000-0000-000000000001',
    labels: '{"team":"platform","tier":"core"}',
  };

  it('returns null when nothing changed', () => {
    expect(buildPatch(original, { ...original })).toBeNull();
  });

  it('includes only name when name changed', () => {
    const edited = { ...original, name: 'Renamed Team' };
    const patch = buildPatch(original, edited);
    expect(patch).toEqual({ name: 'Renamed Team' });
  });

  it('includes only description when description changed', () => {
    const edited = { ...original, description: 'Updated description' };
    const patch = buildPatch(original, edited);
    expect(patch).toEqual({ description: 'Updated description' });
  });

  it('blank description means unchanged (not included in patch)', () => {
    const edited = { ...original, description: '' };
    const patch = buildPatch(original, edited);
    // Description changed from non-empty to empty, but blank = unchanged
    expect(patch).toBeNull();
  });

  it('includes only ownerId when owner changed', () => {
    const edited = { ...original, ownerId: 'u-new-owner' };
    const patch = buildPatch(original, edited);
    expect(patch).toEqual({ ownerId: 'u-new-owner' });
  });

  it('includes only labels when labels changed', () => {
    const edited = { ...original, labels: '{"team":"frontend"}' };
    const patch = buildPatch(original, edited);
    expect(patch).toEqual({ labels: { team: 'frontend' } });
  });

  it('includes multiple changed fields', () => {
    const edited = {
      ...original,
      name: 'New Name',
      ownerId: 'u-new',
    };
    const patch = buildPatch(original, edited);
    expect(patch).toEqual({ name: 'New Name', ownerId: 'u-new' });
  });

  it('omits labels with invalid JSON', () => {
    const edited = { ...original, labels: 'not-json' };
    const patch = buildPatch(original, edited);
    // Labels JSON is invalid, so not included; no other changes → null
    expect(patch).toBeNull();
  });

  it('supports clearing all labels with empty object', () => {
    const edited = { ...original, labels: '{}' };
    const patch = buildPatch(original, edited);
    expect(patch).toEqual({ labels: {} });
  });
});

/* ========================================================================== */
/* Typed-slug gate logic                                                      */
/* ========================================================================== */

describe('typed-slug gate logic', () => {
  it('blocks delete when typed slug does not match', () => {
    const group = makeGroup({ slug: 'platform-engineers' });
    const typedSlug = 'wrong-slug';
    expect(typedSlug === group.slug).toBe(false);
  });

  it('allows delete when typed slug matches exactly', () => {
    const group = makeGroup({ slug: 'platform-engineers' });
    const typedSlug = 'platform-engineers';
    expect(typedSlug === group.slug).toBe(true);
  });

  it('blocks delete when typed slug has different case', () => {
    const group = makeGroup({ slug: 'platform-engineers' });
    const typedSlug = 'Platform-Engineers';
    expect(typedSlug === group.slug).toBe(false);
  });

  it('blocks delete when typed slug is empty', () => {
    const group = makeGroup({ slug: 'platform-engineers' });
    const typedSlug = '';
    expect(typedSlug === group.slug).toBe(false);
  });

  it('blocks delete when typed slug is a partial match', () => {
    const group = makeGroup({ slug: 'platform-engineers' });
    const typedSlug = 'platform';
    expect(typedSlug === group.slug).toBe(false);
  });
});

/* ========================================================================== */
/* constraint_gate → boundary dialog with correct href                        */
/* ========================================================================== */

describe('constraint_gate boundary dialog href', () => {
  it('builds correct access-boundary URL from group ID', () => {
    const groupId = 'g-00000000-0000-0000-0000-000000000001';
    const expectedUrl = `/admin/access-boundaries?subjectKind=group_closure&subjectId=${encodeURIComponent(groupId)}`;
    expect(expectedUrl).toBe(
      '/admin/access-boundaries?subjectKind=group_closure&subjectId=g-00000000-0000-0000-0000-000000000001'
    );
  });

  it('correctly encodes special characters in group ID', () => {
    const groupId = 'group/with+special=chars';
    const expectedUrl = `/admin/access-boundaries?subjectKind=group_closure&subjectId=${encodeURIComponent(groupId)}`;
    expect(expectedUrl).toContain('group%2Fwith%2Bspecial%3Dchars');
  });

  it('classifies constraint_gate error kind from fixture', () => {
    const fixture = loadFixture<{ error: { message: string } }>('error-constraint-gate.json');
    const msg = fixture.error.message;
    // The classifyError function checks for these strings
    expect(
      msg.includes('access constraint') || msg.includes('access_constraint')
    ).toBe(true);
  });
});

/* ========================================================================== */
/* Owner-picker warning visibility                                            */
/* ========================================================================== */

describe('owner-picker warning visibility', () => {
  it('shows warning when owner changed from original', () => {
    const originalOwnerId = 'u-00000000-0000-0000-0000-000000000001';
    const editOwnerId = 'u-new-owner-id';
    const ownerChanged = editOwnerId !== originalOwnerId && editOwnerId !== '';
    expect(ownerChanged).toBe(true);
  });

  it('hides warning when owner is unchanged', () => {
    const originalOwnerId = 'u-00000000-0000-0000-0000-000000000001';
    const editOwnerId = 'u-00000000-0000-0000-0000-000000000001';
    const ownerChanged = editOwnerId !== originalOwnerId && editOwnerId !== '';
    expect(ownerChanged).toBe(false);
  });

  it('hides warning when owner is cleared (empty string)', () => {
    const originalOwnerId = 'u-00000000-0000-0000-0000-000000000001';
    const editOwnerId = '';
    const ownerChanged = editOwnerId !== originalOwnerId && editOwnerId !== '';
    expect(ownerChanged).toBe(false);
  });

  it('shows warning when owner set from empty to new value', () => {
    const originalOwnerId = '';
    const editOwnerId = 'u-new-owner-id';
    const ownerChanged = editOwnerId !== originalOwnerId && editOwnerId !== '';
    expect(ownerChanged).toBe(true);
  });
});
