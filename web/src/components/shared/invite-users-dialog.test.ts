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

// @vitest-environment happy-dom

import { describe, it, expect, vi, afterEach } from 'vitest';

import {
  parseInviteAddressInput,
  parseInviteCsv,
  type InviteBatchResponse,
} from './invite-batch-utils.js';
import './invite-batch-results.js';
import type { ScionInviteBatchResults } from './invite-batch-results.js';
import './invite-users-dialog.js';
import type { ScionInviteUsersDialog } from './invite-users-dialog.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function requestUrl(input: string | URL | Request): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.toString();
  return input.url;
}

describe('invite-batch-utils parsing & deduplication', () => {
  it('parses newline-, comma-, and semicolon-separated addresses and skips comments/blanks', () => {
    const raw = [
      '# Team onboarding list',
      'alice@example.com, bob@example.com',
      '',
      '  carol@example.com ; dave@example.com ',
      '# trailing comment',
    ].join('\n');

    const parsed = parseInviteAddressInput(raw);
    expect(parsed.addresses).toEqual([
      'alice@example.com',
      'bob@example.com',
      'carol@example.com',
      'dave@example.com',
    ]);
    expect(parsed.totalTokens).toBe(4);
    expect(parsed.duplicateCount).toBe(0);
  });

  it('deduplicates addresses case-insensitively while preserving first occurrence', () => {
    const parsed = parseInviteAddressInput(
      'Alice@Example.com\nbob@example.com, alice@example.com\nBOB@EXAMPLE.COM'
    );
    expect(parsed.addresses).toEqual(['Alice@Example.com', 'bob@example.com']);
    expect(parsed.totalTokens).toBe(4);
    expect(parsed.duplicateCount).toBe(2);
  });

  it('parses CSV with optional header, notes, quoted fields, BOM, comments, and deduplication', () => {
    const csv = [
      '\uFEFFemail,note',
      '# Engineering',
      'alice@example.com,New hire',
      'bob@example.com,"Contractor, Q3 ""infra"""',
      'ALICE@example.com,Duplicate note ignored',
      'carol@example.com',
    ].join('\n');

    const parsed = parseInviteCsv(csv);
    expect(parsed.entries).toEqual([
      { email: 'alice@example.com', note: 'New hire' },
      { email: 'bob@example.com', note: 'Contractor, Q3 "infra"' },
      { email: 'carol@example.com' },
    ]);
    expect(parsed.totalRows).toBe(4);
    expect(parsed.duplicateCount).toBe(1);
  });
});

