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
 * Project Members Editor — multi-role membership (ptone/scion#2529)
 *
 * One row per principal; the member dialog (Add and Edit modes) saves the
 * full role set with exactly one PUT …/members/principals/{type}/{id}, and
 * removal is one DELETE of the same path. The harness (mocked apiFetch and
 * showConfirm, private state driven through a cast) is ported from
 * miller79/scion PR #127.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import { apiFetch } from '../../client/api.js';
import type {
  AssignableProjectRole,
  MembershipCapabilities,
  ProjectMemberBinding,
  ProjectMemberGroup,
} from '../../shared/types.js';
import { showConfirm } from './confirm-dialog.js';
import {
  ScionProjectMembersEditor,
  AUTHORITY_CHANGED_MESSAGE,
  AUTHORITY_REDUCED_MESSAGE,
  CATALOG_UNAVAILABLE_REASON,
  CATALOG_REFRESHED_MESSAGE,
  CHANGED_WHILE_EDITING_LOCKED_MESSAGE,
  CHANGED_WHILE_EDITING_MESSAGE,
  CUSTOM_TIER_CAPTION,
  EMPTY_SELECTION_MESSAGE,
  LAST_OWNER_REASON,
  NO_PROJECT_ROLE,
  OWNER_ONLY_REMOVE_REASON,
  REMOVES_ALL_ROLES,
  ROW_LOCKED_REASON,
  TIER_REASON,
  builtInOptionState,
  canEditRow,
  addModeRoleReason,
  canRemoveRow,
  customRoleState,
  defaultBuiltInForAdd,
  deriveDialogLock,
  describeCustomRoleError,
  hasNonDirectSource,
  isCustomProjectRole,
  isEmptySelection,
  isLastOwnerByTier,
  roleIdsUnclassifiable,
  selectionToRoleIds,
  tierFromRoleIds,
  type MemberDialogMode,
} from './project-members-editor.js';

vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));

vi.mock('./confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const OWNER_CAPS: MembershipCapabilities = {
  canManageMembers: true,
  canManageAdmins: true,
  canManageOwners: true,
  canTransfer: true,
  canManageCustomRoles: true,
  actions: [],
};

