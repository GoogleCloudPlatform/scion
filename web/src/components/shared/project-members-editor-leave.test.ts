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
 * Project members editor: the signed-in user's own row offers Leave in place
 * of Remove while the capabilities offer a leave (ptone/scion#7494). Other
 * rows keep Remove; without the leave capabilities the row is unchanged.
 */

import { describe, it, expect, vi, afterEach } from 'vitest';

import { apiFetch } from '../../client/api.js';
import type {
  AssignableProjectRole,
  MembershipCapabilities,
  ProjectMemberGroup,
} from '../../shared/types.js';
import { ScionProjectMembersEditor } from './project-members-editor.js';
import { LEAVE_TRANSFER_OWNERSHIP_EVENT } from './project-leave-dialog.js';
import { LEAVE_BLOCKED_LAST_OWNER } from '../../client/project-leave.js';

vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));
vi.mock('./confirm-dialog.js', () => ({ showConfirm: vi.fn(() => Promise.resolve(true)) }));

const OWNER_CAPS: MembershipCapabilities = {
  canManageMembers: true,
  canManageAdmins: true,
  canManageOwners: true,
  canTransfer: true,
  canManageCustomRoles: true,
  actions: [],
};

const MEMBER_CAPS: MembershipCapabilities = {
  canManageMembers: false,
  canManageAdmins: false,
  canManageOwners: false,
  canTransfer: false,
  canManageCustomRoles: false,
  actions: [],
};

function role(id: string, name: string): AssignableProjectRole {
  return { id, name, description: '', roleKind: 'builtin', grantable: true, reason: '' };
}

const R_OWNER = role('r-owner', 'project-owner');
const R_MEMBER = role('r-member', 'project-member');

function group(principalId: string, r: AssignableProjectRole, name: string): ProjectMemberGroup {
  return {
    principalType: 'user',
    principalId,
    principalDisplayName: name,
    builtInRoleName: r.name,
    bindings: [
      {
        id: `b-${principalId}`,
        roleDefinitionId: r.id,
        roleName: r.name,
        principalType: 'user',
        principalId,
        principalDisplayName: name,
        scopeType: 'project',
        scopeId: 'p-1',
        createdAt: '2026-10-01T00:00:00Z',
        source: 'direct',
        roleKind: 'builtin',
      },
    ],
  };
}

const ALICE = group('u-alice', R_OWNER, 'Alice Owner');
const ODIN = group('u-odin', R_OWNER, 'Odin Owner');
const DAVE = group('u-dave', R_MEMBER, 'Dave Member');

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const mounted: HTMLElement[] = [];

async function mount(
  groups: ProjectMemberGroup[],
  caps: MembershipCapabilities,
  opts: { currentUserId: string; readOnly?: boolean; openTransferOnLoad?: boolean }
): Promise<ScionProjectMembersEditor> {
  vi.mocked(apiFetch).mockImplementation((url: string) => {
    if (url.includes('/members?groupBy=principal')) {
      return Promise.resolve(
        json(200, { items: groups, totalCount: groups.length, _capabilities: caps })
      );
    }
    if (url.endsWith('/members/assignable-roles')) {
      return Promise.resolve(json(200, { items: [R_OWNER, R_MEMBER] }));
    }
    return Promise.reject(new Error(`unexpected ${url}`));
  });
  const el = new ScionProjectMembersEditor();
  el.projectId = 'p-1';
  el.projectName = 'Project One';
  el.currentUserId = opts.currentUserId;
  el.readOnly = opts.readOnly ?? false;
  el.openTransferOnLoad = opts.openTransferOnLoad ?? false;
  document.body.appendChild(el);
  mounted.push(el);
  await vi.waitFor(() => expect((el as unknown as { loading: boolean }).loading).toBe(false));
  await el.updateComplete;
  return el;
}

function row(el: HTMLElement, principalId: string): HTMLElement {
  return el.shadowRoot!.querySelector<HTMLElement>(`tr[data-principal="user:${principalId}"]`)!;
}

afterEach(() => {
  mounted.splice(0).forEach((el) => el.remove());
  vi.mocked(apiFetch).mockReset();
  vi.restoreAllMocks();
});

