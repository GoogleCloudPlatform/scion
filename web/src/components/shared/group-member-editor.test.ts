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
 * Group Member Editor — comprehensive tests
 *
 * Tests covering:
 *   1. Capability split matrix (readOnly × capabilities combinations)
 *   2. Sole-owner disable logic (single owner, multi-owner, non-user-owner)
 *   3. Every error kind → expected inline copy
 *   4. REGRESSION: legacy property set (readOnly, no capabilities) renders identically
 *   5. Self-membership rejection
 *   6. Group picker integration (principal-picker replaces sl-select)
 *   7. Dialog behavior (close prevention, aria-live, role helper copy)
 */

import { describe, it, expect } from 'vitest';
import { readFileSync } from 'fs';
import { resolve } from 'path';

import type { GroupMember } from '../../shared/types.js';
import type { Capabilities } from '../../shared/groups.js';
import { GroupsApiError } from '../../client/groups-api.js';

/** Read the component source once — many tests inspect it. */
const SOURCE = readFileSync(resolve(__dirname, './group-member-editor.ts'), 'utf-8');

/** Extract html`` template content from source for template-level assertions. */
function extractTemplateContent(source: string): string {
  const htmlTemplates: string[] = [];
  const regex = /html`([\s\S]*?)`/g;
  let match;
  while ((match = regex.exec(source)) !== null) {
    htmlTemplates.push(match[1]);
  }
  return htmlTemplates.join('\n');
}

/* ====================================================================== */
/* Helpers                                                                 */
/* ====================================================================== */

/** Create a component instance for logic testing. */
async function createEditor(
  overrides: { readOnly?: boolean; capabilities?: Capabilities; groupId?: string } = {}
) {
  const mod = await import('./group-member-editor.js');
  const el = new mod.ScionGroupMemberEditor();
  el.groupId = overrides.groupId ?? 'g-test';
  if (overrides.readOnly !== undefined) el.readOnly = overrides.readOnly;
  if (overrides.capabilities !== undefined) el.capabilities = overrides.capabilities;
  return el;
}

/** Fixture members matching the frozen fixtures in __fixtures__/groups/members.json. */
function makeMembers(overrides?: Partial<GroupMember>[]): GroupMember[] {
  const defaults: GroupMember[] = [
    {
      groupId: 'g-test',
      memberType: 'user',
      memberId: 'u-alice',
      displayName: 'Alice Admin',
      role: 'owner',
      addedAt: '2026-01-15T10:00:00Z',
    },
    {
      groupId: 'g-test',
      memberType: 'user',
      memberId: 'u-bob',
      displayName: 'Bob Builder',
      role: 'admin',
      addedAt: '2026-02-01T08:30:00Z',
    },
    {
      groupId: 'g-test',
      memberType: 'group',
      memberId: 'g-frontend',
      displayName: 'Frontend Team',
      role: 'member',
      addedAt: '2026-03-10T11:00:00Z',
    },
    {
      groupId: 'g-test',
      memberType: 'agent',
      memberId: 'a-deploy',
      displayName: 'deploy-bot',
      role: 'member',
      addedAt: '2026-04-05T15:00:00Z',
    },
  ];
  if (overrides) {
    return overrides.map((o, i) => ({ ...defaults[i % defaults.length], ...o }));
  }
  return defaults;
}

/* ====================================================================== */
/* 1. Capability split matrix                                              */
/* ====================================================================== */