const ADMIN_CAPS: MembershipCapabilities = {
  canManageMembers: true,
  canManageAdmins: false,
  canManageOwners: false,
  canTransfer: false,
  canManageCustomRoles: false,
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

/** A hub-override actor with no project role: only custom-role authority. */
const HUB_OVERRIDE_CAPS: MembershipCapabilities = {
  ...MEMBER_CAPS,
  canManageCustomRoles: true,
};

const CEILING_REASON = 'actor lacks permission for delegation: agent.delete';

function role(
  id: string,
  name: string,
  roleKind: 'builtin' | 'custom',
  extra: Partial<AssignableProjectRole> = {}
): AssignableProjectRole {
  return { id, name, description: '', roleKind, grantable: true, reason: '', ...extra };
}

const R_OWNER = role('r-owner', 'project-owner', 'builtin');
const R_ADMIN = role('r-admin', 'project-admin', 'builtin');
const R_MEMBER = role('r-member', 'project-member', 'builtin');
const R_MSG = role('r-msg', 'project-messaging', 'custom');
/** A custom role an owner may grant that nobody in ALL_GROUPS holds. */
const R_OPS = role('r-ops', 'project-ops', 'custom');
const R_CEIL = role('r-ceil', 'project-agent-reaper', 'custom', {
  grantable: false,
  reason: CEILING_REASON,
  denialCode: 'target_role_protected',
});

const OWNER_CATALOG = [R_OWNER, R_ADMIN, R_MEMBER, R_CEIL, R_MSG];

/** What assignable-roles returns for a project admin. */
const ADMIN_CATALOG = [
  { ...R_OWNER, grantable: false, reason: 'requires project owner' },
  { ...R_ADMIN, grantable: false, reason: 'requires project owner' },
  R_MEMBER,
  { ...R_CEIL, grantable: false, reason: 'custom role changes require role-binding authority' },
  { ...R_MSG, grantable: false, reason: 'custom role changes require role-binding authority' },
];

function binding(
  principalType: string,
  principalId: string,
  r: AssignableProjectRole,
  displayName = principalId
): ProjectMemberBinding {
  return {
    id: `b-${principalId}-${r.id}`,
    roleDefinitionId: r.id,
    roleName: r.name,
    principalType,
    principalId,
    principalDisplayName: displayName,
    scopeType: 'project',
    scopeId: 'p-1',
    createdAt: '2026-10-01T00:00:00Z',
    source: 'direct',
    roleKind: r.roleKind,
  };
}

function group(
  principalType: string,
  principalId: string,
  roles: AssignableProjectRole[],
  displayName = principalId
): ProjectMemberGroup {
  const builtIn = roles.find((r) => r.roleKind === 'builtin');
  return {
    principalType,
    principalId,
    principalDisplayName: displayName,
    builtInRoleName: builtIn?.name ?? '',
    bindings: roles.map((r) => binding(principalType, principalId, r, displayName)),
  };
}

const ALICE = group('user', 'u-alice', [R_OWNER], 'Alice Owner');
const BOB = group('user', 'u-bob', [R_ADMIN, R_MSG], 'Bob Admin');
const CAROL = group('user', 'u-carol', [R_MSG], 'Carol Custom');
const DAVE = group('user', 'u-dave', [R_MEMBER], 'Dave Member');
const ERIN = group('user', 'u-erin', [R_MEMBER, R_MSG], 'Erin Member');
const BOT = group('agent', 'a-bot', [R_MEMBER, R_MSG], 'bot');
const ENG = group('group', 'g-eng', [R_MEMBER], 'Engineering');

const ALL_GROUPS = [ALICE, BOB, CAROL, DAVE, ERIN, BOT, ENG];

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

/** Private surface the tests drive directly. */
interface EditorInternals {
  capabilities: MembershipCapabilities | null;
  groups: ProjectMemberGroup[];
  assignableRoles: AssignableProjectRole[];
  loading: boolean;
  dialogOpen: boolean;
  dialogMode: MemberDialogMode;
  dlgPrincipalType: string;
  dlgPrincipalId: string;
  dlgDisplayName: string;
  dlgBuiltIn: string;
  dlgCustomIds: string[];
  dlgExpectedIds: string[];
  dlgError: string | null;
  dlgErrorRoleId: string | null;
  dlgInfo: string | null;
  dlgIsLastOwner: boolean;
  dlgLockedReason: string | null;
  actionFeedback: { message: string; variant: string } | null;
  transferDialogOpen: boolean;
  transferNewOwnerId: string;
  transferError: string | null;
  loadData(): Promise<void>;
  openAddDialog(): void;
  openEditDialog(g: ProjectMemberGroup, info?: string | null): void;
  onPrincipalTypeChange(t: string): void;
  onPrincipalChange(d: { principalType: string; principalId: string; displayLabel: string }): void;
  toggleCustomRole(id: string, checked: boolean): void;
  handleSave(): Promise<void>;
  handleRemoveRow(g: ProjectMemberGroup): Promise<void>;
  handleRemoveFromDialog(): Promise<void>;
  openTransferDialog(): void;
  handleTransferOwnership(): Promise<void>;
  shadowRoot: ShadowRoot | null;
  updateComplete: Promise<boolean>;
}

/** Unmounted editor with state set directly; reloads are stubbed out. */
function makeEditor(
  caps: MembershipCapabilities | null,
  opts: { groups?: ProjectMemberGroup[]; catalog?: AssignableProjectRole[] } = {}
): EditorInternals {
  const el = new ScionProjectMembersEditor();
  el.projectId = 'p-1';
  const i = el as unknown as EditorInternals;
  i.capabilities = caps;
  i.groups = opts.groups ?? ALL_GROUPS;
  i.assignableRoles = opts.catalog ?? OWNER_CATALOG;
  i.loading = false;
  // Stop the post-success reload from issuing further requests.
  i.loadData = async () => {};
  return i;
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function apiError(status: number, code: string, message: string, details?: unknown): Response {
  return jsonResponse(status, { error: { code, message, details } });
}

type Route = (url: string, init?: RequestInit) => Response | undefined;

/** Routes apiFetch by URL and method; unmatched requests fail the test. */
function routeApi(...routes: Route[]): void {
  vi.mocked(apiFetch).mockImplementation(async (url: string, init?: RequestInit) => {
    for (const r of routes) {
      const res = r(url, init);
      if (res) return res;
    }
    throw new Error(`unexpected request ${init?.method ?? 'GET'} ${url}`);
  });
}

function listRoute(groups: ProjectMemberGroup[], caps: MembershipCapabilities | null): Route {
  return (url, init) => {
    if ((init?.method ?? 'GET') !== 'GET' || !url.includes('/members?groupBy=principal')) {
      return undefined;
    }
    return jsonResponse(200, {
      items: groups,
      totalCount: groups.length,
      ...(caps ? { _capabilities: caps } : {}),
    });
  };
}

function catalogRoute(catalog: AssignableProjectRole[]): Route {
  return (url) =>
    url.endsWith('/members/assignable-roles') ? jsonResponse(200, { items: catalog }) : undefined;
}

function calls(): Array<{ method: string; url: string; body: unknown }> {
  return vi.mocked(apiFetch).mock.calls.map(([url, init]) => {
    const i = init as RequestInit | undefined;
    return {
      method: i?.method ?? 'GET',
      url: url as string,
      body: typeof i?.body === 'string' ? JSON.parse(i.body) : undefined,
    };
  });
}

function writes() {
  return calls().filter((c) => c.method !== 'GET');
}

const mounted: HTMLElement[] = [];

/** Mounted editor that loads through the mocked API. */
async function mountEditor(
  groups: ProjectMemberGroup[],
  caps: MembershipCapabilities | null,
  catalog: AssignableProjectRole[] = OWNER_CATALOG
): Promise<EditorInternals> {
  routeApi(listRoute(groups, caps), catalogRoute(catalog));
  const el = new ScionProjectMembersEditor();
  el.projectId = 'p-1';
  document.body.appendChild(el);
  mounted.push(el);
  const i = el as unknown as EditorInternals;
  await vi.waitFor(() => expect(i.loading).toBe(false));
  await i.updateComplete;
  return i;
}

function q<T extends Element = HTMLElement>(el: EditorInternals, sel: string): T | null {
  return el.shadowRoot!.querySelector<T>(sel);
}

function qa(el: EditorInternals, sel: string): HTMLElement[] {
  return Array.from(el.shadowRoot!.querySelectorAll<HTMLElement>(sel));
}

function radio(el: EditorInternals, value: string): HTMLElement {
  const r = q(el, `sl-radio[value="${value}"]`);
  if (!r) throw new Error(`no radio ${value}`);
  return r;
}

function checkbox(el: EditorInternals, id: string): HTMLElement | null {
  return q(el, `sl-checkbox[data-role-id="${id}"]`);
}

function row(el: EditorInternals, key: string): HTMLElement {
  const r = q(el, `tr[data-principal="${key}"]`);
  if (!r) throw new Error(`no row ${key}`);
  return r;
}

beforeEach(() => {
  vi.mocked(apiFetch).mockReset();
  vi.mocked(showConfirm).mockClear();
  vi.mocked(showConfirm).mockImplementation(() => Promise.resolve(true));
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  for (const el of mounted.splice(0)) el.remove();
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

describe('pure helpers', () => {
  it('treats only non-built-in names as custom', () => {
    expect(isCustomProjectRole('project-member')).toBe(false);
    expect(isCustomProjectRole('project-admin')).toBe(false);
    expect(isCustomProjectRole('project-owner')).toBe(false);
    expect(isCustomProjectRole('project-messaging')).toBe(true);
  });

  it('builds the full role set and detects the empty selection', () => {
    expect(selectionToRoleIds('r-member', ['r-msg'])).toEqual(['r-member', 'r-msg']);
    expect(selectionToRoleIds(NO_PROJECT_ROLE, ['r-msg', 'r-msg'])).toEqual(['r-msg']);
    expect(isEmptySelection(NO_PROJECT_ROLE, [])).toBe(true);
    expect(isEmptySelection(NO_PROJECT_ROLE, ['r-msg'])).toBe(false);
  });

  it('maps a delegation-ceiling refusal and passes other errors through', () => {
    expect(describeCustomRoleError(CEILING_REASON)).toContain('"agent.delete"');
    expect(describeCustomRoleError(CEILING_REASON)).toContain("which you don't hold yourself");
    expect(describeCustomRoleError('role binding already exists')).toBe(
      'role binding already exists'
    );
  });
});

// ---------------------------------------------------------------------------
// Grouping / render
// ---------------------------------------------------------------------------

describe('grouped list render', () => {
  it('pages the grouped GET and renders one row per principal with all badges', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    const gets = calls().map((c) => c.url);
    expect(gets[0]).toBe('/api/v1/projects/p-1/members?groupBy=principal&limit=500&offset=0');
    expect(gets[1]).toBe('/api/v1/projects/p-1/members/assignable-roles');

    expect(qa(el, 'tbody tr')).toHaveLength(ALL_GROUPS.length);
    const bob = row(el, 'user:u-bob');
    expect(Array.from(bob.querySelectorAll('.role-badge')).map((b) => b.textContent)).toEqual([
      'project-admin',
      'project-messaging',
    ]);
    expect(bob.querySelector('.role-badge.custom')?.textContent).toBe('project-messaging');
  });

  it('shows "No project role" for a custom-only row', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    const carol = row(el, 'user:u-carol');
    expect(carol.querySelector('.no-role')?.textContent).toBe('No project role');
    expect(carol.querySelector('.role-badge.custom')?.textContent).toBe('project-messaging');
    expect(row(el, 'user:u-dave').querySelector('.no-role')).toBeNull();
  });

  it('follows further pages until totalCount principals are loaded', async () => {
    const pages: Record<string, ProjectMemberGroup[]> = { '0': [ALICE, BOB], '2': [DAVE] };
    routeApi((url) => {
      const m = /groupBy=principal&limit=500&offset=(\d+)$/.exec(url);
      if (!m) return undefined;
      return jsonResponse(200, {
        items: pages[m[1]] ?? [],
        totalCount: 3,
        _capabilities: OWNER_CAPS,
      });
    }, catalogRoute(OWNER_CATALOG));
    const el = makeEditor(null);
    Reflect.deleteProperty(el, 'loadData'); // use the real loader
    await el.loadData();
    expect(el.groups.map((g) => g.principalId)).toEqual(['u-alice', 'u-bob', 'u-dave']);
    expect(calls().filter((c) => c.url.includes('groupBy'))).toHaveLength(2);
  });

  it('is read-only and skips assignable-roles for a member actor', async () => {
    const el = await mountEditor(ALL_GROUPS, MEMBER_CAPS);
    expect(calls().map((c) => c.url)).toEqual([
      '/api/v1/projects/p-1/members?groupBy=principal&limit=500&offset=0',
    ]);
    expect(q(el, '.actions-cell')).toBeNull();
  });

  it('stays read-only for a hub-override actor holding only canManageCustomRoles', async () => {
    const el = await mountEditor(ALL_GROUPS, HUB_OVERRIDE_CAPS);
    expect(calls()).toHaveLength(1);
    expect(q(el, '.actions-cell')).toBeNull();
  });

  it('fails closed when _capabilities is absent', async () => {
    const el = await mountEditor(ALL_GROUPS, null);
    expect(el.capabilities).toBeNull();
    expect(q(el, '.actions-cell')).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Dialog validation and defaults
// ---------------------------------------------------------------------------

describe('dialog defaults and validation', () => {
  it('defaults to Admin for an owner adding a user', () => {
    expect(defaultBuiltInForAdd(OWNER_CAPS, 'user', OWNER_CATALOG)).toBe('r-admin');
    const el = makeEditor(OWNER_CAPS);
    el.openAddDialog();
    expect(el.dialogMode).toBe('add');
    expect(el.dlgBuiltIn).toBe('r-admin');
    expect(el.dlgCustomIds).toEqual([]);
  });

  it('defaults to Member for an admin actor and for an agent principal', () => {
    expect(defaultBuiltInForAdd(ADMIN_CAPS, 'user', ADMIN_CATALOG)).toBe('r-member');
    expect(defaultBuiltInForAdd(OWNER_CAPS, 'agent', OWNER_CATALOG)).toBe('r-member');
    const el = makeEditor(OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalTypeChange('agent');
    expect(el.dlgBuiltIn).toBe('r-member');
  });

  it('offers Owner, Admin, Member and None as radios (at most one built-in)', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openAddDialog();
    await el.updateComplete;
    const values = qa(el, 'sl-radio-group sl-radio').map((r) => r.getAttribute('value'));
    expect(values).toEqual(['r-owner', 'r-admin', 'r-member', NO_PROJECT_ROLE]);
    expect(radio(el, NO_PROJECT_ROLE).hasAttribute('disabled')).toBe(false);
  });

  it('disables Save and shows the message for an empty selection (Add mode: no Remove)', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openAddDialog();
    el.dlgPrincipalId = 'u-new';
    el.dlgBuiltIn = NO_PROJECT_ROLE;
    await el.updateComplete;
    expect(q(el, '.validation-warning')?.textContent).toContain(EMPTY_SELECTION_MESSAGE);
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);
    expect(q(el, 'sl-button.remove-member')).toBeNull();

    await el.handleSave();
    expect(apiFetch).toHaveBeenCalledTimes(2); // only the initial loads
  });

  it('disables Save in Add mode until a principal is chosen', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openAddDialog();
    await el.updateComplete;
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);
    el.dlgPrincipalId = 'u-new';
    await el.updateComplete;
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(false);
  });

  it('disables Save in Edit mode until something changed', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openEditDialog(DAVE);
    await el.updateComplete;
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);
    el.toggleCustomRole('r-msg', true);
    await el.updateComplete;
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(false);
  });

  it('Edit mode with an empty selection offers Remove member → DELETE-all after confirm', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openEditDialog(CAROL);
    el.toggleCustomRole('r-msg', false);
    await el.updateComplete;
    expect(q(el, '.validation-warning')?.textContent).toContain(EMPTY_SELECTION_MESSAGE);
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);
    expect(q(el, 'sl-button.remove-member')).not.toBeNull();

    vi.mocked(apiFetch).mockReset();
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse(204, null));
    el.loadData = async () => {};
    await el.handleRemoveFromDialog();

    expect(showConfirm).toHaveBeenCalledTimes(1);
    expect(vi.mocked(showConfirm).mock.calls[0][0]).toContain(REMOVES_ALL_ROLES);
    expect(calls()).toEqual([
      {
        method: 'DELETE',
        url: '/api/v1/projects/p-1/members/principals/user/u-carol',
        body: undefined,
      },
    ]);
    expect(el.dialogOpen).toBe(false);
  });

  it('does not DELETE when the Remove member confirm is declined', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openEditDialog(CAROL);
    el.toggleCustomRole('r-msg', false);
    vi.mocked(showConfirm).mockResolvedValueOnce(false);
    await el.handleRemoveFromDialog();
    expect(apiFetch).not.toHaveBeenCalled();
    expect(el.dialogOpen).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Eligibility
// ---------------------------------------------------------------------------

describe('eligibility', () => {
  const ctx = (
    principalType: string,
    extra: Partial<Parameters<typeof builtInOptionState>[1]> = {}
  ) => ({
    mode: 'add' as MemberDialogMode,
    caps: OWNER_CAPS,
    principalType,
    isLastOwner: false,
    currentBuiltInId: NO_PROJECT_ROLE,
    ...extra,
  });

  it('allows Owner for users only', () => {
    expect(builtInOptionState(R_OWNER, ctx('user')).disabled).toBe(false);
    expect(builtInOptionState(R_OWNER, ctx('group'))).toEqual({
      disabled: true,
      reason: 'Owner is only available for users',
    });
    expect(builtInOptionState(R_OWNER, ctx('agent')).disabled).toBe(true);
  });

  it('does not offer Admin for agents', () => {
    expect(builtInOptionState(R_ADMIN, ctx('group')).disabled).toBe(false);
    expect(builtInOptionState(R_ADMIN, ctx('agent'))).toEqual({
      disabled: true,
      reason: 'Admin is not available for agents',
    });
  });

  it('renders ineligible radios disabled with their reason for a group', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalTypeChange('group');
    await el.updateComplete;
    expect(radio(el, 'r-owner').hasAttribute('disabled')).toBe(true);
    expect(radio(el, 'r-owner').textContent).toContain('Owner is only available for users');
    expect(radio(el, 'r-admin').hasAttribute('disabled')).toBe(false);
    expect(q(el, '.validation-warning')?.textContent).toContain('Owner role is not available');
  });

  it('last direct owner: every non-owner option, None included, is disabled', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openEditDialog(ALICE);
    await el.updateComplete;
    expect(radio(el, 'r-owner').hasAttribute('disabled')).toBe(false);
    for (const v of ['r-admin', 'r-member', NO_PROJECT_ROLE]) {
      expect(radio(el, v).hasAttribute('disabled')).toBe(true);
      expect(radio(el, v).textContent).toContain(LAST_OWNER_REASON);
    }
  });

  it('a second owner lifts the last-owner lock', () => {
    const second = group('user', 'u-zed', [R_OWNER]);
    const el = makeEditor(OWNER_CAPS, { groups: [...ALL_GROUPS, second] });
    el.openEditDialog(ALICE);
    expect((el as unknown as { dlgIsLastOwner: boolean }).dlgIsLastOwner).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// assignable-roles is principal-agnostic and evaluated as an add
// ---------------------------------------------------------------------------

describe('assignable-roles is principal-agnostic', () => {
  it('Edit mode does not disable a built-in radio because grantable=false', async () => {
    const catalog = [
      R_OWNER,
      { ...R_ADMIN, grantable: false, reason: 'whatever' },
      R_MEMBER,
      R_MSG,
    ];
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS, catalog);
    el.openEditDialog(DAVE);
    await el.updateComplete;
    expect(radio(el, 'r-admin').hasAttribute('disabled')).toBe(false);

    // Add mode still honours grantable=false.
    el.openAddDialog();
    await el.updateComplete;
    expect(radio(el, 'r-admin').hasAttribute('disabled')).toBe(true);
    expect(radio(el, 'r-admin').textContent).toContain('whatever');
  });

  it('Edit mode lets the PUT decide and shows its error', async () => {
    const el = makeEditor(OWNER_CAPS, {
      catalog: [R_OWNER, { ...R_ADMIN, grantable: false, reason: 'x' }, R_MEMBER],
    });
    el.openEditDialog(DAVE);
    el.dlgBuiltIn = 'r-admin';
    vi.mocked(apiFetch).mockResolvedValueOnce(
      apiError(403, 'role_assignment_forbidden', 'not allowed to assign project-admin', {
        roleDefinitionId: 'r-admin',
      })
    );
    await el.handleSave();
    expect(el.dialogOpen).toBe(true);
    expect(el.dlgError).toBe('not allowed to assign project-admin');
    expect(el.dlgErrorRoleId).toBe('r-admin');
  });

  it('hides the custom-role section for an agent principal (Add mode)', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openAddDialog();
    await el.updateComplete;
    expect(q(el, '.custom-roles')).not.toBeNull();
    el.onPrincipalTypeChange('agent');
    await el.updateComplete;
    expect(q(el, '.custom-roles')).toBeNull();
    expect(qa(el, 'sl-checkbox')).toHaveLength(0);
  });

  it("shows an agent's held custom role read-only and keeps it in the PUT", async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openEditDialog(BOT);
    await el.updateComplete;
    expect(qa(el, 'sl-checkbox')).toHaveLength(0);
    expect(q(el, '.custom-roles .role-badge.custom')?.textContent).toBe('project-messaging');

    el.dlgBuiltIn = NO_PROJECT_ROLE;
    vi.mocked(apiFetch).mockReset();
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(200, { ...BOT, changed: true }));
    el.loadData = async () => {};
    await el.handleSave();
    expect(writes()).toEqual([
      {
        method: 'PUT',
        url: '/api/v1/projects/p-1/members/principals/agent/a-bot',
        body: { roleDefinitionIds: ['r-msg'], expectedRoleDefinitionIds: ['r-member', 'r-msg'] },
      },
    ]);
  });
});