describe('scion-invite-batch-results', () => {
  let el: ScionInviteBatchResults | null = null;

  afterEach(() => {
    el?.remove();
    el = null;
  });

  async function mountResults(
    response: InviteBatchResponse,
    projectLabels: Record<string, string> = {},
    roleLabels: Record<string, string> = {}
  ): Promise<ScionInviteBatchResults> {
    const comp = document.createElement('scion-invite-batch-results') as ScionInviteBatchResults;
    comp.response = response;
    comp.projectLabels = projectLabels;
    comp.roleLabels = roleLabels;
    document.body.appendChild(comp);
    await comp.updateComplete;
    return comp;
  }

  it('renders counts and per-address outcomes across all statuses and membership results', async () => {
    const response: InviteBatchResponse = {
      batchId: 'batch-1',
      invited: 1,
      skipped: 5,
      total: 8,
      complete: true,
      counts: {
        created: 1,
        already_invited: 1,
        already_active: 1,
        suspended: 1,
        exists: 1,
        invalid: 1,
        duplicate: 1,
        failed: 1,
        membership_failed: 1,
      },
      results: [
        {
          index: 0,
          email: 'new@example.com',
          status: 'created',
          userId: 'u-1',
          memberships: [{ projectId: 'p-1', status: 'applied' }],
        },
        {
          index: 1,
          email: 'pending@example.com',
          status: 'already_invited',
          memberships: [
            {
              projectId: 'p-1',
              status: 'conflict',
              code: 'member_with_other_roles',
              currentRoleDefinitionIds: ['r-viewer'],
            },
          ],
        },
        {
          index: 2,
          email: 'active@example.com',
          status: 'already_active',
          memberships: [{ projectId: 'p-1', status: 'skipped', code: 'address_not_invited' }],
        },
        {
          index: 3,
          email: 'susp@example.com',
          status: 'suspended',
          reason: 'user_suspended_exists',
        },
        {
          index: 4,
          email: 'exists@example.com',
          status: 'exists',
        },
        {
          index: 5,
          input: 'not-an-email',
          status: 'invalid',
          reason: 'invalid_email',
        },
        {
          index: 6,
          email: 'new@example.com',
          status: 'duplicate',
          reason: 'duplicate_in_batch',
          duplicateOf: 0,
        },
        {
          index: 7,
          email: 'err@example.com',
          status: 'failed',
          reason: 'internal_error',
          memberships: [
            { projectId: 'p-1', status: 'failed', code: 'internal_error', cause: 'db timeout' },
          ],
        },
      ],
    };

    el = await mountResults(response, { 'p-1': 'Alpha Project' }, { 'r-viewer': 'Project Viewer' });

    const root = el.shadowRoot!;
    expect(root.querySelector('.incomplete-alert')).toBeNull();

    // Count chips
    const chips = Array.from(root.querySelectorAll('.counts-bar sl-tag')).map((t) =>
      t.textContent?.trim()
    );
    expect(chips).toContain('Created: 1');
    expect(chips).toContain('Already invited: 1');
    expect(chips).toContain('Already active: 1');
    expect(chips).toContain('Suspended: 1');
    expect(chips).toContain('Already exists: 1');
    expect(chips).toContain('Invalid: 1');
    expect(chips).toContain('Duplicate: 1');
    expect(chips).toContain('Failed: 1');
    expect(chips).toContain('Membership failed: 1');

    // Per-address rows
    const rows = root.querySelectorAll('.result-row');
    expect(rows.length).toBe(8);
    expect(root.textContent).toContain('not-an-email');
    expect(root.textContent).toContain('Invalid email address');
    expect(root.textContent).toContain('Duplicate of entry #1');
    expect(root.textContent).toContain('Alpha Project');
    expect(root.textContent).toContain('Already holds other roles (Project Viewer)');
    expect(root.textContent).toContain('Skipped (account is not in invited status)');
    expect(root.textContent).toContain('db timeout');

    // Issues filter narrows rows to issue results (conflict, suspended, invalid, failed = 4)
    const issuesBtn = Array.from(root.querySelectorAll('.filter-buttons sl-button')).find((b) =>
      b.textContent?.includes('Issues')
    ) as HTMLElement;
    expect(issuesBtn).toBeTruthy();
    issuesBtn.click();
    await el.updateComplete;
    expect(root.querySelectorAll('.result-row').length).toBe(4);
  });

  it('shows incomplete batch warning when complete is false', async () => {
    el = await mountResults({
      batchId: 'batch-partial',
      invited: 1,
      skipped: 0,
      total: 2,
      complete: false,
      counts: {
        created: 1,
        already_invited: 0,
        already_active: 0,
        suspended: 0,
        invalid: 0,
        duplicate: 0,
        failed: 1,
        membership_failed: 0,
      },
      results: [
        { index: 0, email: 'a@example.com', status: 'created' },
        { index: 1, email: 'b@example.com', status: 'failed', reason: 'not_processed' },
      ],
    });

    const alert = el.shadowRoot!.querySelector('.incomplete-alert');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('Batch did not finish completely');
    expect(el.shadowRoot!.textContent).toContain('safe to retry');
  });

  it('resets filterMode to all when response changes after filtering to Issues', async () => {
    el = await mountResults({
      batchId: 'batch-1',
      invited: 1,
      skipped: 0,
      total: 2,
      complete: false,
      counts: {
        created: 1,
        already_invited: 0,
        already_active: 0,
        suspended: 0,
        invalid: 0,
        duplicate: 0,
        failed: 1,
        membership_failed: 0,
      },
      results: [
        { index: 0, email: 'a@example.com', status: 'created' },
        { index: 1, email: 'b@example.com', status: 'failed', reason: 'not_processed' },
      ],
    });

    const root = el.shadowRoot!;
    const issuesBtn = Array.from(root.querySelectorAll('.filter-buttons sl-button')).find((b) =>
      b.textContent?.includes('Issues')
    ) as HTMLElement;
    expect(issuesBtn).toBeTruthy();
    issuesBtn.click();
    await el.updateComplete;
    expect(root.querySelectorAll('.result-row').length).toBe(1);

    // Swap in a retry response where all issues are cleared
    el.response = {
      batchId: 'batch-2',
      invited: 1,
      skipped: 1,
      total: 2,
      complete: true,
      counts: {
        created: 1,
        already_invited: 1,
        already_active: 0,
        suspended: 0,
        invalid: 0,
        duplicate: 0,
        failed: 0,
        membership_failed: 0,
      },
      results: [
        { index: 0, email: 'a@example.com', status: 'already_invited' },
        { index: 1, email: 'b@example.com', status: 'created' },
      ],
    };
    await el.updateComplete;

    expect(root.querySelectorAll('.result-row').length).toBe(2);
    expect(root.textContent).toContain('Showing 2 of 2');
    expect(root.querySelector('.empty-filter')).toBeNull();
  });
});

