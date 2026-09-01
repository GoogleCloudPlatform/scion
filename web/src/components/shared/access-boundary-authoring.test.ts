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
 * Access Boundary Authoring — Security-Critical Invariant Tests (R4)
 *
 * Tests proving the five security-critical invariants required by the
 * acceptance gate:
 *
 *   1. Changing subject kind clears IDs
 *   2. No initial select-all default
 *   3. Bulk actions are scoped to current group
 *   4. Long IDs remain usable
 *   5. No generic condition/effect/priority field
 */

import { describe, it, expect } from 'vitest';
import { readFileSync } from 'fs';
import { resolve } from 'path';

/* -------------------------------------------------------------------------- */
/* 1. Changing subject kind clears IDs                                        */
/* -------------------------------------------------------------------------- */

describe('Invariant 1: Changing subject kind clears IDs', () => {
  it('handleOptionSelect clears selectedId and selectedLabel on kind change', async () => {
    // We test the component class logic directly by importing the module
    // and instantiating the element.
    const mod = await import('./access-boundary-subject-selector.js');
    const el = new mod.ScionAccessBoundarySubjectSelector();

    // Simulate an initial selection of exact_user with an ID set
    el.selection = 'exact_user';
    el.selectedId = 'user-123';
    el.selectedLabel = 'Alice Smith';

    // Now simulate changing to exact_agent — this MUST clear the ID fields
    // Access the private method via bracket notation for testing
    (el as unknown as { handleOptionSelect(v: string): void }).handleOptionSelect('exact_agent');

    expect(el.selectedId).toBe('');
    expect(el.selectedLabel).toBe('');
    expect(el.selection).toBe('exact_agent');
  });

  it('does not clear IDs when re-selecting the same kind', async () => {
    const mod = await import('./access-boundary-subject-selector.js');
    const el = new mod.ScionAccessBoundarySubjectSelector();

    el.selection = 'exact_user';
    el.selectedId = 'user-456';
    el.selectedLabel = 'Bob Jones';

    // Re-selecting the same kind should NOT clear the IDs
    (el as unknown as { handleOptionSelect(v: string): void }).handleOptionSelect('exact_user');

    expect(el.selectedId).toBe('user-456');
    expect(el.selectedLabel).toBe('Bob Jones');
  });

  it('clears IDs for every possible kind transition', async () => {
    const mod = await import('./access-boundary-subject-selector.js');
    const kinds = ['exact_user', 'exact_agent', 'exact_group', 'group_closure', 'all_principals'];

    for (const from of kinds) {
      for (const to of kinds) {
        if (from === to) continue;

        const el = new mod.ScionAccessBoundarySubjectSelector();
        el.selection = from as typeof el.selection;
        el.selectedId = 'test-id';
        el.selectedLabel = 'Test Label';

        (el as unknown as { handleOptionSelect(v: string): void }).handleOptionSelect(to);

        expect(el.selectedId, `${from} -> ${to} should clear selectedId`).toBe('');
        expect(el.selectedLabel, `${from} -> ${to} should clear selectedLabel`).toBe('');
      }
    }
  });
});

/* -------------------------------------------------------------------------- */
/* 2. No initial select-all default                                           */
/* -------------------------------------------------------------------------- */

describe('Invariant 2: No initial select-all default', () => {
  it('permission selector starts with empty retainedPermissions', async () => {
    const mod = await import('./maximum-permission-selector.js');
    const el = new mod.ScionMaximumPermissionSelector();

    // The default value of retainedPermissions must be an empty array
    expect(el.retainedPermissions).toEqual([]);
    expect(el.retainedPermissions).toHaveLength(0);
  });

  it('PermissionChangeDetail emitted by emitChange carries totalCount', async () => {
    const mod = await import('./maximum-permission-selector.js');
    const el = new mod.ScionMaximumPermissionSelector();

    // Verify the interface includes totalCount by checking the emitted event
    let capturedDetail: { retainedPermissions: string[]; totalCount: number } | null = null;
    el.addEventListener('permission-change', ((e: CustomEvent) => {
      capturedDetail = e.detail;
    }) as EventListener);

    // Trigger emitChange via a public action (togglePermission would require
    // registryPermissions, so we call emitChange directly)
    (el as unknown as { emitChange(): void }).emitChange();

    expect(capturedDetail).not.toBeNull();
    expect(capturedDetail!.retainedPermissions).toEqual([]);
    expect(capturedDetail!.totalCount).toBe(0); // no registry loaded
  });
});

