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
 * Effective Role Provenance — Acceptance Tests
 *
 * Tests proving:
 *
 *   1. The "Effective access composition" section has been removed — no
 *      toggle, no layer stack, no effective-access API request fires from
 *      this dialog.
 *   2. The role-binding list and add/delete/provenance behavior remain
 *      intact.
 *   3. No redaction bypass in the boundary-notice sibling component.
 *   4. Role cards display names, not UUIDs.
 *
 * The authorization-layer-stack and explain-layer features were removed
 * because the backend only returns a system-scope activeBindingCount, not
 * a real per-permission composition.  A future standalone Effective
 * Permission Viewer is tracked in .design/effective-permission-viewer.md.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'fs';
import { resolve } from 'path';
import type { ScionEffectiveRoleProvenance } from './effective-role-provenance.js';
import { clearAdminStatus } from '../../client/admin-status.js';
import { stateManager } from '../../client/state.js';

// Mock confirm-dialog at module level so showConfirm auto-confirms in all tests.
vi.mock('./confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

/* -------------------------------------------------------------------------- */
/* Helper: extract html`` template content from a source file                 */
/* -------------------------------------------------------------------------- */

function extractTemplateContent(source: string): string {
  const htmlTemplates: string[] = [];
  const regex = /html`([\s\S]*?)`/g;
  let match;
  while ((match = regex.exec(source)) !== null) {
    htmlTemplates.push(match[1]);
  }
  return htmlTemplates.join('\n');
}

/**
 * Extract all user-facing string literals from html`` templates.
 * Strips tags and template expressions, leaving only visible text.
 */
function extractUserFacingText(source: string): string {
  const templates = extractTemplateContent(source);
  // Remove HTML tags
  let text = templates.replace(/<[^>]+>/g, ' ');
  // Remove template expressions ${...} — simple nesting only
  text = text.replace(/\$\{[^}]*\}/g, ' ');
  return text;
}

/* -------------------------------------------------------------------------- */
/* 1. Composition section has been removed                                     */
/* -------------------------------------------------------------------------- */

describe('Composition section removed', () => {
  it('effective-role-provenance.ts contains no "Effective access composition" text', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    const templates = extractTemplateContent(source);
    expect(templates).not.toContain('Effective access composition');
  });

  it('effective-role-provenance.ts does not render scion-authorization-layer-stack', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    const templates = extractTemplateContent(source);
    expect(templates).not.toContain('scion-authorization-layer-stack');
  });

  it('effective-role-provenance.ts does not import authorization-layer-stack', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    expect(source).not.toContain("from './authorization-layer-stack");
    expect(source).not.toContain("import './authorization-layer-stack");
  });

  it('effective-role-provenance.ts does not contain loadExplainLayers or preCheckExplainAccess', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    expect(source).not.toContain('loadExplainLayers');
    expect(source).not.toContain('preCheckExplainAccess');
  });

  it('effective-role-provenance.ts does not fetch /api/v1/admin/effective-access', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    expect(source).not.toContain('/api/v1/admin/effective-access');
  });

  it('effective-role-provenance.ts does not contain _explainLoaded, _explainForbidden, or _explainPreChecked', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    expect(source).not.toContain('_explainLoaded');
    expect(source).not.toContain('_explainForbidden');
    expect(source).not.toContain('_explainPreChecked');
  });

  it('effective-role-provenance.ts does not contain layers-toggle CSS class', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    expect(source).not.toContain('.layers-toggle');
  });
});

/* -------------------------------------------------------------------------- */
/* 2. No prohibited terminology in remaining components                        */
/* -------------------------------------------------------------------------- */

