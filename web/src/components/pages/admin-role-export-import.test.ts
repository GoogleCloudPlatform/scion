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
 * Tests for role export/import functionality on the admin-roles page
 * and the single-role export on admin-role-detail.
 *
 * Covers:
 *   - Export: JSON envelope generation, custom-role filtering, file download trigger
 *   - Import: file parsing, validation, preview rendering, API calls, result display
 *   - Detail page: single-role export
 */

import { describe, it, expect, vi, afterEach, beforeAll, beforeEach } from 'vitest';

// ---------------------------------------------------------------------------
// Mock data
// ---------------------------------------------------------------------------

const CUSTOM_ROLE_1 = {
  id: 'role-custom-1',
  name: 'test-editor',
  description: 'A custom editor role',
  scopeType: 'system',
  permissions: ['project.read', 'project.update'],
  system: false,
  createdAt: '2026-08-01T00:00:00Z',
  updatedAt: '2026-08-15T00:00:00Z',
};

const CUSTOM_ROLE_2 = {
  id: 'role-custom-2',
  name: 'test-viewer',
  description: 'A custom viewer role',
  scopeType: 'project',
  permissions: ['project.read'],
  system: false,
  createdAt: '2026-08-02T00:00:00Z',
  updatedAt: '2026-08-16T00:00:00Z',
};

