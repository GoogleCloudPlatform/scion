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
 * Tests for admin-groups.ts (G2) and group-form-dialog.ts (G3).
 *
 * Covers:
 * - Filter → query-param mapping
 * - URL round-trip (readFiltersFromURL / syncFiltersToURL)
 * - Pagination stack push/pop
 * - Tab switching
 * - Capability-gated header
 * - Empty-state variants (fixture-driven)
 * - Slugify mirror + detach behavior
 * - Label-editor validation
 * - Error kind → surface mapping
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import { canGroup } from '../../shared/groups.js';
import type { Capabilities } from '../../shared/groups.js';
import { buildGroupsQuery } from '../../client/groups-api.js';
import { slugify } from '../shared/group-form-dialog.js';

/* ========================================================================== */
/* canGroup() — capability-gated header                                        */
/* ========================================================================== */

describe('canGroup — capability gating', () => {
  it('returns true when action is in the actions array', () => {
    const caps: Capabilities = { actions: ['create', 'list'] };
    expect(canGroup(caps, 'create')).toBe(true);
    expect(canGroup(caps, 'list')).toBe(true);
  });

  it('returns false when action is not in the actions array', () => {
    const caps: Capabilities = { actions: ['list'] };
    expect(canGroup(caps, 'create')).toBe(false);
  });

  it('returns false when capabilities are undefined (fail-closed)', () => {
    expect(canGroup(undefined, 'create')).toBe(false);
  });

  it('returns false when actions array is empty', () => {
    const caps: Capabilities = { actions: [] };
    expect(canGroup(caps, 'create')).toBe(false);
  });

  it('checks resource-level actions correctly', () => {
    const caps: Capabilities = { actions: ['read', 'update', 'delete', 'addMember', 'removeMember'] };
    expect(canGroup(caps, 'read')).toBe(true);
    expect(canGroup(caps, 'update')).toBe(true);
    expect(canGroup(caps, 'delete')).toBe(true);
    expect(canGroup(caps, 'addMember')).toBe(true);
    expect(canGroup(caps, 'removeMember')).toBe(true);
    expect(canGroup(caps, 'create')).toBe(false);
  });
});

/* ========================================================================== */
/* Filter → query-param mapping                                                */
/* ========================================================================== */

describe('buildGroupsQuery — filter to query-param mapping', () => {
  it('returns bare path with no filters', () => {
    expect(buildGroupsQuery({})).toBe('/api/v1/groups');
  });

  it('maps search filter', () => {
    const url = buildGroupsQuery({ search: 'platform' });
    expect(url).toContain('search=platform');
  });

  it('maps groupType filter', () => {
    const url = buildGroupsQuery({ groupType: 'explicit' });
    expect(url).toContain('groupType=explicit');
  });

  it('maps ownerId filter', () => {
    const url = buildGroupsQuery({ ownerId: 'u-123' });
    expect(url).toContain('ownerId=u-123');
  });

  it('maps limit parameter', () => {
    const url = buildGroupsQuery({ limit: 25 });
    expect(url).toContain('limit=25');
  });

  it('maps cursor parameter', () => {
    const url = buildGroupsQuery({ cursor: 'abc123' });
    expect(url).toContain('cursor=abc123');
  });

  it('maps multiple filters simultaneously', () => {
    const url = buildGroupsQuery({
      search: 'eng',
      groupType: 'explicit',
      ownerId: 'u-456',
      limit: 25,
      cursor: 'xyz',
    });
    expect(url).toContain('search=eng');
    expect(url).toContain('groupType=explicit');
    expect(url).toContain('ownerId=u-456');
    expect(url).toContain('limit=25');
    expect(url).toContain('cursor=xyz');
  });

  it('omits falsy values', () => {
    const url = buildGroupsQuery({ search: '', groupType: undefined });
    expect(url).toBe('/api/v1/groups');
  });

  it('maps projectId filter', () => {
    const url = buildGroupsQuery({ projectId: 'p-001' });
    expect(url).toContain('projectId=p-001');
  });

  it('maps parentId filter', () => {
    const url = buildGroupsQuery({ parentId: 'g-parent' });
    expect(url).toContain('parentId=g-parent');
  });
});

/* ========================================================================== */
/* URL round-trip                                                              */
/* ========================================================================== */