describe('Capability split matrix (readOnly × capabilities)', () => {
  it('canAdd getter: readOnly=false, no capabilities → true (backward compat)', async () => {
    const el = await createEditor({ readOnly: false });
    // Access private getter via cast
    expect((el as unknown as { canAdd: boolean }).canAdd).toBe(true);
  });

  it('canAdd getter: readOnly=true, no capabilities → false', async () => {
    const el = await createEditor({ readOnly: true });
    expect((el as unknown as { canAdd: boolean }).canAdd).toBe(false);
  });

  it('canAdd getter: readOnly=false, capabilities includes addMember → true', async () => {
    const el = await createEditor({
      readOnly: false,
      capabilities: { actions: ['read', 'addMember'] },
    });
    expect((el as unknown as { canAdd: boolean }).canAdd).toBe(true);
  });

  it('canAdd getter: readOnly=false, capabilities does NOT include addMember → false', async () => {
    const el = await createEditor({
      readOnly: false,
      capabilities: { actions: ['read', 'removeMember'] },
    });
    expect((el as unknown as { canAdd: boolean }).canAdd).toBe(false);
  });

  it('canAdd getter: readOnly=true, capabilities includes addMember → false (readOnly wins)', async () => {
    const el = await createEditor({
      readOnly: true,
      capabilities: { actions: ['read', 'addMember'] },
    });
    expect((el as unknown as { canAdd: boolean }).canAdd).toBe(false);
  });

  it('canRemove getter: readOnly=false, no capabilities → true (backward compat)', async () => {
    const el = await createEditor({ readOnly: false });
    expect((el as unknown as { canRemove: boolean }).canRemove).toBe(true);
  });

  it('canRemove getter: readOnly=true, no capabilities → false', async () => {
    const el = await createEditor({ readOnly: true });
    expect((el as unknown as { canRemove: boolean }).canRemove).toBe(false);
  });

  it('canRemove getter: readOnly=false, capabilities includes removeMember → true', async () => {
    const el = await createEditor({
      readOnly: false,
      capabilities: { actions: ['read', 'removeMember'] },
    });
    expect((el as unknown as { canRemove: boolean }).canRemove).toBe(true);
  });

  it('canRemove getter: readOnly=false, capabilities does NOT include removeMember → false', async () => {
    const el = await createEditor({
      readOnly: false,
      capabilities: { actions: ['read', 'addMember'] },
    });
    expect((el as unknown as { canRemove: boolean }).canRemove).toBe(false);
  });

  it('canRemove getter: readOnly=true, capabilities includes removeMember → false (readOnly wins)', async () => {
    const el = await createEditor({
      readOnly: true,
      capabilities: { actions: ['read', 'removeMember'] },
    });
    expect((el as unknown as { canRemove: boolean }).canRemove).toBe(false);
  });

  it('template gates add button on canAdd (not bare readOnly)', () => {
    // The add button should be gated by this.canAdd, not !this.readOnly
    expect(SOURCE).toContain('this.canAdd');
    // Verify add button appears in a canAdd conditional
    expect(SOURCE).toMatch(/this\.canAdd[\s\S]{0,300}Add Member/);
  });

  it('template gates remove button on canRemove (not bare readOnly)', () => {
    expect(SOURCE).toContain('this.canRemove');
    // Verify the actions column header uses canRemove
    expect(SOURCE).toMatch(/this\.canRemove[\s\S]{0,200}actions-cell/);
  });
});

/* ====================================================================== */
/* 2. Sole-owner disable logic                                             */
/* ====================================================================== */