describe('members editor: self row Leave', () => {
  it("replaces the owner's Remove-self with Leave; other rows keep Remove", async () => {
    const el = await mount(
      [ALICE, ODIN, DAVE],
      { ...OWNER_CAPS, canLeave: true },
      {
        currentUserId: 'u-alice',
      }
    );
    const self = row(el, 'u-alice');
    expect(self.querySelector('[data-self-leave]')).not.toBeNull();
    expect(self.querySelector('sl-icon-button[name="trash"]')).toBeNull();
    expect(self.querySelector('sl-icon-button[name="pencil"]')).not.toBeNull();
    for (const other of ['u-odin', 'u-dave']) {
      expect(row(el, other).querySelector('[data-self-leave]')).toBeNull();
      expect(row(el, other).querySelector('sl-icon-button[name="trash"]')).not.toBeNull();
    }
  });

  it('without the leave capabilities (experiment off) the owner keeps Remove-self', async () => {
    const el = await mount([ALICE, ODIN, DAVE], OWNER_CAPS, { currentUserId: 'u-alice' });
    const self = row(el, 'u-alice');
    expect(self.querySelector('[data-self-leave]')).toBeNull();
    expect(self.querySelector('sl-icon-button[name="trash"]')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('scion-project-leave-dialog')).toBeNull();
  });

  it('a read-only member sees Leave on their own row only', async () => {
    const el = await mount(
      [ALICE, DAVE],
      { ...MEMBER_CAPS, canLeave: true },
      {
        currentUserId: 'u-dave',
        readOnly: true,
      }
    );
    expect(el.shadowRoot!.querySelector('th.actions-cell')).not.toBeNull();
    expect(row(el, 'u-dave').querySelector('[data-self-leave]')).not.toBeNull();
    expect(row(el, 'u-alice').querySelector('[data-self-leave]')).toBeNull();
    expect(el.shadowRoot!.querySelector('sl-icon-button[name="trash"]')).toBeNull();
    expect(el.shadowRoot!.querySelector('sl-icon-button[name="pencil"]')).toBeNull();
  });

  it('a read-only member without the leave capabilities has no actions column', async () => {
    const el = await mount([ALICE, DAVE], MEMBER_CAPS, { currentUserId: 'u-dave', readOnly: true });
    expect(el.shadowRoot!.querySelector('th.actions-cell')).toBeNull();
    expect(el.shadowRoot!.querySelector('[data-self-leave]')).toBeNull();
  });

  it('the last owner sees Leave disabled with the last-owner guidance', async () => {
    const el = await mount(
      [ALICE, DAVE],
      { ...OWNER_CAPS, canLeave: false, leaveBlockedReason: 'last_owner' },
      { currentUserId: 'u-alice' }
    );
    const self = row(el, 'u-alice');
    const leave = self.querySelector('[data-self-leave]')!;
    expect(leave.hasAttribute('disabled')).toBe(true);
    expect(self.querySelector('sl-tooltip')?.getAttribute('content')).toBe(
      LEAVE_BLOCKED_LAST_OWNER
    );
  });

  it('canLeave false without a reason hides Leave', async () => {
    const el = await mount(
      [ALICE, DAVE],
      { ...MEMBER_CAPS, canLeave: false },
      {
        currentUserId: 'u-dave',
        readOnly: true,
      }
    );
    expect(el.shadowRoot!.querySelector('[data-self-leave]')).toBeNull();
  });

  it('Leave opens the shared dialog, which runs the dry run', async () => {
    const el = await mount(
      [ALICE, ODIN, DAVE],
      { ...OWNER_CAPS, canLeave: true },
      {
        currentUserId: 'u-alice',
      }
    );
    const dialog = el.shadowRoot!.querySelector('scion-project-leave-dialog')!;
    const show = vi.spyOn(dialog, 'show').mockResolvedValue();
    row(el, 'u-alice').querySelector<HTMLElement>('[data-self-leave]')!.click();
    expect(show).toHaveBeenCalledTimes(1);
    expect(dialog.getAttribute('projectname')).toBe('Project One');
  });

  it("the dialog's Transfer ownership link opens the editor's transfer dialog", async () => {
    const el = await mount(
      [ALICE, DAVE],
      { ...OWNER_CAPS, canLeave: false, leaveBlockedReason: 'last_owner' },
      {
        currentUserId: 'u-alice',
      }
    );
    const dialog = el.shadowRoot!.querySelector('scion-project-leave-dialog')!;
    dialog.dispatchEvent(
      new CustomEvent(LEAVE_TRANSFER_OWNERSHIP_EVENT, { bubbles: true, composed: true })
    );
    expect((el as unknown as { transferDialogOpen: boolean }).transferDialogOpen).toBe(true);
  });

  it('openTransferOnLoad opens the transfer dialog once loaded, for a user who may transfer', async () => {
    const el = await mount([ALICE, DAVE], OWNER_CAPS, {
      currentUserId: 'u-alice',
      openTransferOnLoad: true,
    });
    expect((el as unknown as { transferDialogOpen: boolean }).transferDialogOpen).toBe(true);
  });

  it('openTransferOnLoad does nothing without transfer authority', async () => {
    const el = await mount([ALICE, DAVE], MEMBER_CAPS, {
      currentUserId: 'u-dave',
      readOnly: true,
      openTransferOnLoad: true,
    });
    expect((el as unknown as { transferDialogOpen: boolean }).transferDialogOpen).toBe(false);
  });
});