describe('No priority/override/winner terminology in remaining components', () => {
  const componentsToCheck = [
    {
      name: 'effective-role-provenance.ts',
      path: './effective-role-provenance.ts',
    },
    {
      name: 'effective-access-boundary-notice.ts',
      path: './effective-access-boundary-notice.ts',
    },
  ];

  const prohibitedTerms = [
    /\bpriority\b/i,
    /\boverride\b/i,
    /\boverrides\b/i,
    /\boverriding\b/i,
    /\bwinner\b/i,
    /\bwinning\b/i,
    /\bwon by\b/i,
    /\bwins\b/i,
  ];

  for (const component of componentsToCheck) {
    describe(`${component.name}`, () => {
      it('has no prohibited terminology in user-facing template strings', () => {
        const source = readFileSync(resolve(__dirname, component.path), 'utf-8');
        const userText = extractUserFacingText(source);

        for (const term of prohibitedTerms) {
          expect(
            userText,
            `Found prohibited term ${term} in ${component.name} template text`
          ).not.toMatch(term);
        }
      });
    });
  }
});

/* -------------------------------------------------------------------------- */
/* 3. No redaction bypass in effective-access-boundary-notice                   */
/* -------------------------------------------------------------------------- */

describe('No redaction bypass in effective-access-boundary-notice', () => {
  it('no secondary apiFetch call fetches boundary names by ID', () => {
    const source = readFileSync(
      resolve(__dirname, './effective-access-boundary-notice.ts'),
      'utf-8'
    );
    // The notice component should not fetch individual boundary details
    expect(source).not.toContain('/api/v1/admin/access-boundaries/');
  });

  it('does not target nonexistent /admin/access-explain route', () => {
    const source = readFileSync(
      resolve(__dirname, './effective-access-boundary-notice.ts'),
      'utf-8'
    );
    expect(source).not.toContain('/api/v1/admin/access-explain');
    expect(source).not.toContain("'/admin/access-explain");
    expect(source).not.toContain('"/admin/access-explain');
  });
});

/* -------------------------------------------------------------------------- */
/* 4. Role card displays role name, not UUID                                   */
/* -------------------------------------------------------------------------- */

describe('Role cards display role names, not UUIDs', () => {
  it('renderRoleCard uses roleName with roleDefinitionId as fallback', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    expect(source).toContain('binding.roleName || binding.roleDefinitionId');
  });

  it('EffectiveRoleBinding interface includes roleName field', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    expect(source).toMatch(/roleName:\s*string/);
  });
});

/* -------------------------------------------------------------------------- */
/* 5. Boundary-notice loaded-flag pattern still works                          */
/* -------------------------------------------------------------------------- */

describe('Boundary-notice loaded-flag pattern', () => {
  it('effective-access-boundary-notice uses loaded flag', () => {
    const source = readFileSync(
      resolve(__dirname, './effective-access-boundary-notice.ts'),
      'utf-8'
    );
    expect(source).toMatch(/@state\(\)\s+private\s+loaded\s*=\s*false/);
    expect(source).toContain('this.loaded = true');
  });
});

/* ========================================================================== */
/* Behavioral Component Tests                                                  */
/*                                                                            */
/* These tests mount the real component, mock fetch, and exercise rendered     */
/* buttons/events rather than asserting on source strings.                     */
/* ========================================================================== */

/** Wait for async loads + Lit update cycle. */
async function tick(el: { updateComplete: Promise<boolean> }, ms = 250): Promise<void> {
  await new Promise((r) => setTimeout(r, ms));
  await el.updateComplete;
}

function query(el: HTMLElement, sel: string): Element | null {
  return el.shadowRoot?.querySelector(sel) ?? null;
}

function queryAll(el: HTMLElement, sel: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(sel) ?? []);
}

/** Standard mock bindings: one direct, one group-derived. */
function makeBindings() {
  return {
    items: [
      {
        id: 'b-direct',
        roleDefinitionId: 'role-1',
        roleName: 'Editor',
        principalType: 'user',
        principalId: 'user-1',
        scopeType: 'system',
        scopeId: '',
        createdAt: '2026-01-01T00:00:00Z',
        source: 'direct',
      },
      {
        id: 'b-group',
        roleDefinitionId: 'role-2',
        roleName: 'Viewer',
        principalType: 'group',
        principalId: 'group-1',
        scopeType: 'system',
        scopeId: '',
        createdAt: '2026-01-02T00:00:00Z',
        source: 'group-1',
        sourceGroupName: 'Eng Team',
      },
    ],
    totalCount: 2,
  };
}