describe('Sole-owner disable logic', () => {
  it('single user owner: isSoleUserOwner returns true', async () => {
    const el = await createEditor();
    const members = makeMembers();
    // Set members via cast (private state)
    (el as unknown as { members: GroupMember[] }).members = members;

    const alice = members[0]; // owner, user
    expect(
      (el as unknown as { isSoleUserOwner(m: GroupMember): boolean }).isSoleUserOwner(alice)
    ).toBe(true);
  });

  it('multi user owner: isSoleUserOwner returns false for each', async () => {
    const el = await createEditor();
    const members: GroupMember[] = [
      ...makeMembers(),
      {
        groupId: 'g-test',
        memberType: 'user',
        memberId: 'u-charlie',
        displayName: 'Charlie',
        role: 'owner',
        addedAt: '2026-05-01T00:00:00Z',
      },
    ];
    (el as unknown as { members: GroupMember[] }).members = members;

    const alice = members[0]; // owner, user
    expect(
      (el as unknown as { isSoleUserOwner(m: GroupMember): boolean }).isSoleUserOwner(alice)
    ).toBe(false);
  });

  it('non-user owner (group owner): isSoleUserOwner returns false', async () => {
    const el = await createEditor();
    const members: GroupMember[] = [
      {
        groupId: 'g-test',
        memberType: 'group',
        memberId: 'g-owners',
        displayName: 'Owners Group',
        role: 'owner',
        addedAt: '2026-01-01T00:00:00Z',
      },
    ];
    (el as unknown as { members: GroupMember[] }).members = members;

    expect(
      (el as unknown as { isSoleUserOwner(m: GroupMember): boolean }).isSoleUserOwner(members[0])
    ).toBe(false);
  });

  it('non-user owner (agent owner): isSoleUserOwner returns false', async () => {
    const el = await createEditor();
    const members: GroupMember[] = [
      {
        groupId: 'g-test',
        memberType: 'agent',
        memberId: 'a-admin',
        displayName: 'Admin Agent',
        role: 'owner',
        addedAt: '2026-01-01T00:00:00Z',
      },
    ];
    (el as unknown as { members: GroupMember[] }).members = members;

    expect(
      (el as unknown as { isSoleUserOwner(m: GroupMember): boolean }).isSoleUserOwner(members[0])
    ).toBe(false);
  });

  it('admin user: isSoleUserOwner returns false', async () => {
    const el = await createEditor();
    const members = makeMembers();
    (el as unknown as { members: GroupMember[] }).members = members;

    const bob = members[1]; // admin, user
    expect(
      (el as unknown as { isSoleUserOwner(m: GroupMember): boolean }).isSoleUserOwner(bob)
    ).toBe(false);
  });

  it('template renders disabled remove button with tooltip for sole owner', () => {
    // The template must render a tooltip with the sole-owner message
    expect(SOURCE).toContain('A group must keep at least one owner.');
    // And it must be in an sl-tooltip
    expect(SOURCE).toMatch(/sl-tooltip[\s\S]{0,200}A group must keep at least one owner/);
    // The icon-button inside should be disabled
    expect(SOURCE).toMatch(/isSoleUserOwner[\s\S]{0,500}disabled/);
  });
});

/* ====================================================================== */
/* 3. Error kind → expected inline copy                                    */
/* ====================================================================== */

describe('Error surfacing: every error kind → expected inline copy', () => {
  async function getSurfacedError(
    kind: string,
    message: string,
    httpStatus: number
  ): Promise<{ error: string | null; title: string | null; hint: string | null }> {
    const el = await createEditor();
    const apiErr = new GroupsApiError(
      kind as ConstructorParameters<typeof GroupsApiError>[0],
      message,
      httpStatus
    );
    (
      el as unknown as { surfaceAddError(err: GroupsApiError): void }
    ).surfaceAddError(apiErr);

    return {
      error: (el as unknown as { addMemberError: string | null }).addMemberError,
      title: (el as unknown as { addMemberErrorTitle: string | null }).addMemberErrorTitle,
      hint: (el as unknown as { addMemberErrorHint: string | null }).addMemberErrorHint,
    };
  }

  it('cycle → membership cycle copy', async () => {
    const result = await getSurfacedError(
      'cycle',
      'Adding this group would create a cycle in the group hierarchy',
      400
    );
    expect(result.error).toContain('membership cycle');
    expect(result.error).toContain('directly or nested');
  });

  it('quota → member limit copy', async () => {
    const result = await getSurfacedError(
      'quota',
      'quota exceeded: max_members_per_group',
      429
    );
    expect(result.error).toContain('member limit');
    expect(result.error).toContain('max_members_per_group');
    expect(result.error).toContain('Remove a member');
  });

  it('conflict_member → already a member copy', async () => {
    const result = await getSurfacedError(
      'conflict_member',
      'Member already exists in this group',
      409
    );
    expect(result.error).toBe('Already a member of this group.');
  });

  it('hierarchy → server copy verbatim', async () => {
    const serverMessage = 'Only group owners can add owners or admins';
    const result = await getSurfacedError('hierarchy', serverMessage, 403);
    expect(result.error).toBe(serverMessage);
    expect(result.title).toBeNull();
  });

  it('delegation → title + server reason + explanation hint', async () => {
    const serverMessage =
      'Cannot grant authority you do not hold: caller lacks deploy.execute on resource runtime:prod';
    const result = await getSurfacedError('delegation', serverMessage, 403);
    expect(result.title).toBe('Insufficient authority to grant this membership');
    expect(result.error).toBe(serverMessage);
    expect(result.hint).toContain('role-binding authority');
    expect(result.hint).toContain('you can only grant authority you hold');
  });

  it('validation → server message inline', async () => {
    const serverMessage = 'user not found';
    const result = await getSurfacedError('validation', serverMessage, 422);
    expect(result.error).toBe(serverMessage);
    expect(result.title).toBeNull();
  });

  it('fallback (unknown kind) → server message', async () => {
    const serverMessage = 'Unknown server error';
    const result = await getSurfacedError('http', serverMessage, 500);
    expect(result.error).toBe(serverMessage);
    expect(result.title).toBeNull();
  });

  it('dialog error div uses role="alert" for accessibility', () => {
    expect(SOURCE).toMatch(/dialog-error[\s\S]{0,50}role="alert"/);
  });
});