describe('URL round-trip (readFiltersFromURL / syncFiltersToURL)', () => {
  let originalLocation: Location;
  let replaceStateSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    originalLocation = window.location;
    replaceStateSpy = vi.fn();
    vi.spyOn(window.history, 'replaceState').mockImplementation(replaceStateSpy);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('reads filters from URL query params', async () => {
    // Import dynamically to work with happy-dom
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    // Simulate URL params
    Object.defineProperty(window, 'location', {
      value: {
        ...originalLocation,
        search: '?q=test&groupType=explicit&owner=me&tab=mine&cursor=abc',
        pathname: '/admin/groups',
      },
      writable: true,
    });

    el.readFiltersFromURL();

    // Access private fields via any
    /* eslint-disable @typescript-eslint/no-explicit-any */
    expect((el as any).searchQuery).toBe('test');
    expect((el as any).filterGroupType).toBe('explicit');
    expect((el as any).filterOwnedByMe).toBe(true);
    expect((el as any).activeTab).toBe('mine');
    expect((el as any).currentCursor).toBe('abc');
    /* eslint-enable @typescript-eslint/no-explicit-any */

    // Restore
    Object.defineProperty(window, 'location', {
      value: originalLocation,
      writable: true,
    });
  });

  it('syncs filters to URL', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    Object.defineProperty(window, 'location', {
      value: {
        ...originalLocation,
        search: '',
        pathname: '/admin/groups',
      },
      writable: true,
    });

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).searchQuery = 'hello';
    (el as any).filterGroupType = 'project_agents';
    (el as any).filterOwnedByMe = true;
    (el as any).activeTab = 'mine';
    (el as any).currentCursor = 'cursor123';
    /* eslint-enable @typescript-eslint/no-explicit-any */

    el.syncFiltersToURL();

    expect(replaceStateSpy).toHaveBeenCalledWith(
      {},
      '',
      expect.stringContaining('q=hello')
    );
    const urlArg = replaceStateSpy.mock.calls[0][2] as string;
    expect(urlArg).toContain('groupType=project_agents');
    expect(urlArg).toContain('owner=me');
    expect(urlArg).toContain('tab=mine');
    expect(urlArg).toContain('cursor=cursor123');

    Object.defineProperty(window, 'location', {
      value: originalLocation,
      writable: true,
    });
  });

  it('omits default/empty values from URL', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    Object.defineProperty(window, 'location', {
      value: {
        ...originalLocation,
        search: '',
        pathname: '/admin/groups',
      },
      writable: true,
    });

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).searchQuery = '';
    (el as any).filterGroupType = '';
    (el as any).filterOwnedByMe = false;
    (el as any).activeTab = 'all';
    (el as any).currentCursor = undefined;
    /* eslint-enable @typescript-eslint/no-explicit-any */

    el.syncFiltersToURL();

    expect(replaceStateSpy).toHaveBeenCalledWith({}, '', '/admin/groups');

    Object.defineProperty(window, 'location', {
      value: originalLocation,
      writable: true,
    });
  });
});

/* ========================================================================== */
/* Pagination stack push/pop                                                   */
/* ========================================================================== */