/** Every role-binding permission the panel reads. */
const ALL_ROLE_BINDING_PERMISSIONS = [
  'role_binding.read',
  'role_binding.create',
  'role_binding.delete',
];

/** Admin-status response body for a viewer holding `permissions`. */
function adminStatusResponse(permissions: string[]): Response {
  return new Response(JSON.stringify({ isAdmin: false, isSuperAdmin: false, permissions }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

/**
 * Build a fetch handler that answers the admin-status read and the
 * role-binding requests, recording every call.
 *
 * @param opts.permissions  Permission IDs listed by admin-status
 *   (defaults to all role-binding permissions).
 * @param opts.adminStatusStatus  HTTP status for admin-status (default 200).
 */
function makeFetchHandler(opts?: { permissions?: string[]; adminStatusStatus?: number }) {
  const permissions = opts?.permissions ?? ALL_ROLE_BINDING_PERMISSIONS;
  const adminStatusStatus = opts?.adminStatusStatus ?? 200;
  const calls: { url: string; method: string; body?: string }[] = [];

  const handler = (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    const method = init?.method ?? 'GET';
    const body = typeof init?.body === 'string' ? init.body : undefined;
    calls.push({ url: path, method, body });

    if (path === '/api/v1/auth/admin-status') {
      if (adminStatusStatus !== 200) {
        return Promise.resolve(new Response('{}', { status: adminStatusStatus }));
      }
      return Promise.resolve(adminStatusResponse(permissions));
    }

    // Create a binding
    if (path === '/api/v1/admin/role-bindings' && method === 'POST') {
      return Promise.resolve(new Response(JSON.stringify({ id: 'new-binding' }), { status: 201 }));
    }

    // Remove a binding
    if (path.includes('/api/v1/admin/role-bindings/b-direct') && method === 'DELETE') {
      return Promise.resolve(new Response('', { status: 204 }));
    }

    // Binding list
    if (path.includes('/api/v1/admin/role-bindings') && method === 'GET') {
      return Promise.resolve(
        new Response(JSON.stringify(makeBindings()), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // Role definitions (for add dialog)
    if (path.includes('/api/v1/admin/roles') && method === 'GET') {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            items: [
              { id: 'role-1', name: 'Editor', scopeType: 'system' },
              { id: 'role-2', name: 'Viewer', scopeType: 'system' },
            ],
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    return Promise.resolve(new Response('{}', { status: 200 }));
  };
  return { handler, calls };
}

/** Role-binding list GETs among the recorded calls. */
function listCalls(calls: { url: string; method: string }[]) {
  return calls.filter((c) => c.method === 'GET' && c.url.startsWith('/api/v1/admin/role-bindings'));
}

async function createEl(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>,
  props?: { principalType?: string; principalId?: string }
) {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-effective-role-provenance') as InstanceType<
    typeof ScionEffectiveRoleProvenance
  >;
  el.principalType = (props?.principalType ?? 'user') as 'user' | 'agent';
  el.principalId = props?.principalId ?? 'user-1';
  document.body.appendChild(el);
  await tick(el, 400);
  return el;
}

beforeAll(async () => {
  await import('./effective-role-provenance.js');
  await import('./effective-access-boundary-notice.js');
});

beforeEach(() => {
  clearAdminStatus();
  stateManager.setCurrentUserId('viewer-1');
});

afterEach(() => {
  document.body.innerHTML = '';
  clearAdminStatus();
  vi.restoreAllMocks();
});

/* -------------------------------------------------------------------------- */
/* 6. Behavioral: composition section absent from rendered output              */
/* -------------------------------------------------------------------------- */

describe('Behavioral: composition section absent', () => {
  it('does not render a layers-toggle element', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    const toggle = query(el, '.layers-toggle');
    expect(toggle).toBeNull();
  });

  it('does not render scion-authorization-layer-stack', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    const layerStack = query(el, 'scion-authorization-layer-stack');
    expect(layerStack).toBeNull();
  });

  it('does not contain "Effective access composition" text', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    const shadow = el.shadowRoot?.innerHTML ?? '';
    expect(shadow).not.toContain('Effective access composition');
  });
});

/* -------------------------------------------------------------------------- */
/* 7. Behavioral: no effective-access API request fires                        */
/* -------------------------------------------------------------------------- */

describe('Behavioral: no effective-access API request', () => {
  it('does not send any request to /api/v1/admin/effective-access', async () => {
    const { handler, calls } = makeFetchHandler();
    await createEl(handler);

    const effectiveAccessCalls = calls.filter((c) =>
      c.url.includes('/api/v1/admin/effective-access')
    );
    expect(effectiveAccessCalls.length).toBe(0);
  });
});

/* -------------------------------------------------------------------------- */
/* 8. Behavioral: role-binding list still renders                              */
/* -------------------------------------------------------------------------- */

describe('Behavioral: role-binding list renders', () => {
  it('renders role cards for each binding', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    const cards = queryAll(el, '.role-card');
    expect(cards.length).toBe(2);
  });

  it('displays role names on cards', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    const roleNames = queryAll(el, '.role-name');
    const names = roleNames.map((n) => n.textContent?.trim());
    expect(names).toContain('Editor');
    expect(names).toContain('Viewer');
  });
});

/* -------------------------------------------------------------------------- */
/* 9. Behavioral: create-only permission (can create, cannot delete)           */
/* -------------------------------------------------------------------------- */

describe('Behavioral: create-only permission', () => {
  it('shows Add Binding button but no trash icons', async () => {
    const { handler } = makeFetchHandler({
      permissions: ['role_binding.read', 'role_binding.create'],
    });
    const el = await createEl(handler);

    // Add Binding button should be visible
    const addBtn = query(el, 'sl-button[variant="primary"]');
    expect(addBtn).toBeTruthy();
    expect(addBtn?.textContent?.trim()).toContain('Add Binding');

    // Trash icons should NOT be present (delete denied)
    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(0);
  });
});

/* -------------------------------------------------------------------------- */
/* 10. Behavioral: delete-only permission (can delete, cannot create)          */
/* -------------------------------------------------------------------------- */

describe('Behavioral: delete-only permission', () => {
  it('shows trash icon on direct binding but no Add Binding button', async () => {
    const { handler } = makeFetchHandler({
      permissions: ['role_binding.read', 'role_binding.delete'],
    });
    const el = await createEl(handler);

    // Add Binding button should NOT be visible
    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeUndefined();

    // Trash icon should be present on the direct binding
    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(1); // Only on the direct binding, not the group one
  });
});

/* -------------------------------------------------------------------------- */
/* 11. Behavioral: neither permission (cannot create, cannot delete)           */
/* -------------------------------------------------------------------------- */

describe('Behavioral: neither create nor delete permission', () => {
  it('shows no Add Binding button and no trash icons', async () => {
    const { handler } = makeFetchHandler({ permissions: ['role_binding.read'] });
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeUndefined();

    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(0);
  });
});

/* -------------------------------------------------------------------------- */
/* 12. Behavioral: both permissions (can create and delete)                    */
/* -------------------------------------------------------------------------- */

describe('Behavioral: both create and delete permissions', () => {
  it('shows Add Binding button and trash icon on direct binding', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    // Add Binding button present
    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeTruthy();

    // Trash icon on direct binding
    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(1);
  });

  it('trash icon is only on direct bindings, not group-derived', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    // There are 2 role cards total
    const cards = queryAll(el, '.role-card');
    expect(cards.length).toBe(2);

    // Only the direct binding card has a trash icon
    const directCard = cards.find((c) => c.querySelector('.provenance.direct'));
    const groupCard = cards.find((c) => c.querySelector('.provenance.group'));

    expect(directCard?.querySelector('sl-icon-button[name="trash"]')).toBeTruthy();
    expect(groupCard?.querySelector('sl-icon-button[name="trash"]')).toBeFalsy();
  });
});