/* ====================================================================== */
/* 4. REGRESSION: legacy property set renders identically                  */
/* ====================================================================== */

describe('REGRESSION: backward compatibility with legacy properties', () => {
  it('capabilities property is optional (defaults to undefined)', async () => {
    const el = await createEditor({ readOnly: false });
    expect(el.capabilities).toBeUndefined();
  });

  it('canAdd === !readOnly when capabilities is unset', async () => {
    const elRW = await createEditor({ readOnly: false });
    const elRO = await createEditor({ readOnly: true });
    expect((elRW as unknown as { canAdd: boolean }).canAdd).toBe(true);
    expect((elRO as unknown as { canAdd: boolean }).canAdd).toBe(false);
  });

  it('canRemove === !readOnly when capabilities is unset', async () => {
    const elRW = await createEditor({ readOnly: false });
    const elRO = await createEditor({ readOnly: true });
    expect((elRW as unknown as { canRemove: boolean }).canRemove).toBe(true);
    expect((elRO as unknown as { canRemove: boolean }).canRemove).toBe(false);
  });

  it('all original properties still exist on the component', async () => {
    const el = await createEditor();
    expect(el).toHaveProperty('groupId');
    expect(el).toHaveProperty('readOnly');
    expect(el).toHaveProperty('compact');
    expect(el).toHaveProperty('sectionTitle');
    expect(el).toHaveProperty('sectionDescription');
  });

  it('component tag name is unchanged', async () => {
    const mod = await import('./group-member-editor.js');
    const el = new mod.ScionGroupMemberEditor();
    expect(el.tagName.toLowerCase()).toBe('scion-group-member-editor');
  });

  it('compact mode renders section wrapper with compact class', () => {
    expect(SOURCE).toContain('class="section compact"');
  });

  it('sectionTitle property controls the heading text', () => {
    expect(SOURCE).toMatch(/\$\{this\.sectionTitle\}/);
  });

  it('sectionDescription renders conditionally', () => {
    expect(SOURCE).toMatch(/this\.sectionDescription[\s\S]{0,100}<p>/);
  });

  it('security-review dialog integration is preserved', () => {
    expect(SOURCE).toContain('scion-security-review-dialog');
    expect(SOURCE).toContain('parseLockoutResponse');
    expect(SOURCE).toContain('parseSecurityReviewResponse');
    expect(SOURCE).toContain('security-review-cancel');
  });

  it('template never uses bare !this.readOnly for add/remove gating', () => {
    const templates = extractTemplateContent(SOURCE);
    // The template should NOT use !this.readOnly directly for button gating.
    // It should use this.canAdd / this.canRemove instead.
    // We check that !this.readOnly does NOT appear in template conditionals
    // (it may appear in the getter definitions, which is correct).
    expect(templates).not.toMatch(/!\s*this\.readOnly/);
  });

  it('readOnly property still controls overall editability', async () => {
    const el = await createEditor({ readOnly: true });
    expect((el as unknown as { canAdd: boolean }).canAdd).toBe(false);
    expect((el as unknown as { canRemove: boolean }).canRemove).toBe(false);
  });
});