// ---------------------------------------------------------------------------
// Tiers (admins manage members only)
// ---------------------------------------------------------------------------

describe('tiers', () => {
  it('custom checkboxes need canManageCustomRoles', () => {
    expect(customRoleState(R_MSG, { caps: OWNER_CAPS, held: false }).disabled).toBe(false);
    expect(customRoleState(R_MSG, { caps: ADMIN_CAPS, held: false }).disabled).toBe(true);
    expect(customRoleState(R_MSG, { caps: ADMIN_CAPS, held: true }).disabled).toBe(true);
  });

  it('disables a beyond-ceiling custom role with its reason, unless already held', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openAddDialog();
    await el.updateComplete;
    const ceil = checkbox(el, 'r-ceil')!;
    expect(ceil.hasAttribute('disabled')).toBe(true);
    expect(ceil.textContent).toContain(describeCustomRoleError(CEILING_REASON));
    expect(checkbox(el, 'r-msg')!.hasAttribute('disabled')).toBe(false);

    // Held-but-ungrantable stays uncheckable for owners.
    expect(customRoleState(R_CEIL, { caps: OWNER_CAPS, held: true }).disabled).toBe(false);
  });

  it('admins see custom roles read-only with the owner caption', async () => {
    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, ADMIN_CATALOG);
    el.openEditDialog(ERIN);
    await el.updateComplete;
    expect(q(el, '.custom-roles .form-help')?.textContent).toContain(CUSTOM_TIER_CAPTION);
    const msg = checkbox(el, 'r-msg')!;
    expect(msg.hasAttribute('checked')).toBe(true);
    expect(msg.hasAttribute('disabled')).toBe(true);
    expect(checkbox(el, 'r-ceil')!.hasAttribute('checked')).toBe(false);
  });

  it('admin built-in radios: Member and None enabled, Admin and Owner visible but disabled', async () => {
    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, ADMIN_CATALOG);
    el.openEditDialog(DAVE);
    await el.updateComplete;
    expect(radio(el, 'r-member').hasAttribute('disabled')).toBe(false);
    expect(radio(el, NO_PROJECT_ROLE).hasAttribute('disabled')).toBe(false);
    for (const v of ['r-owner', 'r-admin']) {
      expect(radio(el, v).hasAttribute('disabled')).toBe(true);
      expect(radio(el, v).textContent).toContain(TIER_REASON);
    }
  });

  it('admin keeps held custom roles when changing the built-in role', async () => {
    const el = makeEditor(ADMIN_CAPS, { catalog: ADMIN_CATALOG });
    el.openEditDialog(ERIN);
    el.dlgBuiltIn = NO_PROJECT_ROLE;
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(200, ERIN));
    await el.handleSave();
    expect(writes()[0].body).toEqual({
      roleDefinitionIds: ['r-msg'],
      expectedRoleDefinitionIds: ['r-member', 'r-msg'],
    });
  });

  it('admin row actions: edit and remove member-tier rows only', async () => {
    expect(canEditRow(DAVE, ADMIN_CAPS)).toBe(true);
    expect(canEditRow(CAROL, ADMIN_CAPS)).toBe(true); // built-in none
    expect(canEditRow(ERIN, ADMIN_CAPS)).toBe(true);
    expect(canEditRow(BOB, ADMIN_CAPS)).toBe(false);
    expect(canEditRow(ALICE, ADMIN_CAPS)).toBe(false);

    expect(canRemoveRow(DAVE, ADMIN_CAPS, ALL_GROUPS).disabled).toBe(false);
    expect(canRemoveRow(ENG, ADMIN_CAPS, ALL_GROUPS).disabled).toBe(false);
    expect(canRemoveRow(ERIN, ADMIN_CAPS, ALL_GROUPS)).toEqual({
      disabled: true,
      reason: OWNER_ONLY_REMOVE_REASON,
    });
    expect(canRemoveRow(CAROL, ADMIN_CAPS, ALL_GROUPS).disabled).toBe(true);
    expect(canRemoveRow(BOB, ADMIN_CAPS, ALL_GROUPS).disabled).toBe(true);

    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, ADMIN_CATALOG);
    expect(row(el, 'user:u-dave').querySelector('sl-icon-button[name="pencil"]')).not.toBeNull();
    expect(
      row(el, 'user:u-dave').querySelector('sl-icon-button[name="trash"]')?.hasAttribute('disabled')
    ).toBe(false);
    const erinTrash = row(el, 'user:u-erin').querySelector('sl-icon-button[name="trash"]');
    expect(erinTrash?.hasAttribute('disabled')).toBe(true);
    expect(erinTrash?.closest('sl-tooltip')?.getAttribute('content')).toBe(
      OWNER_ONLY_REMOVE_REASON
    );
    expect(row(el, 'user:u-bob').querySelector('sl-icon-button')).toBeNull();
    expect(row(el, 'user:u-alice').querySelector('sl-icon-button')).toBeNull();
  });

  it('owners can edit and remove every row except the last direct owner', () => {
    for (const g of ALL_GROUPS) expect(canEditRow(g, OWNER_CAPS)).toBe(true);
    expect(canRemoveRow(ALICE, OWNER_CAPS, ALL_GROUPS).disabled).toBe(true);
    for (const g of ALL_GROUPS.filter((g) => g !== ALICE)) {
      expect(canRemoveRow(g, OWNER_CAPS, ALL_GROUPS).disabled).toBe(false);
    }
  });
});