/* -------------------------------------------------------------------------- */
/* 13. Behavioral: pending admin status hides controls and the list           */
/* -------------------------------------------------------------------------- */

describe('Behavioral: pending admin status', () => {
  it('hides Add Binding and trash icons and sends no list request until admin status resolves', async () => {
    const calls: { url: string; method: string }[] = [];
    const pendingAdminStatus = (
      url: string | URL | Request,
      init?: RequestInit
    ): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      calls.push({ url: path, method: init?.method ?? 'GET' });
      if (path === '/api/v1/auth/admin-status') {
        return new Promise(() => {}); // never resolves
      }
      return Promise.resolve(
        new Response(JSON.stringify(makeBindings()), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    };

    const el = await createEl(pendingAdminStatus);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeUndefined();
    expect(queryAll(el, 'sl-icon-button[name="trash"]').length).toBe(0);
    expect(listCalls(calls).length).toBe(0);
  });
});

/* -------------------------------------------------------------------------- */
/* 14. Behavioral: direct vs inherited binding provenance labels               */
/* -------------------------------------------------------------------------- */

describe('Behavioral: direct vs inherited binding provenance', () => {
  it('renders "Direct" label on direct binding and "Via group:" on inherited', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    const provenances = queryAll(el, '.provenance');
    expect(provenances.length).toBe(2);

    const directProv = provenances.find((p) => p.classList.contains('direct'));
    expect(directProv).toBeTruthy();
    expect(directProv?.textContent).toContain('Direct');

    const groupProv = provenances.find((p) => p.classList.contains('group'));
    expect(groupProv).toBeTruthy();
    expect(groupProv?.textContent).toContain('Via group:');
    expect(groupProv?.textContent).toContain('Eng Team');
  });
});