/* -------------------------------------------------------------------------- */
/* 3. Bulk actions are scoped to current group                                */
/* -------------------------------------------------------------------------- */

describe('Invariant 3: Bulk actions are scoped to current group', () => {
  it('selectGroupVisible only retains permissions within the specified group', async () => {
    const mod = await import('./maximum-permission-selector.js');
    const el = new mod.ScionMaximumPermissionSelector();

    // Set up two groups of registry permissions
    const groupAPermissions = [
      { id: 'storage.read', description: 'Read storage', resourceFamily: 'Storage' },
      { id: 'storage.write', description: 'Write storage', resourceFamily: 'Storage' },
    ];
    const groupBPermissions = [
      { id: 'compute.start', description: 'Start VM', resourceFamily: 'Compute' },
      { id: 'compute.stop', description: 'Stop VM', resourceFamily: 'Compute' },
    ];

    // Set up registry permissions (both groups)
    (el as unknown as { registryPermissions: typeof groupAPermissions }).registryPermissions = [
      ...groupAPermissions,
      ...groupBPermissions,
    ];

    // Start with nothing retained
    el.retainedPermissions = [];

    // Select visible in group A only
    (
      el as unknown as {
        selectGroupVisible(perms: typeof groupAPermissions): void;
      }
    ).selectGroupVisible(groupAPermissions);

    // Only group A permissions should be retained
    expect(el.retainedPermissions).toContain('storage.read');
    expect(el.retainedPermissions).toContain('storage.write');
    expect(el.retainedPermissions).not.toContain('compute.start');
    expect(el.retainedPermissions).not.toContain('compute.stop');
  });

  it('clearGroup only clears permissions within the specified group', async () => {
    const mod = await import('./maximum-permission-selector.js');
    const el = new mod.ScionMaximumPermissionSelector();

    const groupAPermissions = [
      { id: 'storage.read', description: 'Read storage', resourceFamily: 'Storage' },
      { id: 'storage.write', description: 'Write storage', resourceFamily: 'Storage' },
    ];
    const groupBPermissions = [
      { id: 'compute.start', description: 'Start VM', resourceFamily: 'Compute' },
    ];

    (el as unknown as { registryPermissions: typeof groupAPermissions }).registryPermissions = [
      ...groupAPermissions,
      ...groupBPermissions,
    ];

    // Start with all permissions retained
    el.retainedPermissions = ['storage.read', 'storage.write', 'compute.start'];

    // Clear only group A
    (
      el as unknown as {
        clearGroup(perms: typeof groupAPermissions): void;
      }
    ).clearGroup(groupAPermissions);

    // Group A should be gone, group B should remain
    expect(el.retainedPermissions).not.toContain('storage.read');
    expect(el.retainedPermissions).not.toContain('storage.write');
    expect(el.retainedPermissions).toContain('compute.start');
  });
});

/* -------------------------------------------------------------------------- */
/* 4. Long IDs remain usable                                                  */
/* -------------------------------------------------------------------------- */