// ---------------------------------------------------------------------------
// Atomic save
// ---------------------------------------------------------------------------

describe('save', () => {
  it('Add mode issues exactly one PUT with the full set and expectedRoleDefinitionIds: []', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'u-new', displayLabel: 'New' });
    el.toggleCustomRole('r-msg', true);
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(201, {}));
    await el.handleSave();

    expect(calls()).toEqual([
      {
        method: 'PUT',
        url: '/api/v1/projects/p-1/members/principals/user/u-new',
        body: { roleDefinitionIds: ['r-admin', 'r-msg'], expectedRoleDefinitionIds: [] },
      },
    ]);
    expect(el.dialogOpen).toBe(false);
    expect(el.actionFeedback).toEqual({ message: 'Member added', variant: 'success' });
  });

  it('Edit mode issues exactly one PUT carrying the starting role set', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openEditDialog(BOB);
    el.dlgBuiltIn = 'r-member';
    el.toggleCustomRole('r-msg', false);
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(200, {}));
    await el.handleSave();
    expect(calls()).toEqual([
      {
        method: 'PUT',
        url: '/api/v1/projects/p-1/members/principals/user/u-bob',
        body: { roleDefinitionIds: ['r-member'], expectedRoleDefinitionIds: ['r-admin', 'r-msg'] },
      },
    ]);
    expect(el.actionFeedback?.message).toBe('Roles updated');
  });

  it('URL-encodes an email address typed into the picker', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'new@x.io', displayLabel: '' });
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(201, {}));
    await el.handleSave();
    expect(calls()[0].url).toBe('/api/v1/projects/p-1/members/principals/user/new%40x.io');
  });

  it('403 ceiling error keeps the dialog open, maps the message, makes no other request', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openEditDialog(DAVE);
    el.toggleCustomRole('r-ceil', true);
    const reload = vi.fn(async () => {});
    el.loadData = reload;
    vi.mocked(apiFetch).mockResolvedValueOnce(
      apiError(403, 'target_role_protected', `cannot assign role: ${CEILING_REASON}`, {
        roleDefinitionId: 'r-ceil',
        roleName: 'project-agent-reaper',
        reason: CEILING_REASON,
      })
    );
    await el.handleSave();

    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(reload).not.toHaveBeenCalled();
    expect(el.dialogOpen).toBe(true);
    expect(el.dlgError).toBe(describeCustomRoleError(CEILING_REASON));
    expect(el.dlgErrorRoleId).toBe('r-ceil');
  });
});

// ---------------------------------------------------------------------------
// Add mode for an existing member, and 409 membership_changed
// ---------------------------------------------------------------------------

describe('Add mode for an existing member switches to Edit', () => {
  it('picking an existing member switches to Edit, pre-filled, without saving', () => {
    const el = makeEditor(OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'U-ERIN', displayLabel: 'Erin' });
    expect(apiFetch).not.toHaveBeenCalled();
    expect(el.dialogMode).toBe('edit');
    expect(el.dlgPrincipalId).toBe('u-erin');
    expect(el.dlgBuiltIn).toBe('r-member');
    expect(el.dlgCustomIds).toEqual(['r-msg']);
    expect(el.dlgExpectedIds).toEqual(['r-member', 'r-msg']);
    expect(el.dlgInfo).toContain('already a member');
  });

  it('a late picker event in Edit mode does not change the Edit target', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openEditDialog(ERIN);
    el.onPrincipalChange({ principalType: 'user', principalId: 'erin@exam', displayLabel: '' });
    expect(el.dialogMode).toBe('edit');
    expect(el.dlgPrincipalId).toBe('u-erin');
    expect(el.dlgDisplayName).toBe('Erin Member');

    el.dlgBuiltIn = NO_PROJECT_ROLE;
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(200, {}));
    await el.handleSave();
    expect(writes()).toEqual([
      {
        method: 'PUT',
        url: '/api/v1/projects/p-1/members/principals/user/u-erin',
        body: { roleDefinitionIds: ['r-msg'], expectedRoleDefinitionIds: ['r-member', 'r-msg'] },
      },
    ]);
  });

  it('matches on principal type too', () => {
    const el = makeEditor(OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalTypeChange('group');
    el.onPrincipalChange({ principalType: 'group', principalId: 'u-erin', displayLabel: '' });
    expect(el.dialogMode).toBe('add');
  });
});

