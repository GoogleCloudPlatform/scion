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
 * Owner field on project members groups (ptone/scion#2672 item 5). The hub
 * rejects any ownerId on a project members group (ptone/scion#2668), so the
 * form disables the owner picker and never sends one.
 */

import { describe, it, expect, afterEach } from 'vitest';

import {
  PROJECT_MEMBERS_GROUP_ANNOTATION,
  LEGACY_PROJECT_MEMBERS_GROUP_ANNOTATION,
  isProjectMembersGroup,
} from '../../shared/groups.js';
import type { AdminGroup } from '../../shared/groups.js';
import { ScionGroupFormDialog } from './group-form-dialog.js';

interface DialogInternals {
  editOwnerId: string;
  formName: string;
  buildPatch(): Record<string, unknown> | null;
}

function adminGroup(extra: Partial<AdminGroup> = {}): AdminGroup {
  return {
    id: 'g-1',
    name: 'Group',
    slug: 'group',
    groupType: 'explicit',
    created: '2026-10-01T00:00:00Z',
    updated: '2026-10-01T00:00:00Z',
    ...extra,
  };
}

const MEMBERS_GROUP = adminGroup({
  name: 'Project Members',
  slug: 'project:demo:members',
  projectId: 'p-1',
  annotations: { [PROJECT_MEMBERS_GROUP_ANNOTATION]: 'true' },
});

const LEGACY_MEMBERS_GROUP = adminGroup({
  projectId: 'p-1',
  annotations: { [LEGACY_PROJECT_MEMBERS_GROUP_ANNOTATION]: 'true' },
});

const PLAIN_GROUP = adminGroup({ ownerId: 'u-alice' });

const mounted: HTMLElement[] = [];

async function mountEdit(group: AdminGroup): Promise<ScionGroupFormDialog> {
  const el = new ScionGroupFormDialog();
  el.mode = 'edit';
  el.group = group;
  document.body.appendChild(el);
  mounted.push(el);
  el.show();
  await el.updateComplete;
  return el;
}

function picker(el: ScionGroupFormDialog): HTMLElement {
  const p = el.shadowRoot!.querySelector<HTMLElement>('scion-principal-picker[label="Owner"]');
  if (!p) throw new Error('no owner picker');
  return p;
}

afterEach(() => {
  for (const el of mounted.splice(0)) el.remove();
});

describe('isProjectMembersGroup', () => {
  it('matches either marker key on a project group', () => {
    expect(isProjectMembersGroup(MEMBERS_GROUP)).toBe(true);
    expect(isProjectMembersGroup(LEGACY_MEMBERS_GROUP)).toBe(true);
  });

  it('does not match plain groups, unset markers or groups without a project', () => {
    expect(isProjectMembersGroup(PLAIN_GROUP)).toBe(false);
    expect(isProjectMembersGroup(null)).toBe(false);
    expect(
      isProjectMembersGroup({
        projectId: 'p-1',
        annotations: { [PROJECT_MEMBERS_GROUP_ANNOTATION]: 'false' },
      })
    ).toBe(false);
    expect(
      isProjectMembersGroup({ annotations: { [PROJECT_MEMBERS_GROUP_ANNOTATION]: 'true' } })
    ).toBe(false);
  });
});

describe('group form owner field', () => {
  for (const [name, group] of [
    ['project members group', MEMBERS_GROUP],
    ['legacy-marked project members group', LEGACY_MEMBERS_GROUP],
  ] as const) {
    it(`is disabled with help text on a ${name}`, async () => {
      const el = await mountEdit(group);
      expect(picker(el).hasAttribute('disabled')).toBe(true);
      const help = el.shadowRoot!.querySelector('.owner-managed');
      expect(help?.textContent?.replace(/\s+/g, ' ').trim()).toBe(
        "Project members groups have no owner; access is managed through the project's members."
      );
    });
  }

  it('never puts ownerId in the PATCH for a project members group', async () => {
    const el = await mountEdit(MEMBERS_GROUP);
    const i = el as unknown as DialogInternals;
    i.editOwnerId = 'u-mallory';
    expect(i.buildPatch()).toBeNull();
  });

  it('sends only the other fields on a mixed edit of a project members group', async () => {
    const el = await mountEdit(MEMBERS_GROUP);
    const i = el as unknown as DialogInternals;
    i.formName = 'Renamed Members';
    i.editOwnerId = 'u-mallory';
    const patch = i.buildPatch();
    expect(patch).toEqual({ name: 'Renamed Members' });
    expect(patch).not.toHaveProperty('ownerId');
  });

  it('stays editable on an ordinary group', async () => {
    const el = await mountEdit(PLAIN_GROUP);
    expect(picker(el).hasAttribute('disabled')).toBe(false);
    expect(el.shadowRoot!.querySelector('.owner-managed')).toBeNull();
    const i = el as unknown as DialogInternals;
    i.editOwnerId = 'u-bob';
    expect(i.buildPatch()).toEqual({ ownerId: 'u-bob' });
  });
});