describe('Pagination stack', () => {
  it('starts at page 1 with empty stack', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();
    expect(el.currentPageNumber).toBe(1);
    expect(el.hasPreviousPage).toBe(false);
  });

  it('pushes current cursor onto stack when going next', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    // Stub loadData and syncFiltersToURL to prevent side effects
    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).syncFiltersToURL = vi.fn();
    (el as any).loadData = vi.fn().mockResolvedValue(undefined);
    (el as any).nextCursor = 'page2-cursor';
    /* eslint-enable @typescript-eslint/no-explicit-any */

    el.goNextPage();

    expect(el.currentPageNumber).toBe(2);
    expect(el.hasPreviousPage).toBe(true);
    /* eslint-disable @typescript-eslint/no-explicit-any */
    expect((el as any).currentCursor).toBe('page2-cursor');
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });

  it('pops from stack when going previous', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).syncFiltersToURL = vi.fn();
    (el as any).loadData = vi.fn().mockResolvedValue(undefined);
    (el as any).nextCursor = 'page2-cursor';
    /* eslint-enable @typescript-eslint/no-explicit-any */

    // Go to page 2
    el.goNextPage();
    expect(el.currentPageNumber).toBe(2);

    // Set up next cursor for page 3
    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).nextCursor = 'page3-cursor';
    /* eslint-enable @typescript-eslint/no-explicit-any */

    // Go to page 3
    el.goNextPage();
    expect(el.currentPageNumber).toBe(3);

    // Go back to page 2
    el.goPreviousPage();
    expect(el.currentPageNumber).toBe(2);

    // Go back to page 1
    el.goPreviousPage();
    expect(el.currentPageNumber).toBe(1);
    expect(el.hasPreviousPage).toBe(false);
  });

  it('resets pagination clears stack and cursor', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).syncFiltersToURL = vi.fn();
    (el as any).loadData = vi.fn().mockResolvedValue(undefined);
    (el as any).nextCursor = 'page2-cursor';
    /* eslint-enable @typescript-eslint/no-explicit-any */

    el.goNextPage();
    expect(el.currentPageNumber).toBe(2);

    el.resetPagination();
    expect(el.currentPageNumber).toBe(1);
    expect(el.hasPreviousPage).toBe(false);
    expect(el.hasNextPage).toBe(false);
  });

  it('does nothing when going next without nextCursor', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).nextCursor = undefined;
    /* eslint-enable @typescript-eslint/no-explicit-any */

    el.goNextPage();
    expect(el.currentPageNumber).toBe(1);
  });

  it('does nothing when going previous with empty stack', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    el.goPreviousPage();
    expect(el.currentPageNumber).toBe(1);
  });
});

/* ========================================================================== */
/* Filter helpers                                                              */
/* ========================================================================== */

describe('Filter helpers (hasActiveFilters, activeFilterCount)', () => {
  it('reports no active filters by default', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();
    expect(el.hasActiveFilters).toBe(false);
    expect(el.activeFilterCount).toBe(0);
  });

  it('counts each active filter', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).searchQuery = 'test';
    expect(el.hasActiveFilters).toBe(true);
    expect(el.activeFilterCount).toBe(1);

    (el as any).filterGroupType = 'explicit';
    expect(el.activeFilterCount).toBe(2);

    (el as any).filterOwnedByMe = true;
    expect(el.activeFilterCount).toBe(3);
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });
});

/* ========================================================================== */
/* Empty-state variants (fixture-driven)                                       */
/* ========================================================================== */

describe('Empty-state variant logic', () => {
  it('distinguishes filtered-empty from truly-empty via hasActiveFilters', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    // No filters = truly empty
    expect(el.hasActiveFilters).toBe(false);

    // With filter = filtered empty
    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).searchQuery = 'nonexistent';
    /* eslint-enable @typescript-eslint/no-explicit-any */
    expect(el.hasActiveFilters).toBe(true);
  });

  it('shows create CTA only with create capability', () => {
    // canGroup with create capability
    expect(canGroup({ actions: ['create', 'list'] }, 'create')).toBe(true);
    // Without create capability
    expect(canGroup({ actions: ['list'] }, 'create')).toBe(false);
    // Undefined capabilities
    expect(canGroup(undefined, 'create')).toBe(false);
  });
});

/* ========================================================================== */
/* Slugify mirror + detach behavior                                            */
/* ========================================================================== */

describe('slugify', () => {
  it('converts a name to a lowercase dash-separated slug', () => {
    expect(slugify('Platform Engineers')).toBe('platform-engineers');
  });

  it('handles multiple spaces and special characters', () => {
    expect(slugify('My  Cool  Group!')).toBe('my-cool-group');
  });

  it('trims leading and trailing dashes', () => {
    expect(slugify('  Hello World  ')).toBe('hello-world');
  });

  it('handles empty string', () => {
    expect(slugify('')).toBe('');
  });

  it('preserves numbers', () => {
    expect(slugify('Team 42')).toBe('team-42');
  });

  it('collapses consecutive non-alphanumeric chars into a single dash', () => {
    expect(slugify('a---b___c')).toBe('a-b-c');
  });

  it('handles unicode by removing non-ascii', () => {
    expect(slugify('Über Cool')).toBe('ber-cool');
  });
});