/* -------------------------------------------------------------------------- */
/* 15. Behavioral: delete confirmation flow                                    */
/* -------------------------------------------------------------------------- */

describe('Behavioral: delete confirmation and request', () => {
  it('clicking trash triggers DELETE request for direct binding', async () => {
    const { handler, calls } = makeFetchHandler();
    const el = await createEl(handler);

    const trashBtn = query(el, 'sl-icon-button[name="trash"]') as HTMLElement;
    expect(trashBtn).toBeTruthy();
    trashBtn.click();
    await tick(el, 400);

    const deleteCalls = calls.filter((c) => c.method === 'DELETE' && c.url.includes('b-direct'));
    expect(deleteCalls.length).toBe(1);
  });

  it('sends DELETE and triggers refresh on success', async () => {
    const { handler, calls } = makeFetchHandler();
    const el = await createEl(handler);
    const initialGetCalls = calls.filter(
      (c) => c.method === 'GET' && c.url.includes('role-bindings')
    ).length;

    const trashBtn = query(el, 'sl-icon-button[name="trash"]') as HTMLElement;
    trashBtn?.click();
    await tick(el, 600);

    // Verify DELETE was sent for the direct binding
    const deleteCalls = calls.filter((c) => c.method === 'DELETE' && c.url.includes('b-direct'));
    expect(deleteCalls.length).toBe(1);

    // Verify a refresh GET was triggered after the delete
    const postDeleteGetCalls = calls.filter(
      (c) => c.method === 'GET' && c.url.includes('role-bindings')
    ).length;
    expect(postDeleteGetCalls).toBeGreaterThan(initialGetCalls);
  });
});

/* -------------------------------------------------------------------------- */
/* 16. Behavioral: create binding dialog with locked principal                 */
/* -------------------------------------------------------------------------- */

describe('Behavioral: create binding request payload', () => {
  it('Add Binding dialog shows locked principal with lock icon', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) =>
      b.textContent?.trim().includes('Add Binding')
    ) as HTMLElement;
    expect(addBtn).toBeTruthy();
    addBtn.click();
    await tick(el, 300);

    const dialog = query(el, 'sl-dialog[open]');
    expect(dialog).toBeTruthy();

    // The locked principal display is now inside the shared assignment form's shadow DOM.
    const form = query(el, 'scion-role-binding-assignment-form') as HTMLElement;
    expect(form).toBeTruthy();
    const lockedPrincipal = form?.shadowRoot?.querySelector('.locked-principal');
    expect(lockedPrincipal).toBeTruthy();
    expect(lockedPrincipal?.textContent).toContain('user');
    expect(lockedPrincipal?.textContent).toContain('user-1');

    const lockIcon = lockedPrincipal?.querySelector('sl-icon[name="lock"]');
    expect(lockIcon).toBeTruthy();
  });
});