describe('409 membership_changed', () => {
  function changed(cause: string, current: string[]): Response {
    return apiError(409, 'membership_changed', 'membership changed', {
      cause,
      currentRoleDefinitionIds: current,
    });
  }

  it('principal_roles_changed in Add mode reloads, then switches to Edit pre-filled', async () => {
    const el = makeEditor(OWNER_CAPS, { groups: [ALICE] });
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'u-erin', displayLabel: 'Erin' });
    expect(el.dialogMode).toBe('add');
    el.loadData = vi.fn(async () => {
      el.groups = [ALICE, ERIN];
    });
    vi.mocked(apiFetch).mockResolvedValueOnce(
      changed('principal_roles_changed', ['r-member', 'r-msg'])
    );
    await el.handleSave();

    expect(el.loadData).toHaveBeenCalledTimes(1);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(el.dialogOpen).toBe(true);
    expect(el.dialogMode).toBe('edit');
    expect(el.dlgBuiltIn).toBe('r-member');
    expect(el.dlgCustomIds).toEqual(['r-msg']);
    expect(el.dlgExpectedIds).toEqual(['r-member', 'r-msg']);
    expect(el.dlgInfo).toContain('already a member');
  });

  it('principal_roles_changed for an email address lands in Edit from currentRoleDefinitionIds', async () => {
    const el = makeEditor(OWNER_CAPS, { groups: [ALICE] });
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'erin@x.io', displayLabel: '' });
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(changed('principal_roles_changed', ['r-member', 'r-msg']))
      .mockResolvedValueOnce(jsonResponse(200, {}));
    await el.handleSave();

    expect(el.dialogMode).toBe('edit');
    expect(el.dlgBuiltIn).toBe('r-member');
    expect(el.dlgCustomIds).toEqual(['r-msg']);
    expect(el.dlgExpectedIds).toEqual(['r-member', 'r-msg']);

    // The retry sends the server's current set as expected.
    el.toggleCustomRole('r-msg', false);
    await el.handleSave();
    expect(calls()[1].body).toEqual({
      roleDefinitionIds: ['r-member'],
      expectedRoleDefinitionIds: ['r-member', 'r-msg'],
    });
  });

  it('principal_roles_changed in Edit mode re-prefills with the changed message', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openEditDialog(DAVE);
    el.dlgBuiltIn = NO_PROJECT_ROLE;
    el.toggleCustomRole('r-msg', true);
    const daveNow = group('user', 'u-dave', [R_ADMIN], 'Dave Member');
    el.loadData = vi.fn(async () => {
      el.groups = ALL_GROUPS.map((g) => (g === DAVE ? daveNow : g));
    });
    vi.mocked(apiFetch).mockResolvedValueOnce(changed('principal_roles_changed', ['r-admin']));
    await el.handleSave();

    expect(el.dlgInfo).toBe(CHANGED_WHILE_EDITING_MESSAGE);
    expect(el.dlgBuiltIn).toBe('r-admin');
    expect(el.dlgCustomIds).toEqual([]);
    expect(el.dlgExpectedIds).toEqual(['r-admin']);
  });

  it('actor_authority_changed in Add mode drops choices the actor can no longer make, never "already a member"', async () => {
    const el = makeEditor(OWNER_CAPS, { groups: [ALICE] });
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'u-new', displayLabel: 'New' });
    expect(el.dlgBuiltIn).toBe('r-admin');
    el.toggleCustomRole('r-msg', true);
    el.loadData = vi.fn(async () => {
      el.capabilities = ADMIN_CAPS;
      el.assignableRoles = ADMIN_CATALOG;
    });
    vi.mocked(apiFetch).mockResolvedValueOnce(changed('actor_authority_changed', []));
    await el.handleSave();

    expect(el.loadData).toHaveBeenCalledTimes(1);
    expect(el.capabilities).toBe(ADMIN_CAPS);
    expect(el.dialogOpen).toBe(true);
    expect(el.dialogMode).toBe('add');
    // The custom role and Admin can no longer be granted by this actor.
    expect(el.dlgCustomIds).toEqual([]);
    expect(el.dlgBuiltIn).toBe('r-member');
    expect(el.dlgError).toBe(AUTHORITY_CHANGED_MESSAGE);
    expect(`${el.dlgError} ${el.dlgInfo ?? ''}`).not.toContain('already a member');
  });

  it('actor_authority_changed in Edit mode keeps held roles and drops new ungrantable ones', async () => {
    const el = makeEditor(OWNER_CAPS, { catalog: [...OWNER_CATALOG, R_OPS] });
    el.openEditDialog(ERIN);
    el.dlgBuiltIn = 'r-admin';
    el.toggleCustomRole('r-ops', true);
    el.loadData = vi.fn(async () => {
      el.capabilities = ADMIN_CAPS;
      el.assignableRoles = ADMIN_CATALOG;
    });
    vi.mocked(apiFetch).mockResolvedValueOnce(changed('actor_authority_changed', []));
    await el.handleSave();

    expect(el.dialogOpen).toBe(true);
    expect(el.dlgLockedReason).toBeNull();
    expect(el.dlgBuiltIn).toBe('r-member');
    expect(el.dlgCustomIds).toEqual(['r-msg']);
    expect(el.dlgError).toBe(AUTHORITY_CHANGED_MESSAGE);
  });

  it('owner demoted to admin while editing an admin gets the locked dialog', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS, [...OWNER_CATALOG, R_OPS]);
    el.openEditDialog(BOB);
    el.dlgBuiltIn = 'r-member';
    el.toggleCustomRole('r-ops', true);
    routeApi(
      (url, init) =>
        init?.method === 'PUT'
          ? changed('actor_authority_changed', ['r-admin', 'r-msg'])
          : undefined,
      listRoute(ALL_GROUPS, ADMIN_CAPS),
      catalogRoute([...ADMIN_CATALOG, { ...R_OPS, grantable: false, reason: 'requires owner' }])
    );
    await el.handleSave();
    await el.updateComplete;

    expect(el.capabilities).toEqual(ADMIN_CAPS);
    expect(el.dialogOpen).toBe(true);
    expect(el.dlgLockedReason).toBe(ROW_LOCKED_REASON);
    // Back to Bob's current roles; the new custom role is no longer selected.
    expect(el.dlgBuiltIn).toBe('r-admin');
    expect(el.dlgCustomIds).toEqual(['r-msg']);
    expect(el.dlgError).toBe(AUTHORITY_REDUCED_MESSAGE);
    expect(q(el, '.dialog-info .locked-reason')?.textContent).toBe(ROW_LOCKED_REASON);
    for (const r of qa(el, 'sl-radio-group sl-radio')) {
      expect(r.hasAttribute('disabled')).toBe(true);
    }
    for (const c of qa(el, 'sl-checkbox')) expect(c.hasAttribute('disabled')).toBe(true);
    expect(checkbox(el, 'r-ops')?.hasAttribute('checked')).toBe(false);
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);
  });

  it('actor_authority_changed with the catalog reload failing resets an Add-mode built-in choice', async () => {
    const el = await mountEditor([ALICE], OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'u-new', displayLabel: 'New' });
    expect(el.dlgBuiltIn).toBe('r-admin');
    routeApi(
      (url, init) => (init?.method === 'PUT' ? changed('actor_authority_changed', []) : undefined),
      listRoute([ALICE], ADMIN_CAPS),
      (url) =>
        url.endsWith('/members/assignable-roles')
          ? apiError(500, 'internal', 'catalog unavailable')
          : undefined
    );
    await el.handleSave();
    await el.updateComplete;

    expect(el.capabilities).toEqual(ADMIN_CAPS);
    expect(el.dialogOpen).toBe(true);
    expect(el.dialogMode).toBe('add');
    // Admin is no longer listed, so it can't stay selected without a radio.
    expect(el.dlgBuiltIn).toBe(NO_PROJECT_ROLE);
    expect(q(el, 'sl-radio[value="r-admin"]')).toBeNull();
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);
    expect(writes()).toHaveLength(1);
  });

  it('actor_authority_changed with the catalog reload failing keeps a row-based Edit dialog unlocked', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    el.openEditDialog(ERIN);
    el.dlgBuiltIn = 'r-admin';
    routeApi(
      (url, init) => (init?.method === 'PUT' ? changed('actor_authority_changed', []) : undefined),
      listRoute(ALL_GROUPS, ADMIN_CAPS),
      (url) =>
        url.endsWith('/members/assignable-roles')
          ? apiError(500, 'internal', 'catalog unavailable')
          : undefined
    );
    await el.handleSave();
    await el.updateComplete;

    expect(el.capabilities).toEqual(ADMIN_CAPS);
    expect(el.dialogOpen).toBe(true);
    // The loaded row decides the lock, not the now-unclassifiable role IDs.
    expect(el.dlgLockedReason).toBeNull();
    expect(q(el, '.dialog-info .locked-reason')).toBeNull();
    expect(el.dlgBuiltIn).toBe('r-member');
    expect(el.dlgCustomIds).toEqual(['r-msg']);
    expect(el.dlgError).toBe(AUTHORITY_CHANGED_MESSAGE);
    expect(radio(el, NO_PROJECT_ROLE).hasAttribute('disabled')).toBe(false);
  });

  it('actor_authority_changed that leaves the editor read-only closes the dialog', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openEditDialog(DAVE);
    el.dlgBuiltIn = 'r-admin';
    el.loadData = vi.fn(async () => {
      el.capabilities = MEMBER_CAPS;
      el.assignableRoles = [];
    });
    vi.mocked(apiFetch).mockResolvedValueOnce(changed('actor_authority_changed', []));
    await el.handleSave();

    expect(el.dialogOpen).toBe(false);
    expect(el.actionFeedback).toEqual({ message: AUTHORITY_REDUCED_MESSAGE, variant: 'danger' });
    expect(writes()).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// Automatic switch to Edit respects the row tier and last-owner rules
// ---------------------------------------------------------------------------

describe('automatic switch to Edit follows the row rules', () => {
  /** Every radio, checkbox and Save is disabled and the reason is shown;
   *  the info text only says the principal is already a member. */
  function expectLocked(el: EditorInternals, name: string): void {
    expect(el.dialogMode).toBe('edit');
    expect(el.dlgLockedReason).toBe(ROW_LOCKED_REASON);
    const radios = qa(el, 'sl-radio-group sl-radio');
    expect(radios.length).toBe(4);
    for (const r of radios) expect(r.hasAttribute('disabled')).toBe(true);
    for (const c of qa(el, 'sl-checkbox')) expect(c.hasAttribute('disabled')).toBe(true);
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);
    expect(el.dlgInfo).toBe(`${name} is already a member.`);
    const info = q(el, '.dialog-info')?.textContent ?? '';
    expect(info).toContain(`${name} is already a member.`);
    expect(info).not.toContain('Editing their roles');
    expect(info).not.toContain('save again');
    expect(q(el, '.dialog-info .locked-reason')?.textContent).toBe(ROW_LOCKED_REASON);
  }

  function changed409(current: string[]): Response {
    return apiError(409, 'membership_changed', 'membership changed', {
      cause: 'principal_roles_changed',
      currentRoleDefinitionIds: current,
    });
  }

  it('infers the tier from the built-in role in the role IDs, failing closed on unknown IDs', () => {
    expect(tierFromRoleIds(['r-admin', 'r-msg'], OWNER_CATALOG)).toBe('admin');
    expect(tierFromRoleIds(['r-owner'], OWNER_CATALOG)).toBe('owner');
    expect(tierFromRoleIds(['r-msg'], OWNER_CATALOG)).toBe('member');
    expect(tierFromRoleIds(['r-unknown'], OWNER_CATALOG)).toBe('owner');
    expect(tierFromRoleIds(['r-member'], [])).toBe('owner');
    expect(isLastOwnerByTier('user', 'owner', [ALICE, DAVE])).toBe(true);
    expect(isLastOwnerByTier('user', 'owner', [ALICE, group('user', 'u-zed', [R_OWNER])])).toBe(
      false
    );
    expect(isLastOwnerByTier('user', 'admin', [ALICE])).toBe(false);
  });

  it('locks without classifying when role IDs cannot be sorted into built-in and custom', () => {
    expect(roleIdsUnclassifiable(['r-member'], OWNER_CATALOG, true)).toBe(true);
    expect(roleIdsUnclassifiable([], OWNER_CATALOG, true)).toBe(true);
    expect(roleIdsUnclassifiable(['r-member'], [], false)).toBe(true);
    expect(roleIdsUnclassifiable(['r-gone', 'r-msg'], OWNER_CATALOG, false)).toBe(true);
    expect(roleIdsUnclassifiable(['r-member', 'r-gone'], OWNER_CATALOG, false)).toBe(false);
    expect(roleIdsUnclassifiable(['r-msg'], OWNER_CATALOG, false)).toBe(false);

    const base = {
      principalType: 'user',
      row: undefined,
      caps: OWNER_CAPS,
      groups: [ALICE, DAVE],
      assignable: OWNER_CATALOG,
      catalogUnavailable: false,
    };
    // An unknown ID and no built-in role: locked even for an owner, and no
    // last-owner status is inferred from the fail-closed owner tier.
    expect(deriveDialogLock({ ...base, roleIds: ['r-gone'] })).toEqual({
      lockedReason: CATALOG_UNAVAILABLE_REASON,
      isLastOwner: false,
    });
    expect(
      deriveDialogLock({ ...base, roleIds: ['r-member'], assignable: [], catalogUnavailable: true })
    ).toEqual({ lockedReason: CATALOG_UNAVAILABLE_REASON, isLastOwner: false });
    // A loaded row follows the row rules even without a catalog.
    expect(
      deriveDialogLock({
        ...base,
        row: DAVE,
        roleIds: [],
        assignable: [],
        catalogUnavailable: true,
      })
    ).toEqual({ lockedReason: null, isLastOwner: false });
    expect(deriveDialogLock({ ...base, roleIds: ['r-owner'] })).toEqual({
      lockedReason: null,
      isLastOwner: true,
    });
    expect(deriveDialogLock({ ...base, caps: ADMIN_CAPS, row: BOB, roleIds: [] })).toEqual({
      lockedReason: ROW_LOCKED_REASON,
      isLastOwner: false,
    });
  });

  it('owner reaching a member by email while the catalog fails gets a locked dialog, no raw IDs', async () => {
    const el = await mountEditor([ALICE, DAVE], OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'dave@x.io', displayLabel: '' });
    el.dlgBuiltIn = 'r-member';
    routeApi(
      (url, init) => (init?.method === 'PUT' ? changed409(['r-member', 'r-msg']) : undefined),
      listRoute([ALICE, DAVE], OWNER_CAPS),
      (url) =>
        url.endsWith('/members/assignable-roles')
          ? apiError(500, 'internal', 'catalog unavailable')
          : undefined
    );
    await el.handleSave();
    await el.updateComplete;

    expect(el.dialogMode).toBe('edit');
    expect(el.dlgLockedReason).toBe(CATALOG_UNAVAILABLE_REASON);
    expect(el.dlgIsLastOwner).toBe(false);
    expect(q(el, '.dialog-member sl-icon[name="shield-lock"]')).toBeNull();
    expect(el.dlgExpectedIds).toEqual(['r-member', 'r-msg']);
    expect(el.dlgInfo).toBe('dave@x.io is already a member.');
    expect(q(el, '.dialog-info .locked-reason')?.textContent).toBe(CATALOG_UNAVAILABLE_REASON);
    for (const c of qa(el, 'sl-checkbox')) {
      expect(['r-member', 'r-msg']).not.toContain(c.textContent?.trim());
    }
    expect(qa(el, 'sl-checkbox')).toEqual([]);
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);
    expect(q(el, 'sl-button.remove-member')).toBeNull();
  });

  it('a member promoted to admin mid-edit locks the dialog with the short changed message', async () => {
    const el = makeEditor(ADMIN_CAPS, { catalog: ADMIN_CATALOG });
    el.openEditDialog(DAVE);
    const daveNow = group('user', 'u-dave', [R_ADMIN], 'Dave Member');
    el.loadData = vi.fn(async () => {
      el.groups = ALL_GROUPS.map((g) => (g === DAVE ? daveNow : g));
    });
    vi.mocked(apiFetch).mockResolvedValueOnce(changed409(['r-admin']));
    await el.handleSave();
    expect(el.dlgLockedReason).toBe(ROW_LOCKED_REASON);
    expect(el.dlgInfo).toBe(CHANGED_WHILE_EDITING_LOCKED_MESSAGE);
  });

  it('admin in Add mode picking an existing admin gets a read-only dialog with the reason', async () => {
    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, ADMIN_CATALOG);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'u-bob', displayLabel: 'Bob' });
    await el.updateComplete;
    expectLocked(el, 'Bob Admin');

    // Even a forced selection change cannot be saved.
    el.dlgBuiltIn = 'r-member';
    vi.mocked(apiFetch).mockReset();
    await el.handleSave();
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('a locked dialog disables every control even with custom-role authority', async () => {
    // Forced state: today a locked actor never holds canManageCustomRoles,
    // so this checks the lock itself rather than the capability.
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS, [...OWNER_CATALOG, R_OPS]);
    el.openEditDialog(ERIN);
    el.dlgLockedReason = ROW_LOCKED_REASON;
    el.dlgBuiltIn = 'r-admin';
    await el.updateComplete;
    expect(qa(el, 'sl-checkbox').length).toBeGreaterThan(0);
    for (const c of qa(el, 'sl-checkbox')) expect(c.hasAttribute('disabled')).toBe(true);
    // A changed, non-empty selection: only the lock disables Save.
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);

    el.dlgBuiltIn = NO_PROJECT_ROLE;
    el.dlgCustomIds = [];
    await el.updateComplete;
    expect(q(el, 'sl-button.remove-member')?.hasAttribute('disabled')).toBe(true);
    expect(q(el, 'sl-button.save-member')?.hasAttribute('disabled')).toBe(true);

    vi.mocked(apiFetch).mockReset();
    await el.handleRemoveFromDialog();
    expect(showConfirm).not.toHaveBeenCalled();
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('admin in Add mode picking an existing member still gets an editable dialog', async () => {
    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, ADMIN_CATALOG);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'u-dave', displayLabel: 'Dave' });
    await el.updateComplete;
    expect(el.dlgLockedReason).toBeNull();
    expect(radio(el, 'r-member').hasAttribute('disabled')).toBe(false);
    expect(q(el, '.dialog-info .locked-reason')).toBeNull();
  });

  it('409 principal_roles_changed for an admin typed by email: read-only with the reason', async () => {
    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, ADMIN_CATALOG);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'bob@x.io', displayLabel: '' });
    el.dlgBuiltIn = 'r-member';
    routeApi(
      (url, init) => (init?.method === 'PUT' ? changed409(['r-admin', 'r-msg']) : undefined),
      listRoute(ALL_GROUPS, ADMIN_CAPS),
      catalogRoute(ADMIN_CATALOG)
    );
    await el.handleSave();
    await el.updateComplete;
    expect(el.dlgPrincipalId).toBe('bob@x.io');
    expectLocked(el, 'bob@x.io');
  });

  it('409 principal_roles_changed for a member typed by email stays editable for an admin', async () => {
    const el = makeEditor(ADMIN_CAPS, { catalog: ADMIN_CATALOG });
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'dave@x.io', displayLabel: '' });
    vi.mocked(apiFetch).mockResolvedValueOnce(changed409(['r-member']));
    await el.handleSave();
    expect(el.dialogMode).toBe('edit');
    expect(el.dlgLockedReason).toBeNull();
  });

  it('owner reaching the last owner by email gets the last-owner lock', async () => {
    const el = await mountEditor([ALICE, DAVE], OWNER_CAPS);
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'alice@x.io', displayLabel: '' });
    routeApi(
      (url, init) => (init?.method === 'PUT' ? changed409(['r-owner']) : undefined),
      listRoute([ALICE, DAVE], OWNER_CAPS),
      catalogRoute(OWNER_CATALOG)
    );
    await el.handleSave();
    await el.updateComplete;
    expect(el.dialogMode).toBe('edit');
    expect(el.dlgLockedReason).toBeNull();
    expect(el.dlgIsLastOwner).toBe(true);
    expect(radio(el, 'r-owner').hasAttribute('disabled')).toBe(false);
    for (const v of ['r-admin', 'r-member', NO_PROJECT_ROLE]) {
      expect(radio(el, v).hasAttribute('disabled')).toBe(true);
      expect(radio(el, v).textContent).toContain(LAST_OWNER_REASON);
    }
  });

  it('an owner typed by email is not locked when another direct owner exists', async () => {
    const zed = group('user', 'u-zed', [R_OWNER]);
    const el = makeEditor(OWNER_CAPS, { groups: [ALICE, zed] });
    el.openAddDialog();
    el.onPrincipalChange({ principalType: 'user', principalId: 'alice@x.io', displayLabel: '' });
    vi.mocked(apiFetch).mockResolvedValueOnce(changed409(['r-owner']));
    await el.handleSave();
    expect(el.dialogMode).toBe('edit');
    expect(el.dlgIsLastOwner).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// 400 invalid_role_set
// ---------------------------------------------------------------------------

describe('400 invalid_role_set', () => {
  it('refreshes assignable-roles, prunes the vanished role and shows the message', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.openEditDialog(DAVE);
    el.toggleCustomRole('r-msg', true);
    const gone = [R_OWNER, R_ADMIN, R_MEMBER, R_CEIL];
    vi.mocked(apiFetch)
      .mockResolvedValueOnce(
        apiError(400, 'invalid_role_set', 'unknown role definition: r-msg', {
          roleDefinitionId: 'r-msg',
        })
      )
      .mockResolvedValueOnce(jsonResponse(200, { items: gone }));
    await el.handleSave();

    expect(calls().map((c) => `${c.method} ${c.url}`)).toEqual([
      'PUT /api/v1/projects/p-1/members/principals/user/u-dave',
      'GET /api/v1/projects/p-1/members/assignable-roles',
    ]);
    expect(el.assignableRoles).toEqual(gone);
    expect(el.dlgCustomIds).toEqual([]);
    expect(el.dlgError).toBe('unknown role definition: r-msg');
    expect(el.dlgInfo).toBe(CATALOG_REFRESHED_MESSAGE);
    expect(el.dialogOpen).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// assignable-roles failure
// ---------------------------------------------------------------------------

describe('assignable-roles failure', () => {
  const FRANK = group('user', 'u-frank', [R_MEMBER, R_MSG, R_CEIL], 'Frank');

  function failingCatalogRoute(): Route {
    return (url) =>
      url.endsWith('/members/assignable-roles')
        ? apiError(500, 'internal', 'catalog unavailable')
        : undefined;
  }

  it('shows the error, keeps no catalog, and still shows and keeps held roles', async () => {
    routeApi(listRoute([ALICE, FRANK], OWNER_CAPS), failingCatalogRoute());
    const el = new ScionProjectMembersEditor();
    el.projectId = 'p-1';
    document.body.appendChild(el);
    mounted.push(el);
    const i = el as unknown as EditorInternals;
    await vi.waitFor(() => expect(i.loading).toBe(false));
    expect(i.assignableRoles).toEqual([]);

    i.openEditDialog(FRANK);
    await i.updateComplete;
    expect(q(i, '.dialog-error')?.textContent).toContain(
      "Couldn't load the list of roles: catalog unavailable"
    );
    // The held built-in role is still offered (and selected), with None.
    expect(qa(i, 'sl-radio-group sl-radio').map((r) => r.getAttribute('value'))).toEqual([
      'r-member',
      NO_PROJECT_ROLE,
    ]);
    expect(radio(i, 'r-member').textContent).toContain('Member');
    expect(q<HTMLElement & { value: string }>(i, 'sl-radio-group')?.value).toBe('r-member');
    // Held custom roles are still shown, checked.
    expect(checkbox(i, 'r-msg')?.hasAttribute('checked')).toBe(true);
    expect(checkbox(i, 'r-ceil')?.hasAttribute('checked')).toBe(true);

    // Dropping one custom role keeps the built-in and the other custom role.
    i.toggleCustomRole('r-ceil', false);
    vi.mocked(apiFetch).mockReset();
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(200, {}));
    i.loadData = async () => {};
    await i.handleSave();
    expect(writes()).toEqual([
      {
        method: 'PUT',
        url: '/api/v1/projects/p-1/members/principals/user/u-frank',
        body: {
          roleDefinitionIds: ['r-member', 'r-msg'],
          expectedRoleDefinitionIds: ['r-member', 'r-msg', 'r-ceil'],
        },
      },
    ]);
  });

  it('a failed refresh clears the previous catalog instead of keeping it stale', async () => {
    const el = makeEditor(OWNER_CAPS, { groups: [ALICE, FRANK] });
    el.openEditDialog(FRANK);
    el.toggleCustomRole('r-ceil', false);
    routeApi(
      (url, init) =>
        init?.method === 'PUT'
          ? apiError(400, 'invalid_role_set', 'unknown role definition: r-x', {
              roleDefinitionId: 'r-x',
            })
          : undefined,
      failingCatalogRoute()
    );
    await el.handleSave();
    expect(el.assignableRoles).toEqual([]);
    expect((el as unknown as { catalogError: string | null }).catalogError).toBe(
      'catalog unavailable'
    );
    // Held roles survive the cleared catalog.
    expect(el.dlgBuiltIn).toBe('r-member');
    expect(el.dlgCustomIds).toEqual(['r-msg']);
  });
});

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

describe('row delete', () => {
  it('calls DELETE …/principals/{type}/{id} once after confirm', async () => {
    const el = makeEditor(OWNER_CAPS);
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(204, null));
    await el.handleRemoveRow(BOB);
    expect(showConfirm).toHaveBeenCalledTimes(1);
    expect(showConfirm).toHaveBeenCalledWith(
      'Remove user "Bob Admin" from this project? This removes all of their project roles.'
    );
    expect(calls()).toEqual([
      {
        method: 'DELETE',
        url: '/api/v1/projects/p-1/members/principals/user/u-bob',
        body: undefined,
      },
    ]);
    expect(el.actionFeedback).toEqual({ message: 'Member removed', variant: 'success' });
  });

  it('group principals use …/principals/group/{encoded id} for Add, Edit and row delete', async () => {
    const team = group('group', 'g/ops team', [R_MEMBER], 'Ops');
    const el = makeEditor(OWNER_CAPS, { groups: [...ALL_GROUPS, team] });
    vi.mocked(apiFetch).mockImplementation(async () => jsonResponse(200, {}));

    el.openAddDialog();
    el.onPrincipalTypeChange('group');
    el.onPrincipalChange({ principalType: 'group', principalId: 'g/new team', displayLabel: '' });
    await el.handleSave();

    el.openEditDialog(team);
    el.dlgBuiltIn = 'r-admin';
    await el.handleSave();

    await el.handleRemoveRow(team);

    expect(writes().map((c) => `${c.method} ${c.url}`)).toEqual([
      'PUT /api/v1/projects/p-1/members/principals/group/g%2Fnew%20team',
      'PUT /api/v1/projects/p-1/members/principals/group/g%2Fops%20team',
      'DELETE /api/v1/projects/p-1/members/principals/group/g%2Fops%20team',
    ]);
    expect(writes()[0].body).toEqual({
      roleDefinitionIds: ['r-admin'],
      expectedRoleDefinitionIds: [],
    });
    expect(writes()[1].body).toEqual({
      roleDefinitionIds: ['r-admin'],
      expectedRoleDefinitionIds: ['r-member'],
    });
  });

  it('refuses the last direct owner without a request', async () => {
    const el = makeEditor(OWNER_CAPS);
    await el.handleRemoveRow(ALICE);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(el.actionFeedback?.variant).toBe('danger');
  });

  it('surfaces a server refusal', async () => {
    const el = makeEditor(OWNER_CAPS);
    vi.mocked(apiFetch).mockResolvedValueOnce(
      apiError(
        403,
        'role_assignment_forbidden',
        'custom role changes require role-binding authority'
      )
    );
    await el.handleRemoveRow(ERIN);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(el.actionFeedback).toEqual({
      message: 'custom role changes require role-binding authority',
      variant: 'danger',
    });
  });
});

// ---------------------------------------------------------------------------
// Transfer Ownership (unchanged; smoke test)
// ---------------------------------------------------------------------------

describe('Transfer Ownership', () => {
  it('shows the button for canTransfer and posts newOwnerId', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    expect(q(el, 'sl-button[variant="warning"]')?.textContent).toContain('Transfer Ownership');

    el.openTransferDialog();
    await el.updateComplete;
    expect(q(el, 'sl-dialog[label="Transfer Project Ownership"]')).not.toBeNull();

    vi.mocked(apiFetch).mockReset();
    vi.mocked(apiFetch).mockResolvedValueOnce(jsonResponse(200, {}));
    el.loadData = async () => {};
    el.transferNewOwnerId = ' u-dave ';
    await el.handleTransferOwnership();
    expect(calls()).toEqual([
      {
        method: 'POST',
        url: '/api/v1/projects/p-1/transfer-ownership',
        body: { newOwnerId: 'u-dave' },
      },
    ]);
    expect(el.transferDialogOpen).toBe(false);
  });

  it('is hidden without canTransfer', async () => {
    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, ADMIN_CATALOG);
    expect(q(el, 'sl-button[variant="warning"]')).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Source column (ptone/scion#2672 item 3)
// ---------------------------------------------------------------------------

describe('Source column', () => {
  const VIA_GROUP: ProjectMemberGroup = {
    ...DAVE,
    bindings: DAVE.bindings.map((b) => ({
      ...b,
      source: 'group:g-eng',
      sourceGroupName: 'Engineering',
    })),
  };

  it('hasNonDirectSource is false while every binding is direct', () => {
    expect(hasNonDirectSource(ALL_GROUPS)).toBe(false);
    expect(hasNonDirectSource([])).toBe(false);
    expect(hasNonDirectSource([VIA_GROUP])).toBe(true);
  });

  it('is hidden when every binding is direct', async () => {
    const el = await mountEditor(ALL_GROUPS, OWNER_CAPS);
    const headers = qa(el, 'thead th').map((th) => th.textContent?.trim());
    expect(headers).not.toContain('Source');
    expect(qa(el, '.provenance-badge')).toHaveLength(0);
  });

  it('is shown when at least one binding has a non-direct source', async () => {
    const groups = ALL_GROUPS.map((g) => (g === DAVE ? VIA_GROUP : g));
    const el = await mountEditor(groups, OWNER_CAPS);
    const headers = qa(el, 'thead th').map((th) => th.textContent?.trim());
    expect(headers).toContain('Source');
    // Every row gets a cell, so columns stay aligned.
    expect(qa(el, '.provenance-badge')).toHaveLength(groups.length);
    const badge = (key: string) =>
      row(el, key).querySelector('.provenance-badge')?.textContent?.replace(/\s+/g, ' ').trim();
    expect(badge('user:u-dave')).toBe('Via group: Engineering');
    expect(badge('user:u-alice')).toBe('Direct');
  });
});

// ---------------------------------------------------------------------------
// Add dialog per-role reasons (ptone/scion#2672 item 4)
// ---------------------------------------------------------------------------

describe('Add dialog per-role reasons', () => {
  it('addModeRoleReason reports the catalog reason in Add mode only', () => {
    const owner = ADMIN_CATALOG[0];
    expect(addModeRoleReason(owner, 'add')).toBe('requires project owner');
    expect(addModeRoleReason(owner, 'edit')).toBe('');
    expect(addModeRoleReason(R_MEMBER, 'add')).toBe('');
    expect(addModeRoleReason({ ...R_OWNER, grantable: false, reason: '' }, 'add')).toBe('');
  });

  it('admin Add dialog shows each disabled role with its assignable-roles reason', async () => {
    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, ADMIN_CATALOG);
    el.openAddDialog();
    await el.updateComplete;

    // Built-in roles above the admin's tier stay visible, disabled, with the
    // hub's reason rather than the generic tier text.
    for (const v of ['r-owner', 'r-admin']) {
      expect(radio(el, v).hasAttribute('disabled')).toBe(true);
      expect(radio(el, v).textContent).toContain('requires project owner');
      expect(radio(el, v).textContent).not.toContain(TIER_REASON);
    }
    expect(radio(el, 'r-member').hasAttribute('disabled')).toBe(false);

    // Custom roles are listed (not omitted), disabled, each with its reason.
    for (const id of ['r-msg', 'r-ceil']) {
      const cb = checkbox(el, id);
      expect(cb).not.toBeNull();
      expect(cb!.hasAttribute('disabled')).toBe(true);
      expect(cb!.textContent).toContain('custom role changes require role-binding authority');
    }
  });

  it('falls back to the tier text when the catalog gives no reason', async () => {
    const el = await mountEditor(ALL_GROUPS, ADMIN_CAPS, [R_OWNER, R_ADMIN, R_MEMBER]);
    el.openAddDialog();
    await el.updateComplete;
    expect(radio(el, 'r-owner').textContent).toContain(TIER_REASON);
  });

  it('Edit mode keeps the tier text and leaves custom checkboxes without a reason', () => {
    expect(customRoleState(ADMIN_CATALOG[4], { caps: ADMIN_CAPS, held: false })).toEqual({
      disabled: true,
      reason: '',
    });
    expect(
      customRoleState(ADMIN_CATALOG[4], { caps: ADMIN_CAPS, held: true, mode: 'add' })
    ).toEqual({ disabled: true, reason: '' });
  });
});