describe('slug detach behavior', () => {
  it('auto-fills slug from name by default', async () => {
    const { ScionGroupFormDialog } = await import('../shared/group-form-dialog.js');
    const el = new ScionGroupFormDialog();

    // Simulate name input
    /* eslint-disable @typescript-eslint/no-explicit-any */
    const nameEvent = { target: { value: 'Test Group' } } as unknown as Event;
    (el as any).onNameInput(nameEvent);
    expect((el as any).formSlug).toBe('test-group');
    expect((el as any).slugDetached).toBe(false);
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });

  it('detaches slug from name on manual slug edit', async () => {
    const { ScionGroupFormDialog } = await import('../shared/group-form-dialog.js');
    const el = new ScionGroupFormDialog();

    // Type a name
    /* eslint-disable @typescript-eslint/no-explicit-any */
    const nameEvent = { target: { value: 'Test Group' } } as unknown as Event;
    (el as any).onNameInput(nameEvent);

    // Manually edit slug
    const slugEvent = { target: { value: 'custom-slug' } } as unknown as Event;
    (el as any).onSlugInput(slugEvent);
    expect((el as any).formSlug).toBe('custom-slug');
    expect((el as any).slugDetached).toBe(true);

    // Further name changes should NOT update slug
    const nameEvent2 = { target: { value: 'Another Name' } } as unknown as Event;
    (el as any).onNameInput(nameEvent2);
    expect((el as any).formSlug).toBe('custom-slug');
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });
});

/* ========================================================================== */
/* Label-editor validation                                                     */
/* ========================================================================== */

describe('Label-editor validation', () => {
  it('rejects empty label keys', async () => {
    const { ScionGroupFormDialog } = await import('../shared/group-form-dialog.js');
    const el = new ScionGroupFormDialog();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).formName = 'Test';
    (el as any).formLabels = [{ key: '', value: 'val' }];
    const valid = el.validate();
    expect(valid).toBe(false);
    expect((el as any).labelErrors.get(0)).toContain('empty');
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });

  it('rejects duplicate label keys', async () => {
    const { ScionGroupFormDialog } = await import('../shared/group-form-dialog.js');
    const el = new ScionGroupFormDialog();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).formName = 'Test';
    (el as any).formLabels = [
      { key: 'env', value: 'prod' },
      { key: 'env', value: 'staging' },
    ];
    const valid = el.validate();
    expect(valid).toBe(false);
    expect((el as any).labelErrors.get(1)).toContain('Duplicate');
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });

  it('accepts valid labels', async () => {
    const { ScionGroupFormDialog } = await import('../shared/group-form-dialog.js');
    const el = new ScionGroupFormDialog();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).formName = 'Test';
    (el as any).formLabels = [
      { key: 'env', value: 'prod' },
      { key: 'team', value: 'platform' },
    ];
    const valid = el.validate();
    expect(valid).toBe(true);
    expect((el as any).labelErrors.size).toBe(0);
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });

  it('rejects project: prefix in slug', async () => {
    const { ScionGroupFormDialog } = await import('../shared/group-form-dialog.js');
    const el = new ScionGroupFormDialog();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).formName = 'Test';
    (el as any).formSlug = 'project:my-group';
    const valid = el.validate();
    expect(valid).toBe(false);
    expect((el as any).slugError).toContain('project:');
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });

  it('rejects empty name', async () => {
    const { ScionGroupFormDialog } = await import('../shared/group-form-dialog.js');
    const el = new ScionGroupFormDialog();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).formName = '   ';
    const valid = el.validate();
    expect(valid).toBe(false);
    expect((el as any).nameError).toContain('required');
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });

  it('passes validation with valid inputs', async () => {
    const { ScionGroupFormDialog } = await import('../shared/group-form-dialog.js');
    const el = new ScionGroupFormDialog();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    (el as any).formName = 'Valid Group';
    (el as any).formSlug = 'valid-group';
    const valid = el.validate();
    expect(valid).toBe(true);
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });
});

/* ========================================================================== */
/* Error kind → surface mapping                                                */
/* ========================================================================== */