/* -------------------------------------------------------------------------- */
/* 17. Behavioral: error feedback on failed mutation                           */
/* -------------------------------------------------------------------------- */

describe('Behavioral: error feedback on failed mutation', () => {
  it('shows error feedback when DELETE fails', async () => {
    const calls: { url: string; method: string; body?: string }[] = [];
    const failingHandler = (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      const method = init?.method ?? 'GET';
      const body = typeof init?.body === 'string' ? init.body : undefined;
      calls.push({ url: path, method, body });

      if (path.includes('/api/v1/admin/role-bindings/b-direct') && method === 'DELETE') {
        return Promise.resolve(
          new Response(JSON.stringify({ error: 'forbidden' }), { status: 403 })
        );
      }
      if (path.includes('/api/v1/admin/role-bindings') && method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify(makeBindings()), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (path === '/api/v1/auth/admin-status') {
        return Promise.resolve(adminStatusResponse(ALL_ROLE_BINDING_PERMISSIONS));
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    };

    const el = await createEl(failingHandler);

    const trashBtn = query(el, 'sl-icon-button[name="trash"]') as HTMLElement;
    trashBtn?.click();
    await tick(el, 400);

    const feedback = query(el, '.mutation-feedback.danger');
    expect(feedback).toBeTruthy();
  });
});

/* -------------------------------------------------------------------------- */
/* 18. Behavioral: successful refresh after mutation                           */
/* -------------------------------------------------------------------------- */

describe('Behavioral: successful refresh after mutation', () => {
  it('refreshes binding list after successful delete', async () => {
    const { handler, calls } = makeFetchHandler();
    const el = await createEl(handler);

    const initialGetCalls = calls.filter(
      (c) => c.method === 'GET' && c.url.includes('role-bindings')
    ).length;

    const trashBtn = query(el, 'sl-icon-button[name="trash"]') as HTMLElement;
    trashBtn?.click();
    await tick(el, 500);

    const postDeleteGetCalls = calls.filter(
      (c) => c.method === 'GET' && c.url.includes('role-bindings')
    ).length;
    expect(postDeleteGetCalls).toBeGreaterThan(initialGetCalls);
  });
});

/* -------------------------------------------------------------------------- */
/* 19. Behavioral: no write request while rendering                           */
/* -------------------------------------------------------------------------- */

describe('Behavioral: no write request while rendering', () => {
  it('sends no POST or DELETE while rendering', async () => {
    for (const permissions of [ALL_ROLE_BINDING_PERMISSIONS, ['role_binding.read'], []]) {
      clearAdminStatus();
      const { handler, calls } = makeFetchHandler({ permissions });
      await createEl(handler);

      expect(calls.length).toBeGreaterThan(0);
      expect(calls.filter((c) => c.method !== 'GET')).toEqual([]);
      document.body.innerHTML = '';
    }
  });

  it('reads permissions from admin status', async () => {
    const { handler, calls } = makeFetchHandler();
    await createEl(handler);

    expect(calls.filter((c) => c.url === '/api/v1/auth/admin-status').length).toBe(1);
  });
});

/* -------------------------------------------------------------------------- */
/* 20. Behavioral: buttons follow the admin-status permissions array          */
/* -------------------------------------------------------------------------- */

describe('Behavioral: buttons follow the permissions array', () => {
  it('shows Add Binding with role_binding.create and no trash without role_binding.delete', async () => {
    const { handler } = makeFetchHandler({
      permissions: ['role_binding.read', 'role_binding.create'],
    });
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeTruthy();

    expect(queryAll(el, 'sl-icon-button[name="trash"]').length).toBe(0);
  });

  it('shows trash with role_binding.delete and no Add Binding without role_binding.create', async () => {
    const { handler } = makeFetchHandler({
      permissions: ['role_binding.read', 'role_binding.delete'],
    });
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeUndefined();

    expect(queryAll(el, 'sl-icon-button[name="trash"]').length).toBe(1);
  });

  it('shows both controls for a super-admin', async () => {
    const handler = (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path === '/api/v1/auth/admin-status') {
        return Promise.resolve(
          new Response(JSON.stringify({ isAdmin: true, isSuperAdmin: true, permissions: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return makeFetchHandler().handler(url, init);
    };
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    expect(addBtns.find((b) => b.textContent?.trim().includes('Add Binding'))).toBeTruthy();
    expect(queryAll(el, 'sl-icon-button[name="trash"]').length).toBe(1);
  });
});

/* -------------------------------------------------------------------------- */
/* 21. Behavioral: role-binding list follows role_binding.read                */
/* -------------------------------------------------------------------------- */

describe('Behavioral: role-binding list follows role_binding.read', () => {
  it('skips the role-binding list and renders nothing without role_binding.read', async () => {
    const { handler, calls } = makeFetchHandler({
      permissions: ['role_binding.create', 'role_binding.delete'],
    });
    const el = await createEl(handler);

    expect(listCalls(calls).length).toBe(0);
    expect(queryAll(el, '.role-card').length).toBe(0);
    expect(query(el, '.error-state')).toBeNull();
    expect(query(el, 'sl-button')).toBeNull();
  });

  it('loads the role-binding list with role_binding.read', async () => {
    const { handler, calls } = makeFetchHandler({ permissions: ['role_binding.read'] });
    const el = await createEl(handler);

    expect(listCalls(calls).length).toBe(1);
    expect(queryAll(el, '.role-card').length).toBe(2);
  });

  it('skips the role-binding list when admin status fails', async () => {
    const { handler, calls } = makeFetchHandler({ adminStatusStatus: 500 });
    const el = await createEl(handler);

    expect(listCalls(calls).length).toBe(0);
    expect(queryAll(el, '.role-card').length).toBe(0);
    expect(query(el, 'sl-button')).toBeNull();
  });
});

/* -------------------------------------------------------------------------- */
/* 22. Behavioral: boundary notice follows hub.audit.read                     */
/* -------------------------------------------------------------------------- */

describe('Behavioral: boundary notice follows hub.audit.read', () => {
  async function createNotice(permissions: string[]) {
    const calls: { url: string; method: string }[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request, init?: RequestInit): Promise<Response> => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        calls.push({ url: path, method: init?.method ?? 'GET' });
        if (path === '/api/v1/auth/admin-status') {
          return Promise.resolve(adminStatusResponse(permissions));
        }
        return Promise.resolve(
          new Response(JSON.stringify({ count: 2 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );
    const el = document.createElement('scion-effective-access-boundary-notice') as HTMLElement & {
      contextType: string;
      contextId: string;
      updateComplete: Promise<boolean>;
    };
    el.contextType = 'agent';
    el.contextId = 'agent-1';
    document.body.appendChild(el);
    await tick(el, 300);
    const effectiveAccessCalls = calls.filter((c) =>
      c.url.startsWith('/api/v1/admin/effective-access')
    );
    return { el, calls, effectiveAccessCalls };
  }

  it('skips the effective-access fetch without hub.audit.read', async () => {
    const { el, calls, effectiveAccessCalls } = await createNotice(ALL_ROLE_BINDING_PERMISSIONS);

    expect(effectiveAccessCalls.length).toBe(0);
    expect(calls.filter((c) => c.method !== 'GET')).toEqual([]);
    expect(query(el, '.notice')).toBeNull();
  });

  it('fetches effective access with hub.audit.read alone', async () => {
    const { el, effectiveAccessCalls } = await createNotice(['hub.audit.read']);

    expect(effectiveAccessCalls.length).toBe(1);
    expect(query(el, '.notice')?.textContent).toContain('2 access');
  });
});