/* ====================================================================== */
/* 5. Self-membership rejection                                            */
/* ====================================================================== */

describe('Self-membership rejection', () => {
  it('self-membership check exists in handlePrincipalChange', () => {
    expect(SOURCE).toContain('A group cannot contain itself');
  });

  it('self-membership check also guards handleAddMember', () => {
    // The check should exist in both the change handler (inline feedback)
    // and the submit handler (safety net).
    const matches = SOURCE.match(/A group cannot contain itself/g);
    expect(matches).not.toBeNull();
    expect(matches!.length).toBeGreaterThanOrEqual(2);
  });

  it('self-membership compares addMemberInput to groupId', () => {
    expect(SOURCE).toMatch(/addMemberType\s*===\s*'group'/);
    expect(SOURCE).toMatch(/addMemberInput[\s\S]{0,60}this\.groupId/);
  });
});

/* ====================================================================== */
/* 6. Group picker integration                                             */
/* ====================================================================== */

describe('Group picker: principal-picker replaces sl-select dump', () => {
  it('uses scion-principal-picker with principalType="group"', () => {
    expect(SOURCE).toContain('principalType="group"');
  });

  it('does NOT use the old 100-item sl-select dump for groups', () => {
    // The old implementation loaded groups into availableGroups and rendered
    // them in an sl-select. That pattern should be gone.
    expect(SOURCE).not.toContain('availableGroups');
    expect(SOURCE).not.toContain('groupsLoading');
    expect(SOURCE).not.toContain('loadAvailableGroups');
  });

  it('uses principal-picker for both user and group types', () => {
    const pickerMatches = SOURCE.match(/scion-principal-picker/g);
    expect(pickerMatches).not.toBeNull();
    // Should have at least 3: import, user picker, group picker
    expect(pickerMatches!.length).toBeGreaterThanOrEqual(3);
  });
});

/* ====================================================================== */
/* 7. Dialog behavior & accessibility                                      */
/* ====================================================================== */

describe('Dialog behavior and accessibility', () => {
  it('dialog close is prevented while loading (sl-request-close suppressed)', () => {
    // The sl-request-close handler should check addMemberLoading
    // and call preventDefault when true
    expect(SOURCE).toMatch(/sl-request-close[\s\S]{0,200}addMemberLoading[\s\S]{0,100}preventDefault/);
  });

  it('aria-live="polite" on member count for screen reader announcements', () => {
    expect(SOURCE).toContain('aria-live="polite"');
    // Should be on the member-count span
    expect(SOURCE).toMatch(/member-count[\s\S]{0,30}aria-live="polite"/);
  });

  it('role helper copy explains governance role', () => {
    expect(SOURCE).toContain('Governance role inside this group');
    expect(SOURCE).toContain('does not grant resource permissions');
  });

  it('remove button has accessible label', () => {
    expect(SOURCE).toMatch(/label="Remove member"/);
  });

  it('role is rendered as read-only badge (no change-role control)', () => {
    // The member row renders role as a badge, not a select
    expect(SOURCE).toMatch(/role-badge \$\{member\.role\}/);
    // No sl-select for role in the member row
    const memberRowMethod = SOURCE.match(
      /renderMemberRow[\s\S]*?(?=\n\s+private\s|$)/
    )?.[0];
    expect(memberRowMethod).toBeDefined();
    expect(memberRowMethod).not.toContain('sl-select');
  });
});