describe('Error kind → surface mapping', () => {
  it('GroupsApiError with conflict_slug kind is identifiable', async () => {
    const { GroupsApiError: GAE } = await import('../../client/groups-api.js');
    const err = new GAE('conflict_slug', 'Group with this slug already exists', 409);
    expect(err.kind).toBe('conflict_slug');
    expect(err.httpStatus).toBe(409);
    expect(err.message).toContain('slug');
  });

  it('GroupsApiError with validation kind is identifiable', async () => {
    const { GroupsApiError: GAE } = await import('../../client/groups-api.js');
    const err = new GAE('validation', 'Name is required', 400);
    expect(err.kind).toBe('validation');
  });

  it('GroupsApiError with forbidden kind is identifiable', async () => {
    const { GroupsApiError: GAE } = await import('../../client/groups-api.js');
    const err = new GAE('forbidden', 'Access denied', 403);
    expect(err.kind).toBe('forbidden');
    expect(err.httpStatus).toBe(403);
  });

  it('GroupsApiError with http kind is the fallback', async () => {
    const { GroupsApiError: GAE } = await import('../../client/groups-api.js');
    const err = new GAE('http', 'Internal server error', 500);
    expect(err.kind).toBe('http');
  });
});

/* ========================================================================== */
/* Tab switching                                                               */
/* ========================================================================== */

describe('Tab switching', () => {
  it('defaults to "all" tab', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();
    /* eslint-disable @typescript-eslint/no-explicit-any */
    expect((el as any).activeTab).toBe('all');
    /* eslint-enable @typescript-eslint/no-explicit-any */
  });

  it('reads mine tab from URL', async () => {
    const { ScionPageAdminGroups } = await import('./admin-groups.js');
    const el = new ScionPageAdminGroups();

    const origLocation = window.location;
    Object.defineProperty(window, 'location', {
      value: { ...origLocation, search: '?tab=mine', pathname: '/admin/groups' },
      writable: true,
    });

    el.readFiltersFromURL();

    /* eslint-disable @typescript-eslint/no-explicit-any */
    expect((el as any).activeTab).toBe('mine');
    /* eslint-enable @typescript-eslint/no-explicit-any */

    Object.defineProperty(window, 'location', {
      value: origLocation,
      writable: true,
    });
  });
});

/* ========================================================================== */
/* G6: Accessibility assertions                                                */
/* ========================================================================== */

describe('Accessibility (G6 sweep)', () => {
  const { readFileSync } = require('fs');
  const { resolve } = require('path');
  const LIST_SOURCE = readFileSync(resolve(__dirname, './admin-groups.ts'), 'utf-8');
  const FORM_SOURCE = readFileSync(
    resolve(__dirname, '../shared/group-form-dialog.ts'),
    'utf-8'
  );

  it('groups table has role="table" and aria-label', () => {
    expect(LIST_SOURCE).toContain('role="table"');
    expect(LIST_SOURCE).toContain('aria-label="Groups"');
  });

  it('groups table has a visually-hidden caption', () => {
    expect(LIST_SOURCE).toContain('<caption class="sr-only">');
  });

  it('groups table headers have scope="col"', () => {
    expect(LIST_SOURCE).toContain('scope="col"');
  });

  it('result count region uses aria-live="polite"', () => {
    expect(LIST_SOURCE).toContain('aria-live="polite"');
    expect(LIST_SOURCE).toContain('role="status"');
  });

  it('decorative group icons have aria-hidden', () => {
    expect(LIST_SOURCE).toMatch(/group-icon[^"]*"[^>]*aria-hidden="true"/);
  });

  it('error states use role="alert"', () => {
    expect(LIST_SOURCE).toMatch(/error-state[^"]*"[^>]*role="alert"/);
  });

  it('form dialog banner error uses role="alert"', () => {
    expect(FORM_SOURCE).toContain('role="alert"');
  });

  it('form dialog has focusFirstError for validation failures', () => {
    expect(FORM_SOURCE).toContain('focusFirstError');
    expect(FORM_SOURCE).toContain('[data-error="true"]');
  });

  it('decorative prefix icons are aria-hidden in list page', () => {
    // All sl-icon elements with slot="prefix" should be aria-hidden
    const prefixIcons = LIST_SOURCE.match(/<sl-icon[^>]*slot="prefix"[^>]*>/g) ?? [];
    expect(prefixIcons.length).toBeGreaterThan(0);
    for (const icon of prefixIcons) {
      expect(icon).toContain('aria-hidden="true"');
    }
  });

  it('form dialog suppresses close while submitting', () => {
    expect(FORM_SOURCE).toContain('this.submitting');
    expect(FORM_SOURCE).toContain('e.preventDefault()');
  });

  it('create button has an id for focus return', () => {
    expect(LIST_SOURCE).toContain('id="create-group-btn"');
  });
});