describe('scion-invite-users-dialog', () => {
  let dialogEl: ScionInviteUsersDialog | null = null;

  afterEach(() => {
    dialogEl?.remove();
    dialogEl = null;
    vi.restoreAllMocks();
  });

  async function mountDialog(
    opts: {
      mode?: 'invite' | 'csv';
      canReadUsers?: boolean;
    } = {}
  ): Promise<ScionInviteUsersDialog> {
    const el = document.createElement('scion-invite-users-dialog') as ScionInviteUsersDialog;
    el.mode = opts.mode ?? 'invite';
    el.canReadUsers = opts.canReadUsers ?? true;
    el.open = true;
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  function setTextarea(el: ScionInviteUsersDialog, value: string): void {
    const ta = el.shadowRoot!.querySelector('sl-textarea') as HTMLTextAreaElement;
    expect(ta).not.toBeNull();
    ta.value = value;
    ta.dispatchEvent(new Event('sl-input'));
  }

  function clickButton(el: ScionInviteUsersDialog, textMatch: string): void {
    const btn = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find((b) =>
      b.textContent?.trim().includes(textMatch)
    ) as HTMLElement | undefined;
    expect(btn, `button "${textMatch}"`).toBeTruthy();
    btn!.click();
  }

  it('shows Display name only for a single address without project memberships', async () => {
    dialogEl = await mountDialog({ canReadUsers: true });

    // Empty / 0 addresses: Display name shown
    expect(
      dialogEl.shadowRoot!.querySelector('sl-input[label="Display name (optional)"]')
    ).not.toBeNull();

    // 1 address: Display name still shown
    setTextarea(dialogEl, 'alice@example.com');
    await dialogEl.updateComplete;
    expect(
      dialogEl.shadowRoot!.querySelector('sl-input[label="Display name (optional)"]')
    ).not.toBeNull();

    // 2 addresses: Display name hidden
    setTextarea(dialogEl, 'alice@example.com\nbob@example.com');
    await dialogEl.updateComplete;
    expect(
      dialogEl.shadowRoot!.querySelector('sl-input[label="Display name (optional)"]')
    ).toBeNull();

    // Back to 1 address, type a Display name, then add a project: Display name hidden with hint
    setTextarea(dialogEl, 'alice@example.com');
    await dialogEl.updateComplete;
    const displayNameInput = dialogEl.shadowRoot!.querySelector(
      'sl-input[label="Display name (optional)"]'
    ) as HTMLInputElement;
    expect(displayNameInput).not.toBeNull();
    displayNameInput.value = 'Alice Smith';
    displayNameInput.dispatchEvent(new Event('sl-input'));
    await dialogEl.updateComplete;

    clickButton(dialogEl, 'Add project');
    await dialogEl.updateComplete;
    expect(
      dialogEl.shadowRoot!.querySelector('sl-input[label="Display name (optional)"]')
    ).toBeNull();
    const hint = dialogEl.shadowRoot!.querySelector('[data-testid="display-name-omitted-hint"]');
    expect(hint).not.toBeNull();
    expect(hint!.textContent).toContain('will not be sent');

    // Removing the project brings Display name back and hides the hint
    const removeBtn = dialogEl.shadowRoot!.querySelector(
      'sl-icon-button[label="Remove project"]'
    ) as HTMLElement;
    removeBtn.click();
    await dialogEl.updateComplete;
    expect(
      dialogEl.shadowRoot!.querySelector('sl-input[label="Display name (optional)"]')
    ).not.toBeNull();
    expect(
      dialogEl.shadowRoot!.querySelector('[data-testid="display-name-omitted-hint"]')
    ).toBeNull();
  });

  it('hides Add project and explains user.read requirement when canReadUsers is false', async () => {
    dialogEl = await mountDialog({ canReadUsers: false });

    expect(
      dialogEl.shadowRoot!.querySelector('[data-testid="projects-permission-hint"]')?.textContent
    ).toContain('user.read');
    const addBtn = Array.from(dialogEl.shadowRoot!.querySelectorAll('sl-button')).find((b) =>
      b.textContent?.includes('Add project')
    );
    expect(addBtn).toBeUndefined();
  });

  it('submits multi-address batch with project memberships and renders per-address results', async () => {
    const fetchMock = vi.fn((url: string | URL | Request, init?: RequestInit) => {
      const path = requestUrl(url);
      if (path.endsWith('/api/v1/projects/proj-1')) {
        return Promise.resolve(jsonResponse({ id: 'proj-1', name: 'Alpha', slug: 'alpha' }));
      }
      if (path.includes('/api/v1/projects/proj-1/members/assignable-roles')) {
        return Promise.resolve(
          jsonResponse({
            items: [
              {
                id: 'role-member',
                name: 'Project Member',
                description: 'Standard member',
                roleKind: 'builtin',
                grantable: true,
                reason: '',
              },
              {
                id: 'role-owner',
                name: 'Project Owner',
                description: 'Full owner',
                roleKind: 'builtin',
                grantable: false,
                reason: 'Only owners may grant Owner',
              },
            ],
          })
        );
      }
      if (init?.method === 'POST' && path.includes('/api/v1/admin/users/invite/bulk')) {
        return Promise.resolve(
          jsonResponse({
            batchId: 'b-1',
            invited: 2,
            skipped: 0,
            total: 2,
            complete: true,
            counts: {
              created: 2,
              already_invited: 0,
              already_active: 0,
              suspended: 0,
              invalid: 0,
              duplicate: 0,
              failed: 0,
              membership_failed: 0,
            },
            results: [
              {
                index: 0,
                email: 'alice@example.com',
                status: 'created',
                memberships: [{ projectId: 'proj-1', status: 'applied' }],
              },
              {
                index: 1,
                email: 'bob@example.com',
                status: 'created',
                memberships: [{ projectId: 'proj-1', status: 'applied' }],
              },
            ],
          } satisfies InviteBatchResponse)
        );
      }
      return Promise.resolve(jsonResponse({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    dialogEl = await mountDialog({ canReadUsers: true });
    const completedSpy = vi.fn();
    dialogEl.addEventListener('invite-completed', completedSpy);

    setTextarea(dialogEl, 'alice@example.com, bob@example.com\nALICE@example.com');
    const noteInput = dialogEl.shadowRoot!.querySelector(
      'sl-input[label="Note (optional)"]'
    ) as HTMLInputElement;
    noteInput.value = 'Q4 cohort';
    noteInput.dispatchEvent(new Event('sl-input'));
    await dialogEl.updateComplete;

    expect(
      dialogEl
        .shadowRoot!.querySelector('[data-testid="address-summary"]')
        ?.textContent?.replace(/\s+/g, ' ')
        .trim()
    ).toBe('2 addresses (1 duplicate removed)');

    clickButton(dialogEl, 'Add project');
    await dialogEl.updateComplete;

    const picker = dialogEl.shadowRoot!.querySelector('scion-project-picker')!;
    picker.dispatchEvent(
      new CustomEvent('project-change', {
        detail: { projectId: 'proj-1', displayLabel: 'Alpha (alpha)' },
        bubbles: true,
        composed: true,
      })
    );
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    // Non-grantable built-in role is a disabled radio with reason
    const ownerRadio = dialogEl.shadowRoot!.querySelector('sl-radio[data-role-id="role-owner"]')!;
    expect(ownerRadio.hasAttribute('disabled')).toBe(true);
    expect(dialogEl.shadowRoot!.textContent).toContain('Only owners may grant Owner');

    // Select grantable built-in role via radio group
    const radioGroup = dialogEl.shadowRoot!.querySelector('sl-radio-group') as HTMLInputElement;
    expect(radioGroup).not.toBeNull();
    radioGroup.value = 'role-member';
    radioGroup.dispatchEvent(new Event('sl-change'));
    await dialogEl.updateComplete;

    clickButton(dialogEl, 'Invite 2 Users');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const postCall = fetchMock.mock.calls.find(
      ([url, init]) =>
        requestUrl(url).includes('/api/v1/admin/users/invite/bulk') && init?.method === 'POST'
    );
    expect(postCall).toBeTruthy();
    const sentBody = JSON.parse(postCall![1]?.body as string) as Record<string, unknown>;
    expect(sentBody).toEqual({
      emails: [
        { email: 'alice@example.com', note: 'Q4 cohort' },
        { email: 'bob@example.com', note: 'Q4 cohort' },
      ],
      projects: [{ projectId: 'proj-1', roleDefinitionIds: ['role-member'] }],
    });

    expect(completedSpy).toHaveBeenCalledTimes(1);
    const resultsComp = dialogEl.shadowRoot!.querySelector(
      'scion-invite-batch-results'
    ) as ScionInviteBatchResults;
    expect(resultsComp).not.toBeNull();
    expect(resultsComp.response?.counts.created).toBe(2);
    expect(resultsComp.projectLabels['proj-1']).toBe('Alpha (alpha)');
  });

  it('handles 403 batch_projects_not_grantable inline without closing dialog', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request, init?: RequestInit) => {
        const path = requestUrl(url);
        if (path.endsWith('/api/v1/projects/proj-1')) {
          return Promise.resolve(jsonResponse({ id: 'proj-1', name: 'Alpha', slug: 'alpha' }));
        }
        if (path.includes('/assignable-roles')) {
          return Promise.resolve(
            jsonResponse({
              items: [
                {
                  id: 'role-admin',
                  name: 'Project Admin',
                  description: '',
                  roleKind: 'builtin',
                  grantable: true,
                  reason: '',
                },
              ],
            })
          );
        }
        if (init?.method === 'POST') {
          return Promise.resolve(
            jsonResponse(
              {
                error: {
                  code: 'forbidden',
                  message:
                    'one or more requested project memberships cannot be granted by the caller; nothing was created',
                  details: {
                    reason: 'batch_projects_not_grantable',
                    batchId: 'b-refused',
                    projects: [
                      {
                        index: 0,
                        projectId: 'proj-1',
                        reason: 'role_not_grantable',
                        roleDefinitionId: 'role-admin',
                      },
                    ],
                  },
                },
              },
              403
            )
          );
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    dialogEl = await mountDialog({ canReadUsers: true });
    setTextarea(dialogEl, 'a@example.com');
    clickButton(dialogEl, 'Add project');
    await dialogEl.updateComplete;

    dialogEl.shadowRoot!.querySelector('scion-project-picker')!.dispatchEvent(
      new CustomEvent('project-change', {
        detail: { projectId: 'proj-1', displayLabel: 'Alpha' },
      })
    );
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const radioGroup = dialogEl.shadowRoot!.querySelector('sl-radio-group') as HTMLInputElement;
    radioGroup.value = 'role-admin';
    radioGroup.dispatchEvent(new Event('sl-change'));
    await dialogEl.updateComplete;

    clickButton(dialogEl, 'Invite User');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const alert = dialogEl.shadowRoot!.querySelector('.submit-error-alert');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('nothing was created');
    expect(alert!.textContent).toContain('Alpha (Project Admin)');
    expect(dialogEl.shadowRoot!.querySelector('scion-invite-batch-results')).toBeNull();
  });

  it('handles 422 role_selection_not_supported error inline', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(
            {
              error: {
                code: 'unprocessable',
                message:
                  'hub roles cannot be set on an invite; the role is assigned at first sign-in',
                details: { reason: 'role_selection_not_supported' },
              },
            },
            422
          )
        )
      )
    );

    dialogEl = await mountDialog();
    setTextarea(dialogEl, 'a@example.com');
    await dialogEl.updateComplete;
    clickButton(dialogEl, 'Invite User');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const alert = dialogEl.shadowRoot!.querySelector('.submit-error-alert');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('hub roles cannot be set on an invite');
  });

  it('supports Retry batch when response.complete is false', async () => {
    let callCount = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        callCount++;
        return Promise.resolve(
          jsonResponse({
            batchId: `b-${callCount}`,
            invited: 1,
            skipped: 0,
            total: 2,
            complete: callCount > 1,
            counts: {
              created: callCount > 1 ? 1 : 1,
              already_invited: callCount > 1 ? 1 : 0,
              already_active: 0,
              suspended: 0,
              invalid: 0,
              duplicate: 0,
              failed: callCount > 1 ? 0 : 1,
              membership_failed: 0,
            },
            results: [
              {
                index: 0,
                email: 'a@example.com',
                status: callCount > 1 ? 'already_invited' : 'created',
              },
              {
                index: 1,
                email: 'b@example.com',
                status: callCount > 1 ? 'created' : 'failed',
                reason: callCount > 1 ? undefined : 'not_processed',
              },
            ],
          } satisfies InviteBatchResponse)
        );
      })
    );

    dialogEl = await mountDialog();
    setTextarea(dialogEl, 'a@example.com\nb@example.com');
    await dialogEl.updateComplete;
    clickButton(dialogEl, 'Invite 2 Users');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    // Retry button is visible on incomplete batch
    clickButton(dialogEl, 'Retry batch');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    expect(callCount).toBe(2);
    const retryBtn = Array.from(dialogEl.shadowRoot!.querySelectorAll('sl-button')).find((b) =>
      b.textContent?.includes('Retry batch')
    );
    expect(retryBtn).toBeUndefined();
  });

  it('imports CSV with per-row notes and optional project memberships and renders per-address results', async () => {
    const fetchMock = vi.fn((url: string | URL | Request, init?: RequestInit) => {
      const path = requestUrl(url);
      if (path.endsWith('/api/v1/projects/proj-2')) {
        return Promise.resolve(jsonResponse({ id: 'proj-2', name: 'Beta', slug: 'beta' }));
      }
      if (path.includes('/assignable-roles')) {
        return Promise.resolve(
          jsonResponse({
            items: [
              {
                id: 'role-viewer',
                name: 'Project Viewer',
                description: '',
                roleKind: 'builtin',
                grantable: true,
                reason: '',
              },
            ],
          })
        );
      }
      if (init?.method === 'POST' && path.includes('/api/v1/admin/users/invite/bulk')) {
        return Promise.resolve(
          jsonResponse({
            batchId: 'b-csv',
            invited: 2,
            skipped: 0,
            total: 2,
            complete: true,
            counts: {
              created: 2,
              already_invited: 0,
              already_active: 0,
              suspended: 0,
              invalid: 0,
              duplicate: 0,
              failed: 0,
              membership_failed: 0,
            },
            results: [
              { index: 0, email: 'csv1@example.com', status: 'created' },
              { index: 1, email: 'csv2@example.com', status: 'created' },
            ],
          } satisfies InviteBatchResponse)
        );
      }
      return Promise.resolve(jsonResponse({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    dialogEl = await mountDialog({ mode: 'csv', canReadUsers: true });

    const fileInput = dialogEl.shadowRoot!.querySelector('#import-file-input') as HTMLInputElement;
    const csvFile = new File(
      ['email,note\ncsv1@example.com,Alpha\ncsv2@example.com,Beta\n'],
      'team.csv',
      { type: 'text/csv' }
    );
    Object.defineProperty(fileInput, 'files', { value: [csvFile], configurable: true });
    fileInput.dispatchEvent(new Event('change'));
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    expect(
      dialogEl
        .shadowRoot!.querySelector('[data-testid="csv-summary"]')
        ?.textContent?.replace(/\s+/g, ' ')
        .trim()
    ).toContain('Loaded 2 addresses from team.csv');

    clickButton(dialogEl, 'Add project');
    await dialogEl.updateComplete;
    dialogEl.shadowRoot!.querySelector('scion-project-picker')!.dispatchEvent(
      new CustomEvent('project-change', {
        detail: { projectId: 'proj-2', displayLabel: 'Beta' },
      })
    );
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const radioGroup = dialogEl.shadowRoot!.querySelector('sl-radio-group') as HTMLInputElement;
    radioGroup.value = 'role-viewer';
    radioGroup.dispatchEvent(new Event('sl-change'));
    await dialogEl.updateComplete;

    clickButton(dialogEl, 'Import 2');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const postCall = fetchMock.mock.calls.find(
      ([url, init]) =>
        requestUrl(url).includes('/api/v1/admin/users/invite/bulk') && init?.method === 'POST'
    );
    expect(postCall).toBeTruthy();
    expect(JSON.parse(postCall![1]?.body as string)).toEqual({
      emails: [
        { email: 'csv1@example.com', note: 'Alpha' },
        { email: 'csv2@example.com', note: 'Beta' },
      ],
      projects: [{ projectId: 'proj-2', roleDefinitionIds: ['role-viewer'] }],
    });

    const resultsComp = dialogEl.shadowRoot!.querySelector(
      'scion-invite-batch-results'
    ) as ScionInviteBatchResults;
    expect(resultsComp).not.toBeNull();
    expect(resultsComp.response?.counts.created).toBe(2);
  });

  it('ignores bubbled sl-request-close events from nested elements', async () => {
    dialogEl = await mountDialog();
    const closeSpy = vi.fn();
    dialogEl.addEventListener('invite-dialog-close', closeSpy);

    const inner = dialogEl.shadowRoot!.querySelector('.invite-form')!;
    inner.dispatchEvent(new CustomEvent('sl-request-close', { bubbles: true, composed: true }));
    expect(closeSpy).not.toHaveBeenCalled();

    const dialog = dialogEl.shadowRoot!.querySelector('sl-dialog')!;
    dialog.dispatchEvent(new CustomEvent('sl-request-close', { bubbles: true, composed: true }));
    expect(closeSpy).toHaveBeenCalledTimes(1);
  });

  it('enforces single-select for built-in roles while allowing multiple custom roles', async () => {
    const fetchMock = vi.fn((url: string | URL | Request, init?: RequestInit) => {
      const path = requestUrl(url);
      if (path.endsWith('/api/v1/projects/proj-1')) {
        return Promise.resolve(jsonResponse({ id: 'proj-1', name: 'Alpha', slug: 'alpha' }));
      }
      if (path.includes('/assignable-roles')) {
        return Promise.resolve(
          jsonResponse({
            items: [
              {
                id: 'role-admin',
                name: 'project-admin',
                description: '',
                roleKind: 'builtin',
                grantable: true,
                reason: '',
              },
              {
                id: 'role-member',
                name: 'project-member',
                description: '',
                roleKind: 'builtin',
                grantable: true,
                reason: '',
              },
              {
                id: 'role-custom-a',
                name: 'Deployer',
                description: '',
                roleKind: 'custom',
                grantable: true,
                reason: '',
              },
              {
                id: 'role-custom-b',
                name: 'Auditor',
                description: '',
                roleKind: 'custom',
                grantable: true,
                reason: '',
              },
            ],
          })
        );
      }
      if (init?.method === 'POST' && path.includes('/api/v1/admin/users/invite/bulk')) {
        return Promise.resolve(
          jsonResponse({
            batchId: 'b-roles',
            invited: 1,
            skipped: 0,
            total: 1,
            complete: true,
            counts: {
              created: 1,
              already_invited: 0,
              already_active: 0,
              suspended: 0,
              invalid: 0,
              duplicate: 0,
              failed: 0,
              membership_failed: 0,
            },
            results: [{ index: 0, email: 'a@example.com', status: 'created' }],
          } satisfies InviteBatchResponse)
        );
      }
      return Promise.resolve(jsonResponse({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    dialogEl = await mountDialog({ canReadUsers: true });
    setTextarea(dialogEl, 'a@example.com');
    clickButton(dialogEl, 'Add project');
    await dialogEl.updateComplete;

    dialogEl.shadowRoot!.querySelector('scion-project-picker')!.dispatchEvent(
      new CustomEvent('project-change', {
        detail: { projectId: 'proj-1', displayLabel: 'Alpha' },
      })
    );
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    // Built-in roles are radios inside a single radio group (plus None because custom roles exist)
    const radioGroup = dialogEl.shadowRoot!.querySelector('sl-radio-group') as HTMLInputElement;
    expect(radioGroup).not.toBeNull();
    expect(
      dialogEl.shadowRoot!.querySelector('sl-radio[data-role-id="role-admin"]')
    ).not.toBeNull();
    expect(
      dialogEl.shadowRoot!.querySelector('sl-radio[data-role-id="role-member"]')
    ).not.toBeNull();
    expect(dialogEl.shadowRoot!.querySelector('sl-radio[data-role-id="__none__"]')).not.toBeNull();
    expect(dialogEl.shadowRoot!.querySelector('sl-checkbox[data-role-id="role-admin"]')).toBeNull();
    expect(
      dialogEl.shadowRoot!.querySelector('sl-checkbox[data-role-id="role-member"]')
    ).toBeNull();

    // Selecting first built-in role then second replaces the first
    radioGroup.value = 'role-admin';
    radioGroup.dispatchEvent(new Event('sl-change'));
    await dialogEl.updateComplete;

    radioGroup.value = 'role-member';
    radioGroup.dispatchEvent(new Event('sl-change'));
    await dialogEl.updateComplete;

    // Multiple custom roles can still be checked simultaneously
    const customA = dialogEl.shadowRoot!.querySelector(
      'sl-checkbox[data-role-id="role-custom-a"]'
    ) as HTMLInputElement;
    const customB = dialogEl.shadowRoot!.querySelector(
      'sl-checkbox[data-role-id="role-custom-b"]'
    ) as HTMLInputElement;
    customA.checked = true;
    customA.dispatchEvent(new Event('sl-change'));
    customB.checked = true;
    customB.dispatchEvent(new Event('sl-change'));
    await dialogEl.updateComplete;

    clickButton(dialogEl, 'Invite User');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const postCall = fetchMock.mock.calls.find(
      ([url, init]) =>
        requestUrl(url).includes('/api/v1/admin/users/invite/bulk') && init?.method === 'POST'
    );
    expect(postCall).toBeTruthy();
    const sentBody = JSON.parse(postCall![1]?.body as string) as {
      projects: Array<{ projectId: string; roleDefinitionIds: string[] }>;
    };
    expect(sentBody.projects).toEqual([
      {
        projectId: 'proj-1',
        roleDefinitionIds: ['role-member', 'role-custom-a', 'role-custom-b'],
      },
    ]);
  });

  it('shows submitError and refusalLines in results view when Retry batch fails and clears on next retry', async () => {
    let callCount = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        callCount++;
        if (callCount === 1) {
          return Promise.resolve(
            jsonResponse({
              batchId: 'b-1',
              invited: 1,
              skipped: 0,
              total: 2,
              complete: false,
              counts: {
                created: 1,
                already_invited: 0,
                already_active: 0,
                suspended: 0,
                invalid: 0,
                duplicate: 0,
                failed: 1,
                membership_failed: 0,
              },
              results: [
                { index: 0, email: 'a@example.com', status: 'created' },
                { index: 1, email: 'b@example.com', status: 'failed', reason: 'not_processed' },
              ],
            } satisfies InviteBatchResponse)
          );
        }
        if (callCount === 2) {
          return Promise.resolve(
            jsonResponse(
              {
                error: {
                  code: 'internal',
                  message: 'boom retry failed',
                },
              },
              500
            )
          );
        }
        if (callCount === 3) {
          return Promise.resolve(
            jsonResponse(
              {
                error: {
                  code: 'forbidden',
                  message: 'project permissions changed; nothing was created',
                  details: {
                    reason: 'batch_projects_not_grantable',
                    projects: [
                      {
                        index: 0,
                        projectId: 'proj-1',
                        reason: 'role_not_grantable',
                        roleDefinitionId: 'role-admin',
                      },
                    ],
                  },
                },
              },
              403
            )
          );
        }
        return Promise.resolve(
          jsonResponse({
            batchId: 'b-4',
            invited: 1,
            skipped: 1,
            total: 2,
            complete: true,
            counts: {
              created: 1,
              already_invited: 1,
              already_active: 0,
              suspended: 0,
              invalid: 0,
              duplicate: 0,
              failed: 0,
              membership_failed: 0,
            },
            results: [
              { index: 0, email: 'a@example.com', status: 'already_invited' },
              { index: 1, email: 'b@example.com', status: 'created' },
            ],
          } satisfies InviteBatchResponse)
        );
      })
    );

    dialogEl = await mountDialog();
    setTextarea(dialogEl, 'a@example.com\nb@example.com');
    await dialogEl.updateComplete;
    clickButton(dialogEl, 'Invite 2 Users');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    // First retry fails with 500 -> error banner is visible in results view
    clickButton(dialogEl, 'Retry batch');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const alert500 = dialogEl.shadowRoot!.querySelector('.submit-error-alert');
    expect(alert500).not.toBeNull();
    expect(alert500!.textContent).toContain('boom retry failed');
    expect(dialogEl.shadowRoot!.querySelector('scion-invite-batch-results')).not.toBeNull();

    // Second retry fails with 403 refusal -> error banner updates with refusal lines
    clickButton(dialogEl, 'Retry batch');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    const alert403 = dialogEl.shadowRoot!.querySelector('.submit-error-alert');
    expect(alert403).not.toBeNull();
    expect(alert403!.textContent).toContain('project permissions changed');
    expect(alert403!.textContent).toContain('proj-1');

    // Third retry succeeds -> error banner is cleared
    clickButton(dialogEl, 'Retry batch');
    await new Promise((r) => setTimeout(r, 20));
    await dialogEl.updateComplete;

    expect(dialogEl.shadowRoot!.querySelector('.submit-error-alert')).toBeNull();
  });
});