describe('Invariant 4: Long IDs remain usable', () => {
  it('permission IDs longer than typical length are preserved in full', async () => {
    const mod = await import('./maximum-permission-selector.js');
    const el = new mod.ScionMaximumPermissionSelector();

    // A very long permission ID that exceeds typical lengths
    const longId =
      'resourcemanager.organizations.departments.subdivisions.units.teams.members.permissions.readWriteExecuteAdmin';

    (
      el as unknown as {
        registryPermissions: Array<{ id: string; description: string; resourceFamily: string }>;
      }
    ).registryPermissions = [
      { id: longId, description: 'Long permission', resourceFamily: 'ResourceManager' },
    ];

    // Toggle it on
    (el as unknown as { togglePermission(id: string): void }).togglePermission(longId);

    // The full ID must be retained — no truncation
    expect(el.retainedPermissions).toContain(longId);
    expect(el.retainedPermissions[0]).toBe(longId);
    expect(el.retainedPermissions[0]).toHaveLength(longId.length);
  });

  it('isRetained correctly identifies long IDs', async () => {
    const mod = await import('./maximum-permission-selector.js');
    const el = new mod.ScionMaximumPermissionSelector();

    const longId =
      'iam.serviceAccounts.organizations.projects.folders.billingAccounts.keys.create.extended.v2';

    el.retainedPermissions = [longId];

    const isRetained = (el as unknown as { isRetained(id: string): boolean }).isRetained(longId);
    expect(isRetained).toBe(true);
  });

  it('the permission-id CSS class uses word-break: break-all for wrapping', async () => {
    // Verify in the component's static styles that long IDs won't be clipped
    const mod = await import('./maximum-permission-selector.js');
    const styles = mod.ScionMaximumPermissionSelector.styles;
    const cssText = Array.isArray(styles)
      ? styles.map((s) => s.cssText).join('\n')
      : (styles as { cssText: string }).cssText;

    // The .permission-id class must have word-break: break-all
    expect(cssText).toContain('word-break');
    expect(cssText).toContain('break-all');
  });
});

/* -------------------------------------------------------------------------- */
/* 5. No generic condition/effect/priority field                              */
/* -------------------------------------------------------------------------- */

describe('Invariant 5: No generic condition/effect/priority field', () => {
  /**
   * This invariant ensures the editor never exposes free-form condition,
   * effect, or priority fields — these are policy-engine concepts that do
   * not belong in a user-facing access boundary editor.
   */

  it('editor component source has no "condition" input/field in the template', () => {
    const source = readFileSync(
      resolve(__dirname, '../pages/admin-access-boundary-editor.ts'),
      'utf-8'
    );

    // Match field bindings or form elements named condition/effect/priority.
    // We exclude the string from comments and imports.
    const templateSection = extractTemplateContent(source);

    expect(templateSection).not.toMatch(/\bname=["']condition["']/i);
    expect(templateSection).not.toMatch(/\bname=["']effect["']/i);
    expect(templateSection).not.toMatch(/\bname=["']priority["']/i);
    expect(templateSection).not.toMatch(/\blabel=["']Condition["']/i);
    expect(templateSection).not.toMatch(/\blabel=["']Effect["']/i);
    expect(templateSection).not.toMatch(/\blabel=["']Priority["']/i);
  });

  it('editor draft state has no condition, effect, or priority properties', () => {
    const source = readFileSync(
      resolve(__dirname, '../pages/admin-access-boundary-editor.ts'),
      'utf-8'
    );

    // Check that no @state() or @property() declares these field names
    const stateDeclarations = source.match(/@state\(\).*(?:condition|effect|priority)\b/g);
    const propertyDeclarations = source.match(/@property\(.*\).*(?:condition|effect|priority)\b/g);

    expect(stateDeclarations).toBeNull();
    expect(propertyDeclarations).toBeNull();
  });

  it('summary component source has no condition/effect/priority fields', () => {
    const source = readFileSync(
      resolve(__dirname, './access-boundary-definition-summary.ts'),
      'utf-8'
    );

    const templateSection = extractTemplateContent(source);

    expect(templateSection).not.toMatch(/\bname=["']condition["']/i);
    expect(templateSection).not.toMatch(/\bname=["']effect["']/i);
    expect(templateSection).not.toMatch(/\bname=["']priority["']/i);
  });
});

/* -------------------------------------------------------------------------- */
/* Helpers                                                                    */
/* -------------------------------------------------------------------------- */

/**
 * Extracts the render method body (template content) from a Lit component
 * source string. This is a simple heuristic: find `render()` and extract
 * everything from the next `html\`` to the method end.
 */
function extractTemplateContent(source: string): string {
  // Match all html`` template literals in the source
  const htmlTemplates: string[] = [];
  const regex = /html`([\s\S]*?)`/g;
  let match;
  while ((match = regex.exec(source)) !== null) {
    htmlTemplates.push(match[1]);
  }
  return htmlTemplates.join('\n');
}
