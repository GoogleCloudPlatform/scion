/**
 * Tests for D2: Project settings — cross-project messaging policy section.
 *
 * Verifies the messaging policy UI: three inbound options (none/members/any),
 * owner-only editing, Hub-disabled state, and CAS-based save with conflict handling.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionPageProjectSettings: any;

const PROJECT_RESPONSE = {
  id: 'proj-1',
  name: 'Test Project',
  slug: 'test-project',
  _capabilities: { actions: ['update', 'manage'] },
};

const MESSAGING_POLICY_RESPONSE = {
  crossProjectInbound: 'none' as const,
  revision: 3,
  effectiveCrossProjectInbound: 'none' as const,
  hubCrossProjectEnabled: true,
};

function createFetchHandler(opts?: {
  project?: Record<string, unknown>;
  messagingPolicy?: Record<string, unknown> | null;
  putHandler?: (body: Record<string, unknown>) => {
    status: number;
    body: Record<string, unknown>;
  };
}) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (init?.method === 'PUT' && path.includes('/messaging-policy') && opts?.putHandler) {
      const reqBody = JSON.parse(init.body as string);
      const result = opts.putHandler(reqBody);
      return Promise.resolve(
        new Response(JSON.stringify(result.body), {
          status: result.status,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/messaging-policy')) {
      if (opts?.messagingPolicy === null) {
        return Promise.resolve(new Response('', { status: 404 }));
      }
      return Promise.resolve(
        new Response(
          JSON.stringify(opts?.messagingPolicy ?? MESSAGING_POLICY_RESPONSE),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    if (path.match(/\/api\/v1\/projects\/[^/]+$/) || path.match(/\/api\/v1\/projects\/[^/]+\?/)) {
      return Promise.resolve(
        new Response(JSON.stringify(opts?.project ?? PROJECT_RESPONSE), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/settings/resolved')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            projectId: 'proj-1',
            settings: {},
            resolvedSettings: {},
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    if (path.includes('/settings/public')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({}),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    if (path.includes('/templates')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({ templates: [], page: 1, pageSize: 100, total: 0 }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    // Catch-all for other API calls (harness configs, brokers, etc.)
    return Promise.resolve(
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

async function createComponent(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
) {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-project-settings') as InstanceType<
    typeof ScionPageProjectSettings
  >;
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 400));
  await el.updateComplete;
  return el;
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

describe('project-settings: messaging policy section (D2)', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler()));
    const mod = await import('./project-settings.js');
    ScionPageProjectSettings = mod.ScionPageProjectSettings;
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('renders the Cross-Project Messaging section', async () => {
    element = await createComponent(createFetchHandler());
    const text = shadowText(element);
    expect(text).toContain('Cross-Project Messaging');
  });

  it('calls the messaging-policy API on connect', async () => {
    element = await createComponent(createFetchHandler());
    expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      expect.stringContaining('/messaging-policy'),
      expect.any(Object)
    );
  });

  it('renders three radio options: none, members, any', async () => {
    element = await createComponent(createFetchHandler());
    const text = shadowText(element);
    expect(text).toContain('No external agents');
    expect(text).toContain('members');
    expect(text).toContain('any project on this Hub');
  });

  it('shows Hub-disabled warning when hubCrossProjectEnabled is false', async () => {
    element = await createComponent(
      createFetchHandler({
        messagingPolicy: {
          crossProjectInbound: 'members',
          revision: 2,
          effectiveCrossProjectInbound: 'none',
          hubCrossProjectEnabled: false,
        },
      })
    );

    const text = shadowText(element);
    expect(text).toContain('Disabled by Hub administrator');
  });

  it('does not show Hub-disabled warning when hubCrossProjectEnabled is true', async () => {
    element = await createComponent(
      createFetchHandler({
        messagingPolicy: {
          crossProjectInbound: 'members',
          revision: 2,
          effectiveCrossProjectInbound: 'members',
          hubCrossProjectEnabled: true,
        },
      })
    );

    const text = shadowText(element);
    expect(text).not.toContain('Disabled by Hub administrator');
  });

  it('shows read-only notice for non-owner users', async () => {
    element = await createComponent(
      createFetchHandler({
        project: {
          ...PROJECT_RESPONSE,
          _capabilities: { actions: ['update'] },
        },
      })
    );

    const text = shadowText(element);
    expect(text).toContain('Only project owners');
  });

  it('sends CAS revision on policy save', async () => {
    let capturedPayload: Record<string, unknown> | null = null;
    element = await createComponent(
      createFetchHandler({
        messagingPolicy: {
          crossProjectInbound: 'none',
          revision: 5,
          effectiveCrossProjectInbound: 'none',
          hubCrossProjectEnabled: true,
        },
        putHandler: (body) => {
          capturedPayload = body;
          return {
            status: 200,
            body: {
              crossProjectInbound: 'members',
              revision: 6,
              effectiveCrossProjectInbound: 'members',
              hubCrossProjectEnabled: true,
            },
          };
        },
      })
    );

    // Wait for load
    await new Promise((resolve) => setTimeout(resolve, 300));
    await (element as any).updateComplete;

    // Call save directly
    await (element as any).saveMessagingPolicy('members');
    await (element as any).updateComplete;

    expect(capturedPayload).not.toBeNull();
    expect(capturedPayload!.expectedRevision).toBe(5);
    expect(capturedPayload!.crossProjectInbound).toBe('members');
  });

  it('handles 409 conflict by showing error and reloading', async () => {
    element = await createComponent(
      createFetchHandler({
        messagingPolicy: {
          crossProjectInbound: 'none',
          revision: 5,
          effectiveCrossProjectInbound: 'none',
          hubCrossProjectEnabled: true,
        },
        putHandler: () => ({
          status: 409,
          body: { error: 'conflict' },
        }),
      })
    );

    // Wait for all API calls to settle
    await new Promise((resolve) => setTimeout(resolve, 500));
    await (element as any).updateComplete;

    // Verify the messaging policy loaded before save
    expect((element as any).messagingPolicy).not.toBeNull();

    await (element as any).saveMessagingPolicy('any');
    await new Promise((resolve) => setTimeout(resolve, 500));
    await (element as any).updateComplete;

    const err = (element as any).messagingPolicyError;
    expect(err).toBeTruthy();
    expect(err).toContain('modified');
  });

  it('shows inbound policy explanation text', async () => {
    element = await createComponent(createFetchHandler());
    const text = shadowText(element);
    expect(text).toContain('inbound policy');
  });
});