/* ====================================================================== */
/* 8. API adapter migration                                                */
/* ====================================================================== */

describe('API adapter migration', () => {
  it('loadMembers uses listMembers from groups-api.ts', () => {
    expect(SOURCE).toContain("import { listMembers, addMember, removeMember, GroupsApiError } from '../../client/groups-api.js'");
    expect(SOURCE).toMatch(/await listMembers\(this\.groupId\)/);
  });

  it('handleAddMember uses addMember from groups-api.ts', () => {
    expect(SOURCE).toMatch(/await addMember\(this\.groupId/);
  });

  it('does NOT import apiFetch or extractApiError', () => {
    // All API calls should go through groups-api.ts adapter
    expect(SOURCE).not.toContain("from '../../client/api.js'");
    expect(SOURCE).not.toContain('extractApiError');
  });

  it('handleRemoveMember uses removeMember from groups-api.ts', () => {
    // removeMember should delegate to the adapter's removeMember()
    // which internally uses raw fetch with credentials:'include'
    // to bypass the global scion:access-denied 403 toast
    const removeMethod = SOURCE.match(
      /handleRemoveMember[\s\S]*?(?=\n\s+\/\*|private\s+format)/
    )?.[0];
    expect(removeMethod).toBeDefined();
    expect(removeMethod).toContain('await removeMember(');
    expect(removeMethod).not.toContain('await fetch(');
  });

  it('catches GroupsApiError in handleAddMember', () => {
    expect(SOURCE).toContain('instanceof GroupsApiError');
    expect(SOURCE).toContain('surfaceAddError');
  });
});

/* ====================================================================== */
/* G6: Accessibility assertions                                            */
/* ====================================================================== */

describe('Accessibility (G6 sweep)', () => {
  const templates = extractTemplateContent(SOURCE);

  it('members table has role="table" and aria-label', () => {
    expect(templates).toContain('role="table"');
    expect(templates).toContain('aria-label="Group members"');
  });

  it('members table has a visually-hidden caption', () => {
    expect(templates).toContain('<caption class="sr-only">');
  });

  it('table headers have scope="col"', () => {
    // All th elements (not thead) should have scope="col"
    const thMatches = SOURCE.match(/<th\b[^>]*>/g) ?? [];
    const thOnly = thMatches.filter((t: string) => !t.startsWith('<thead'));
    expect(thOnly.length).toBeGreaterThan(0);
    for (const th of thOnly) {
      expect(th).toContain('scope="col"');
    }
  });

  it('member count uses aria-live="polite"', () => {
    expect(templates).toContain('aria-live="polite"');
  });

  it('decorative member icons have aria-hidden on parent', () => {
    expect(templates).toMatch(/member-icon[^"]*"[^>]*aria-hidden="true"/);
  });

  it('error states use role="alert"', () => {
    expect(templates).toMatch(/error-state[^"]*"[^>]*role="alert"/);
  });

  it('add-dialog error uses role="alert"', () => {
    expect(templates).toMatch(/dialog-error[^"]*"[^>]*role="alert"/);
  });

  it('decorative prefix icons are aria-hidden', () => {
    // All sl-icon elements with slot="prefix" should be aria-hidden
    const prefixIcons = templates.match(/<sl-icon[^>]*slot="prefix"[^>]*>/g) ?? [];
    expect(prefixIcons.length).toBeGreaterThan(0);
    for (const icon of prefixIcons) {
      expect(icon).toContain('aria-hidden="true"');
    }
  });

  it('remove buttons have accessible label', () => {
    expect(templates).toContain('label="Remove member"');
  });

  it('sr-only utility class is defined in styles', () => {
    expect(SOURCE).toContain('.sr-only');
    expect(SOURCE).toContain('clip: rect(0, 0, 0, 0)');
  });

  it('actions column header has sr-only text', () => {
    expect(SOURCE).toMatch(/actions-cell[^"]*"[^>]*>.*sr-only.*Actions/s);
  });
});
