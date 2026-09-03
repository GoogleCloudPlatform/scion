import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// ── Mock API responses ──

const MOCK_ROLES = {
  items: [
    { id: 'role-1', name: 'alpha-role', scopeType: 'system', system: false },
    { id: 'role-2', name: 'beta-role', scopeType: 'project', system: false },
  ],
};

function makeBindings(count = 3) {
  const items = [];
  for (let i = 0; i < count; i++) {
    items.push({
      id: `binding-${i}`,
      roleDefinitionId: `role-${(i % 2) + 1}`,
      principalType: 'user',
      principalId: `user-${String.fromCharCode(65 + i).toLowerCase()}`,
      scopeType: i % 2 === 0 ? 'system' : 'project',
      scopeId: i % 2 === 0 ? '' : `proj-${i}`,
      createdBy: 'admin',
      createdAt: new Date(2026, 0, i + 1).toISOString(),
    });
  }
  return { items, totalCount: count };
}

function makeFetchHandler() {
  const calls: string[] = [];
  const handler = (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    calls.push(path);

    if (path.includes('/api/v1/admin/role-bindings')) {
      return Promise.resolve(
        new Response(JSON.stringify(makeBindings()), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }
    if (path.includes('/api/v1/admin/roles')) {
      return Promise.resolve(
        new Response(JSON.stringify(MOCK_ROLES), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  };
  return { handler, calls };
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let Component: any;

async function createComponent(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
) {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-admin-role-bindings') as InstanceType<
    typeof Component
  >;
  document.body.appendChild(el);
  await el.updateComplete;
  // Allow async loadData to complete.
  await new Promise((resolve) => setTimeout(resolve, 200));
  await el.updateComplete;
  return el;
}

function queryAll(el: HTMLElement, selector: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(selector) ?? []);
}

function query(el: HTMLElement, selector: string): Element | null {
  return el.shadowRoot?.querySelector(selector) ?? null;
}

// ── Tests ──

beforeAll(async () => {
  const mod = await import('./admin-role-bindings.js');
  Component = mod.ScionPageAdminRoleBindings;
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

describe('scion-page-admin-role-bindings sorting', () => {
  it('renders sortable column headers with correct classes', async () => {
    const { handler } = makeFetchHandler();
    const el = await createComponent(handler);

    const sortableHeaders = queryAll(el, 'th.sortable');
    expect(sortableHeaders.length).toBe(3); // Principal, Role, Created

    const headerTexts = sortableHeaders.map((h) => h.textContent?.trim());
    expect(headerTexts[0]).toContain('Principal');
    expect(headerTexts[1]).toContain('Role');
    expect(headerTexts[2]).toContain('Created');
  });

  it('marks the default sort column (created) as active', async () => {
    const { handler } = makeFetchHandler();
    const el = await createComponent(handler);

    const activeHeaders = queryAll(el, 'th.sortable.active');
    expect(activeHeaders.length).toBe(1);
    expect(activeHeaders[0].textContent).toContain('Created');
  });

  it('shows descending indicator on default sort column', async () => {
    const { handler } = makeFetchHandler();
    const el = await createComponent(handler);

    const activeHeader = query(el, 'th.sortable.active');
    expect(activeHeader?.textContent).toContain('▼');
  });

  it('passes sort_by and sort_order params in the initial API call', async () => {
    const { handler, calls } = makeFetchHandler();
    await createComponent(handler);

    const bindingsCall = calls.find((c) => c.includes('/api/v1/admin/role-bindings'));
    expect(bindingsCall).toBeDefined();
    expect(bindingsCall).toContain('sort_by=created');
    expect(bindingsCall).toContain('sort_order=desc');
  });

  it('toggles sort direction when clicking the active column', async () => {
    const { handler, calls } = makeFetchHandler();
    const el = await createComponent(handler);

    // Click the Created header (already active with desc).
    const createdHeader = queryAll(el, 'th.sortable')[2] as HTMLElement;
    createdHeader.click();
    await new Promise((resolve) => setTimeout(resolve, 200));
    await el.updateComplete;

    // After click, should request created asc.
    const lastBindingsCall = calls
      .filter((c) => c.includes('/api/v1/admin/role-bindings'))
      .pop();
    expect(lastBindingsCall).toContain('sort_by=created');
    expect(lastBindingsCall).toContain('sort_order=asc');

    // Header should now show ascending indicator.
    const updatedHeader = query(el, 'th.sortable.active');
    expect(updatedHeader?.textContent).toContain('▲');
  });

  it('switches sort field when clicking a different column', async () => {
    const { handler, calls } = makeFetchHandler();
    const el = await createComponent(handler);

    // Click the Principal header.
    const principalHeader = queryAll(el, 'th.sortable')[0] as HTMLElement;
    principalHeader.click();
    await new Promise((resolve) => setTimeout(resolve, 200));
    await el.updateComplete;

    const lastBindingsCall = calls
      .filter((c) => c.includes('/api/v1/admin/role-bindings'))
      .pop();
    expect(lastBindingsCall).toContain('sort_by=principal');
    expect(lastBindingsCall).toContain('sort_order=asc'); // default for non-created

    // Principal header should now be active.
    const activeHeaders = queryAll(el, 'th.sortable.active');
    expect(activeHeaders.length).toBe(1);
    expect(activeHeaders[0].textContent).toContain('Principal');
  });

  it('resets to page 1 when sort changes', async () => {
    const { handler, calls } = makeFetchHandler();
    const el = await createComponent(handler);

    // Click the Role header.
    const roleHeader = queryAll(el, 'th.sortable')[1] as HTMLElement;
    roleHeader.click();
    await new Promise((resolve) => setTimeout(resolve, 200));
    await el.updateComplete;

    const lastBindingsCall = calls
      .filter((c) => c.includes('/api/v1/admin/role-bindings'))
      .pop();
    expect(lastBindingsCall).toContain('offset=0');
  });

  it('non-sortable columns (Scope, Scope ID, Actions) have no sortable class', async () => {
    const { handler } = makeFetchHandler();
    const el = await createComponent(handler);

    const allHeaders = queryAll(el, 'th');
    const nonSortableHeaders = allHeaders.filter((h) => !h.classList.contains('sortable'));
    const nonSortableTexts = nonSortableHeaders.map((h) => h.textContent?.trim());
    expect(nonSortableTexts).toContain('Scope');
    expect(nonSortableTexts).toContain('Actions');
  });
});