const SYSTEM_ROLE = {
  id: 'role-system-1',
  name: 'hub-admin',
  description: 'System administrator',
  scopeType: 'system',
  permissions: ['project.read', 'project.update', 'user.read'],
  system: true,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

const PERMISSIONS = [
  { ID: 'project.read', Resource: 'project', Action: 'read', Description: 'Read projects' },
  { ID: 'project.update', Resource: 'project', Action: 'update', Description: 'Update projects' },
  { ID: 'user.read', Resource: 'user', Action: 'read', Description: 'Read users' },
];

// ---------------------------------------------------------------------------
// Fetch handler
// ---------------------------------------------------------------------------

function createRolesListFetchHandler(opts?: {
  roles?: Record<string, unknown>[];
  importResponse?: Record<string, unknown>;
  importStatus?: number;
  importError?: string;
}) {
  const roles = opts?.roles ?? [CUSTOM_ROLE_1, CUSTOM_ROLE_2, SYSTEM_ROLE];

  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    // POST /api/v1/admin/roles/import (bulk import)
    if (init?.method === 'POST' && path.includes('/api/v1/admin/roles/import')) {
      const status = opts?.importStatus ?? 200;
      if (status >= 400) {
        return Promise.resolve(
          new Response(JSON.stringify({ error: opts?.importError ?? 'Bad request' }), {
            status,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (opts?.importResponse) {
        return Promise.resolve(
          new Response(JSON.stringify(opts.importResponse), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      // Default: parse the request body and create results
      const body = JSON.parse(init.body as string);
      const existingNames = new Set(roles.filter((r: any) => !r.system).map((r: any) => r.name));
      const items = (body.roles || []).map((r: any) => {
        if (existingNames.has(r.name)) {
          return { name: r.name, status: 'skipped', reason: 'role with this name and scope already exists' };
        }
        return { name: r.name, status: 'created', id: `role-new-${r.name}` };
      });
      const created = items.filter((i: any) => i.status === 'created').length;
      const skipped = items.filter((i: any) => i.status === 'skipped').length;
      return Promise.resolve(
        new Response(
          JSON.stringify({ created, skipped, errors: 0, items }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    // GET /api/v1/admin/roles/export
    if (path.includes('/api/v1/admin/roles/export')) {
      const customRoles = roles.filter((r: any) => !r.system);
      return Promise.resolve(
        new Response(
          JSON.stringify({
            version: '1',
            exportedAt: new Date().toISOString(),
            roles: customRoles.map((r: any) => ({
              name: r.name,
              description: r.description,
              scopeType: r.scopeType,
              permissions: r.permissions,
            })),
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    // GET roles
    if (path.includes('/api/v1/admin/roles')) {
      return Promise.resolve(
        new Response(JSON.stringify({ items: roles }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // GET permissions
    if (path.includes('/api/v1/admin/permissions')) {
      return Promise.resolve(
        new Response(JSON.stringify({ items: PERMISSIONS }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // Admin status
    if (path.includes('/api/v1/auth/admin-status')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({ isAdmin: true, isSuperAdmin: true, permissions: [] }),
          { status: 200 }
        )
      );
    }

    // Settings public
    if (path.includes('/api/v1/settings/public')) {
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200 }));
    }

    return Promise.resolve(new Response('{}', { status: 200 }));
  };
}

// ---------------------------------------------------------------------------
// Pre-import modules
// ---------------------------------------------------------------------------

let ScionPageAdminRolesCtor: typeof import('./admin-roles.js')['ScionPageAdminRoles'];
let ScionPageAdminRoleDetailCtor: typeof import('./admin-role-detail.js')['ScionPageAdminRoleDetail'];

beforeAll(async () => {
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))));
  const [rolesMod, detailMod] = await Promise.all([
    import('./admin-roles.js'),
    import('./admin-role-detail.js'),
  ]);
  ScionPageAdminRolesCtor = rolesMod.ScionPageAdminRoles;
  ScionPageAdminRoleDetailCtor = detailMod.ScionPageAdminRoleDetail;
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

async function createRolesPage(
  fetchHandler: ReturnType<typeof createRolesListFetchHandler>
): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = new ScionPageAdminRolesCtor();
  document.body.appendChild(el);

  const deadline = Date.now() + 4000;
  while (Date.now() < deadline) {
    await el.updateComplete;
    if (
      el.shadowRoot?.querySelector('table') ||
      el.shadowRoot?.querySelector('.empty-state') ||
      el.shadowRoot?.querySelector('.error-state')
    ) {
      break;
    }
    await new Promise((r) => setTimeout(r, 20));
  }
  await el.updateComplete;
  return el;
}

async function createRoleDetailPage(
  rolePath = '/admin/roles/role-custom-1'
): Promise<HTMLElement> {
  try {
    Object.defineProperty(window.location, 'pathname', {
      value: rolePath,
      writable: true,
      configurable: true,
    });
  } catch {
    Object.defineProperty(window, 'location', {
      value: { ...window.location, pathname: rolePath },
      writable: true,
      configurable: true,
    });
  }

  const fetchHandler = (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (path.includes('/api/v1/admin/roles/')) {
      return Promise.resolve(
        new Response(JSON.stringify(CUSTOM_ROLE_1), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }
    if (path.includes('/api/v1/admin/permissions')) {
      return Promise.resolve(
        new Response(JSON.stringify({ items: PERMISSIONS }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }
    if (path.includes('/api/v1/admin/role-bindings')) {
      return Promise.resolve(
        new Response(JSON.stringify({ items: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  };

  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = new ScionPageAdminRoleDetailCtor();
  document.body.appendChild(el);

  const deadline = Date.now() + 4000;
  while (Date.now() < deadline) {
    await el.updateComplete;
    if (el.shadowRoot?.querySelector('h1') || el.shadowRoot?.querySelector('.error-state')) {
      break;
    }
    await new Promise((r) => setTimeout(r, 20));
  }
  await el.updateComplete;
  return el;
}

/**
 * Create a mock File object from a string.
 */
function createJsonFile(content: string, name = 'roles.json'): File {
  return new File([content], name, { type: 'application/json' });
}

// ---------------------------------------------------------------------------
// Tests: Export (roles list page)
// ---------------------------------------------------------------------------

describe('admin-roles: export', () => {
  let el: HTMLElement | null = null;

  afterEach(() => {
    if (el?.parentNode) el.parentNode.removeChild(el);
    el = null;
    vi.restoreAllMocks();
  });

  it('renders Export button in the header', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    const buttons = el.shadowRoot?.querySelectorAll('.header-right sl-button');
    const labels = [...(buttons ?? [])].map((b) => b.textContent?.trim());
    expect(labels).toContain('Export');
  });

  it('renders Import button in the header', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    const buttons = el.shadowRoot?.querySelectorAll('.header-right sl-button');
    const labels = [...(buttons ?? [])].map((b) => b.textContent?.trim());
    expect(labels).toContain('Import');
  });

  it('disables Export button when there are no custom roles', async () => {
    const handler = createRolesListFetchHandler({ roles: [SYSTEM_ROLE] });
    el = await createRolesPage(handler);

    const buttons = el.shadowRoot?.querySelectorAll('.header-right sl-button');
    const exportBtn = [...(buttons ?? [])].find((b) => b.textContent?.trim() === 'Export');
    expect(exportBtn?.hasAttribute('disabled')).toBe(true);
  });

  it('triggers download with correct JSON envelope on export', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    // Capture the Blob passed to URL.createObjectURL
    let capturedBlob: Blob | null = null;
    let downloadFilename = '';
    const clickSpy = vi.fn();

    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob: Blob) => {
      capturedBlob = blob;
      return 'blob:mock-url';
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});

    const originalCreateElement = document.createElement.bind(document);
    vi.spyOn(document, 'createElement').mockImplementation((tag: string) => {
      const elem = originalCreateElement(tag);
      if (tag === 'a') {
        Object.defineProperty(elem, 'click', { value: clickSpy });
        return new Proxy(elem, {
          set(target, prop, value) {
            if (prop === 'download') downloadFilename = value;
            return Reflect.set(target, prop, value);
          },
        });
      }
      return elem;
    });

    // Click export — exportRoles() is async, wait for it
    const buttons = el.shadowRoot?.querySelectorAll('.header-right sl-button');
    const exportBtn = [...(buttons ?? [])].find((b) => b.textContent?.trim() === 'Export');
    exportBtn?.click();
    // Wait for the async export to complete
    await new Promise((r) => setTimeout(r, 50));
    await el.updateComplete;

    // Verify download was triggered
    expect(clickSpy).toHaveBeenCalled();
    expect(downloadFilename).toMatch(/^scion-roles-export-\d{4}-\d{2}-\d{2}\.json$/);

    // Verify the blob content (from backend export endpoint)
    expect(capturedBlob).not.toBeNull();
    const text = await capturedBlob!.text();
    const data = JSON.parse(text);
    expect(data.version).toBe('1');
    expect(data.exportedAt).toBeDefined();
    expect(data.roles).toHaveLength(2); // Only custom roles, not system
    expect(data.roles[0].name).toBe('test-editor');
    expect(data.roles[1].name).toBe('test-viewer');
    // Should NOT include server-generated fields
    expect(data.roles[0].id).toBeUndefined();
    expect(data.roles[0].system).toBeUndefined();
    expect(data.roles[0].createdAt).toBeUndefined();
  });

  it('shows feedback when no custom roles to export', async () => {
    const handler = createRolesListFetchHandler({ roles: [SYSTEM_ROLE] });
    el = await createRolesPage(handler);

    // Even though button is disabled, verify internal method behavior
    // by directly calling exportRoles
    await (el as any).exportRoles();
    await el.updateComplete;

    const alert = el.shadowRoot?.querySelector('.feedback-alert');
    expect(alert?.textContent).toContain('No custom roles to export');
  });
});

// ---------------------------------------------------------------------------
// Tests: Import (roles list page)
// ---------------------------------------------------------------------------

describe('admin-roles: import', () => {
  let el: HTMLElement | null = null;

  afterEach(() => {
    if (el?.parentNode) el.parentNode.removeChild(el);
    el = null;
    vi.restoreAllMocks();
  });

  it('opens import dialog when Import button is clicked', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    const buttons = el.shadowRoot?.querySelectorAll('.header-right sl-button');
    const importBtn = [...(buttons ?? [])].find((b) => b.textContent?.trim() === 'Import');
    importBtn?.click();
    await el.updateComplete;

    const dialog = el.shadowRoot?.querySelector('sl-dialog[label="Import Roles"]');
    expect(dialog).not.toBeNull();
  });

  it('shows file input in import dialog', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    (el as any).openImportDialog();
    await el.updateComplete;

    const fileInput = el.shadowRoot?.querySelector('#role-import-input');
    expect(fileInput).not.toBeNull();
    expect(fileInput?.getAttribute('type')).toBe('file');
    expect(fileInput?.getAttribute('accept')).toBe('.json,application/json');
  });

  it('parses valid envelope format JSON file', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    (el as any).openImportDialog();
    await el.updateComplete;

    const validExport = JSON.stringify({
      version: '1',
      exportedAt: '2026-09-01T00:00:00Z',
      roles: [
        { name: 'imported-role', description: 'A new role', scopeType: 'system', permissions: ['project.read'] },
      ],
    });

    const file = createJsonFile(validExport);
    const mockEvent = { target: { files: [file] } } as unknown as Event;
    await (el as any).handleImportFileSelect(mockEvent);
    await el.updateComplete;

    expect((el as any).importParsedRoles).toHaveLength(1);
    expect((el as any).importParsedRoles[0].name).toBe('imported-role');
    expect((el as any).importParseError).toBeNull();
  });

  it('parses valid plain array format JSON file', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    (el as any).openImportDialog();
    await el.updateComplete;

    const validArray = JSON.stringify([
      { name: 'role-a', description: 'Role A', scopeType: 'system', permissions: [] },
      { name: 'role-b', description: 'Role B', scopeType: 'project', permissions: ['project.read'] },
    ]);

    const file = createJsonFile(validArray);
    const mockEvent = { target: { files: [file] } } as unknown as Event;
    await (el as any).handleImportFileSelect(mockEvent);
    await el.updateComplete;

    expect((el as any).importParsedRoles).toHaveLength(2);
    expect((el as any).importParseError).toBeNull();
  });

  it('rejects invalid JSON file', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    (el as any).openImportDialog();
    await el.updateComplete;

    const file = createJsonFile('not valid json {{{');
    const mockEvent = { target: { files: [file] } } as unknown as Event;
    await (el as any).handleImportFileSelect(mockEvent);
    await el.updateComplete;

    expect((el as any).importParsedRoles).toHaveLength(0);
    expect((el as any).importParseError).toContain('Failed to parse');
  });

  it('rejects file with invalid structure (no roles array)', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    (el as any).openImportDialog();
    await el.updateComplete;

    const file = createJsonFile(JSON.stringify({ name: 'not-an-array' }));
    const mockEvent = { target: { files: [file] } } as unknown as Event;
    await (el as any).handleImportFileSelect(mockEvent);
    await el.updateComplete;

    expect((el as any).importParsedRoles).toHaveLength(0);
    expect((el as any).importParseError).toContain('Invalid format');
  });

  it('rejects file with empty roles array', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    (el as any).openImportDialog();
    await el.updateComplete;

    const file = createJsonFile(JSON.stringify({ roles: [] }));
    const mockEvent = { target: { files: [file] } } as unknown as Event;
    await (el as any).handleImportFileSelect(mockEvent);
    await el.updateComplete;

    expect((el as any).importParsedRoles).toHaveLength(0);
    expect((el as any).importParseError).toContain('no roles to import');
  });

  it('reports validation error for role entries missing name', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    (el as any).openImportDialog();
    await el.updateComplete;

    const file = createJsonFile(
      JSON.stringify({ roles: [{ description: 'no name', permissions: [] }] })
    );
    const mockEvent = { target: { files: [file] } } as unknown as Event;
    await (el as any).handleImportFileSelect(mockEvent);
    await el.updateComplete;

    expect((el as any).importParsedRoles).toHaveLength(0);
    expect((el as any).importParseError).toContain('missing or empty "name"');
  });

  it('renders preview with new/existing badges', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    (el as any).openImportDialog();
    await el.updateComplete;

    // Import a file that has one new role and one that already exists
    const file = createJsonFile(
      JSON.stringify({
        roles: [
          { name: 'brand-new-role', description: 'New', scopeType: 'system', permissions: [] },
          { name: 'test-editor', description: 'Already exists', scopeType: 'system', permissions: [] },
        ],
      })
    );
    const mockEvent = { target: { files: [file] } } as unknown as Event;
    await (el as any).handleImportFileSelect(mockEvent);
    await el.updateComplete;

    const previewItems = el.shadowRoot?.querySelectorAll('.import-preview-item');
    expect(previewItems?.length).toBe(2);

    const newBadge = el.shadowRoot?.querySelector('.import-new-badge');
    expect(newBadge?.textContent?.trim()).toBe('new');

    const skipBadge = el.shadowRoot?.querySelector('.import-skip-badge');
    expect(skipBadge?.textContent?.trim()).toContain('exists');
  });

  it('imports new roles via backend import endpoint and handles skips', async () => {
    const fetchFn = vi.fn(createRolesListFetchHandler());
    vi.stubGlobal('fetch', fetchFn);

    el = new ScionPageAdminRolesCtor();
    document.body.appendChild(el);

    const deadline = Date.now() + 4000;
    while (Date.now() < deadline) {
      await el.updateComplete;
      if (el.shadowRoot?.querySelector('table')) break;
      await new Promise((r) => setTimeout(r, 20));
    }
    await el.updateComplete;

    // Prepare import data
    const importData = [
      { name: 'brand-new-role', description: 'New', scopeType: 'system', permissions: ['project.read'] },
      { name: 'test-editor', description: 'Exists', scopeType: 'system', permissions: [] },
    ];

    (el as any).importParsedRoles = importData;
    await (el as any).importRoles();
    await el.updateComplete;

    const results = (el as any).importResults;
    expect(results).not.toBeNull();
    expect(results.created).toBe(1);
    expect(results.skipped).toBe(1);
    expect(results.errors).toHaveLength(1);
    expect(results.errors[0].name).toBe('test-editor');
    expect(results.errors[0].status).toBe('skipped');

    // Verify POST was sent to the import endpoint
    const postCalls = fetchFn.mock.calls.filter(
      (call: unknown[]) => (call[1] as RequestInit)?.method === 'POST'
    );
    expect(postCalls).toHaveLength(1);
    const postUrl = typeof postCalls[0][0] === 'string' ? postCalls[0][0] : '';
    expect(postUrl).toContain('/api/v1/admin/roles/import');
    const postBody = JSON.parse((postCalls[0][1] as RequestInit).body as string);
    expect(postBody.version).toBe('1');
    expect(postBody.roles).toHaveLength(2);
  });

  it('reports API errors from import endpoint', async () => {
    const handler = createRolesListFetchHandler({
      importStatus: 403,
      importError: 'Insufficient permissions: role.create required',
    });
    el = await createRolesPage(handler);

    const importData = [
      { name: 'new-role', description: 'New', scopeType: 'system', permissions: [] },
    ];
    (el as any).importParsedRoles = importData;
    await (el as any).importRoles();
    await el.updateComplete;

    const results = (el as any).importResults;
    expect(results.created).toBe(0);
    expect(results.errors).toHaveLength(1);
    expect(results.errors[0].status).toBe('error');
  });

  it('handles per-role errors from import endpoint', async () => {
    const handler = createRolesListFetchHandler({
      importResponse: {
        created: 0,
        skipped: 0,
        errors: 1,
        items: [
          { name: 'bad-role', status: 'error', reason: 'invalid permission IDs: foo.bar' },
        ],
      },
    });
    el = await createRolesPage(handler);

    const importData = [
      { name: 'bad-role', description: 'Bad', scopeType: 'system', permissions: ['foo.bar'] },
    ];
    (el as any).importParsedRoles = importData;
    await (el as any).importRoles();
    await el.updateComplete;

    const results = (el as any).importResults;
    expect(results.created).toBe(0);
    expect(results.errors).toHaveLength(1);
    expect(results.errors[0].name).toBe('bad-role');
    expect(results.errors[0].status).toBe('error');
    expect(results.errors[0].error).toContain('invalid permission');
  });

  it('renders results summary after import', async () => {
    const handler = createRolesListFetchHandler();
    el = await createRolesPage(handler);

    // Set up results directly
    (el as any).showImportDialog = true;
    (el as any).importResults = {
      created: 2,
      skipped: 1,
      errors: [
        { name: 'skipped-role', status: 'skipped', error: 'A role with this name already exists' },
      ],
    };
    await el.updateComplete;

    const alert = el.shadowRoot?.querySelector('.import-results sl-alert');
    expect(alert?.textContent).toContain('2 created');
    expect(alert?.textContent).toContain('1 skipped');
  });
});

// ---------------------------------------------------------------------------
// Tests: Export from role detail page
// ---------------------------------------------------------------------------

describe('admin-role-detail: single role export', () => {
  let el: HTMLElement | null = null;

  afterEach(() => {
    if (el?.parentNode) el.parentNode.removeChild(el);
    el = null;
    vi.restoreAllMocks();
  });

  it('renders Export button for custom roles', async () => {
    el = await createRoleDetailPage();

    const buttons = el.shadowRoot?.querySelectorAll('.header-actions sl-button');
    const labels = [...(buttons ?? [])].map((b) => b.textContent?.trim());
    expect(labels).toContain('Export');
  });

  it('triggers download with correct filename for single role export', async () => {
    el = await createRoleDetailPage();

    let capturedBlob: Blob | null = null;
    let downloadFilename = '';
    const clickSpy = vi.fn();

    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob: Blob) => {
      capturedBlob = blob;
      return 'blob:mock-url';
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});

    const originalCreateElement = document.createElement.bind(document);
    vi.spyOn(document, 'createElement').mockImplementation((tag: string) => {
      const elem = originalCreateElement(tag);
      if (tag === 'a') {
        Object.defineProperty(elem, 'click', { value: clickSpy });
        return new Proxy(elem, {
          set(target, prop, value) {
            if (prop === 'download') downloadFilename = value;
            return Reflect.set(target, prop, value);
          },
        });
      }
      return elem;
    });

    // Click Export button
    const buttons = el.shadowRoot?.querySelectorAll('.header-actions sl-button');
    const exportBtn = [...(buttons ?? [])].find((b) => b.textContent?.trim() === 'Export');
    exportBtn?.click();
    await el.updateComplete;

    expect(clickSpy).toHaveBeenCalled();
    expect(downloadFilename).toBe('scion-role-test-editor-export.json');

    // Verify content
    expect(capturedBlob).not.toBeNull();
    const text = await capturedBlob!.text();
    const data = JSON.parse(text);
    expect(data.version).toBe('1');
    expect(data.roles).toHaveLength(1);
    expect(data.roles[0].name).toBe('test-editor');
    expect(data.roles[0].id).toBeUndefined(); // No server fields
  });
});
