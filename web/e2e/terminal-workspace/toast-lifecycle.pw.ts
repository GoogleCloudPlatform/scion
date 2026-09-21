import { test, expect, type Page } from '@playwright/test';

// ---------------------------------------------------------------------------
// #1733 — Browser regression tests for toast lifecycle fix
//
// Verifies that the Shoelace `.toast()` lifecycle completes without
// `NotFoundError` (double-removal race) after the competing
// `sl-after-hide` listener was removed. Also checks that suppressed
// invite-stats 403s never produce a toast, and that terminal
// pane/socket/session identity remains stable during route transitions
// while a toast may be active.
// ---------------------------------------------------------------------------

const agent = '11111111-1111-4111-8111-111111111111';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Collected page errors. */
interface ErrorCollector {
  readonly errors: Error[];
}

/** Install a page.on('pageerror') collector and return the list. */
function collectPageErrors(page: Page): ErrorCollector {
  const errors: Error[] = [];
  page.on('pageerror', (err) => errors.push(err));
  return { errors };
}

/**
 * Mock the core API endpoints the app-shell and dashboard need to
 * bootstrap without network errors. This mirrors the workspace.pw.ts
 * `setup()` pattern but is focused on the dashboard page.
 */
async function setupDashboard(
  page: Page,
  overrides: {
    /** Return 403 for /api/v1/admin/invites/stats. Default true. */
    inviteStats403?: boolean;
    /** Return 403 for /api/v1/agents (unsuppressed). Default false. */
    agents403?: boolean;
  } = {}
): Promise<void> {
  const { inviteStats403 = true, agents403 = false } = overrides;

  // Feature flags
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': true };
    // Stub EventSource (SSE) — no real server
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });

  // Auth
  await page.route('**/auth/me', (route) =>
    route.fulfill({
      json: { id: 'fixture-user', email: 'fixture@example.test', role: 'admin' },
    })
  );

  // Admin status (fetched during bootstrap by main.ts)
  await page.route('**/api/v1/auth/admin-status', (route) =>
    route.fulfill({
      json: { isAdmin: true, isSuperAdmin: false, permissions: ['admin'] },
    })
  );

  // Public settings
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );

  // System status
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );

  // Agents list
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => {
    if (agents403) {
      void route.fulfill({ status: 403, json: { error: 'forbidden' } });
    } else {
      void route.fulfill({
        json: [
          {
            id: agent,
            name: 'isolated-agent',
            phase: 'running',
            projectId: 'fixture-project',
            _capabilities: { actions: ['attach'] },
          },
        ],
      });
    }
  });

  // Individual agent detail
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    void route.fulfill({
      json: {
        id: agent,
        name: 'isolated-agent',
        phase: 'running',
        projectId: 'fixture-project',
      },
    });
  });

  // Projects
  await page.route('**/api/v1/projects**', (route) => route.fulfill({ json: { projects: [] } }));

  // Invite stats — either 403 (for suppressed-toast test) or empty stats
  await page.route('**/api/v1/admin/invites/stats', (route) => {
    if (inviteStats403) {
      void route.fulfill({ status: 403, json: { error: 'forbidden' } });
    } else {
      void route.fulfill({ json: { total: 0, pending: 0, accepted: 0 } });
    }
  });
}

/**
 * Set up the terminal workspace with socket tracking, reusing the same
 * patterns as workspace.pw.ts `setup()`. Also mocks the endpoints needed
 * for dashboard bootstrap so the app-shell is fully operational when
 * the test navigates between routes.
 */
async function setupTerminal(page: Page): Promise<{
  readonly attaches: number;
  readonly closes: number;
  sendToSocket(index: number, data: string): void;
  sent: string[];
}> {
  let attaches = 0;
  let closes = 0;
  const sent: string[] = [];
  const sockets: Array<{
    close: (options?: { code?: number; reason?: string }) => void;
    send: (data: string | Buffer) => void;
  }> = [];

  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': true };
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });

  // Auth
  await page.route('**/auth/me', (route) =>
    route.fulfill({
      json: { id: 'fixture-user', email: 'fixture@example.test', role: 'admin' },
    })
  );

  // Admin status (fetched during bootstrap by main.ts)
  await page.route('**/api/v1/auth/admin-status', (route) =>
    route.fulfill({
      json: { isAdmin: true, isSuperAdmin: false, permissions: ['admin'] },
    })
  );

  // Public settings
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );

  // System status
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );

  // Agents list
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => {
    void route.fulfill({
      json: [
        {
          id: agent,
          name: 'isolated-agent',
          phase: 'running',
          projectId: 'fixture-project',
          _capabilities: { actions: ['attach'] },
        },
      ],
    });
  });

  // Individual agent detail
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    void route.fulfill({
      json: {
        id: agent,
        name: 'isolated-agent',
        phase: 'running',
        projectId: 'fixture-project',
      },
    });
  });

  // Projects
  await page.route('**/api/v1/projects**', (route) => route.fulfill({ json: { projects: [] } }));

  // Invite stats — OK (no toast from here)
  await page.route('**/api/v1/admin/invites/stats', (route) =>
    route.fulfill({ json: { total: 0, pending: 0, accepted: 0 } })
  );

  // WebSocket mock for PTY
  await page.routeWebSocket('**/pty?*', (socket) => {
    attaches++;
    sockets.push(socket);
    socket.onMessage((message) => sent.push(String(message)));
    socket.onClose(() => closes++);
  });

  return {
    get attaches(): number {
      return attaches;
    },
    get closes(): number {
      return closes;
    },
    sendToSocket(index: number, data: string): void {
      if (sockets[index]) sockets[index].send(data);
    },
    sent,
  };
}

/**
 * Dispatch a `scion:access-denied` event on the page's window, which
 * triggers the app-shell listener → `showAccessDeniedToast()` →
 * real Shoelace `.toast()`. This is Approach B from the spec.
 */
async function triggerAccessDeniedToast(page: Page): Promise<void> {
  await page.evaluate(() => {
    window.dispatchEvent(
      new CustomEvent('scion:access-denied', {
        detail: {
          action: 'test-action',
          resource: 'test-resource',
          reason: 'Test access denied',
        },
      })
    );
  });
}

/**
 * Query the number of `sl-alert` elements in Shoelace's toast stack.
 */
async function toastStackCount(page: Page): Promise<number> {
  return page.evaluate(() => document.querySelectorAll('.sl-toast-stack sl-alert').length);
}

/**
 * Wait for any toast in the Shoelace toast stack to become visible.
 */
async function waitForToastVisible(page: Page): Promise<void> {
  await expect.poll(() => toastStackCount(page), { timeout: 5000 }).toBeGreaterThanOrEqual(1);
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

test('dashboard invite-stats 403 produces no toast when suppressed', async ({ page }) => {
  const collector = collectPageErrors(page);

  // Set up a listener for scion:access-denied to track whether it fires
  await page.addInitScript(() => {
    const w = window as typeof window & {
      __accessDeniedEvents?: Array<{ action?: string; resource?: string }>;
    };
    w.__accessDeniedEvents = [];
    window.addEventListener('scion:access-denied', ((
      e: CustomEvent<{ action?: string; resource?: string }>
    ) => {
      w.__accessDeniedEvents!.push(e.detail ?? {});
    }) as EventListener);
  });

  await setupDashboard(page, { inviteStats403: true });
  await page.goto('/');

  // Wait long enough for any toast to appear (the invite-stats call
  // returns 403 but uses suppressAccessDeniedToast, so no toast should fire)
  await page.waitForTimeout(1000);

  // No sl-alert in the toast stack
  expect(await toastStackCount(page)).toBe(0);

  // No access-denied events were dispatched at all (suppressed at apiFetch)
  const events = await page.evaluate(
    () =>
      (
        window as typeof window & {
          __accessDeniedEvents?: Array<{ action?: string; resource?: string }>;
        }
      ).__accessDeniedEvents ?? []
  );
  expect(events.length).toBe(0);

  // No page errors
  expect(collector.errors).toHaveLength(0);
});

test('unsuppressed toast lifecycle — no double-removal, no pageerror', async ({ page }) => {
  // Toast auto-dismiss is 6000ms (warning variant) + animation time
  test.setTimeout(20000);

  const collector = collectPageErrors(page);

  await setupDashboard(page, { inviteStats403: false });
  await page.goto('/');

  // Wait for the dashboard to render
  await page.waitForTimeout(500);

  // Trigger a real toast via the access-denied event path (Approach B)
  await triggerAccessDeniedToast(page);

  // Toast should appear in Shoelace's toast stack
  await waitForToastVisible(page);

  // Verify exactly one toast appeared
  expect(await toastStackCount(page)).toBe(1);

  // Verify the toast contains the expected text
  const toastText = await page.evaluate(() => {
    const alert = document.querySelector('.sl-toast-stack sl-alert');
    return alert?.textContent?.trim() ?? '';
  });
  expect(toastText).toContain('Test access denied');

  // Wait for auto-dismiss: 6000ms duration + 2000ms buffer for animation
  await page.waitForTimeout(8000);

  // After dismiss, no sl-alert remains in the toast stack
  await expect.poll(() => toastStackCount(page), { timeout: 5000 }).toBe(0);

  // The critical assertion: no NotFoundError during the entire lifecycle.
  // The fix for #1733 removed the competing sl-after-hide listener that
  // caused a double-removal race resulting in NotFoundError.
  const notFoundErrors = collector.errors.filter((e) => e.message.includes('NotFoundError'));
  expect(notFoundErrors).toHaveLength(0);
});

test('route transition during active toast — no pageerror', async ({ page }) => {
  test.setTimeout(20000);

  const collector = collectPageErrors(page);
  const socket = await setupTerminal(page);

  // Start on the dashboard to bootstrap the app-shell (which registers
  // the scion:access-denied listener). Navigating to /terminals/ first
  // would skip shell creation.
  await page.goto('/');
  await page.waitForTimeout(500);

  // Trigger a toast while on the dashboard
  await triggerAccessDeniedToast(page);
  await waitForToastVisible(page);

  // While the toast is still visible, navigate to a terminal
  await page.evaluate(
    (path) =>
      document.dispatchEvent(new CustomEvent('nav-click', { detail: { path }, bubbles: true })),
    `/terminals/${agent}`
  );

  // Terminal workspace should appear
  await expect(page.locator('#terminal-workspace')).toHaveCount(1);
  await expect.poll(() => socket.attaches).toBe(1);

  // Wait through the full toast dismiss/animation cycle
  await page.waitForTimeout(8000);

  // After dismiss, no toast residue
  await expect.poll(() => toastStackCount(page), { timeout: 5000 }).toBe(0);

  // No NotFoundError from double-removal during route transition
  const notFoundErrors = collector.errors.filter((e) => e.message.includes('NotFoundError'));
  expect(notFoundErrors).toHaveLength(0);

  // Socket attaches is exactly 1 — no recreation
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);
});

test('terminal pane identity stable across toast + route transition', async ({ page }) => {
  test.setTimeout(20000);

  const collector = collectPageErrors(page);
  const socket = await setupTerminal(page);

  // Navigate to dashboard first to bootstrap the app-shell (which registers
  // the scion:access-denied listener needed for toast triggering).
  await page.goto('/');
  await page.waitForTimeout(500);

  // Now navigate to the terminal workspace
  await page.evaluate(
    (path) =>
      document.dispatchEvent(new CustomEvent('nav-click', { detail: { path }, bubbles: true })),
    `/terminals/${agent}`
  );
  await expect(page.locator('#terminal-workspace')).toHaveCount(1);
  await expect.poll(() => socket.attaches).toBe(1);

  // Record DOM identity of the terminal pane
  const initialIdentity = await page.evaluate(() => {
    const host = document.querySelector('#terminal-workspace')!;
    const pane = host.querySelector('scion-terminal-pane')!;
    const memo = window as typeof window & {
      __toastTestIdentity?: { host: Element; pane: Element };
    };
    memo.__toastTestIdentity = { host, pane };
    return { hasHost: !!host, hasPane: !!pane };
  });
  expect(initialIdentity.hasHost).toBe(true);
  expect(initialIdentity.hasPane).toBe(true);

  // Trigger a toast (app-shell listener is active from initial dashboard load)
  await triggerAccessDeniedToast(page);
  await waitForToastVisible(page);

  // Navigate away from terminals to dashboard
  await page.evaluate(() =>
    document.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path: '/' },
        bubbles: true,
      })
    )
  );
  await expect(page).toHaveURL('/');

  // Navigate back to terminals
  await page.evaluate(
    (path) =>
      document.dispatchEvent(new CustomEvent('nav-click', { detail: { path }, bubbles: true })),
    `/terminals/${agent}`
  );

  // Wait through the toast dismiss cycle
  await page.waitForTimeout(8000);

  // Verify pane/socket identity is unchanged — same DOM elements
  const identityCheck = await page.evaluate(() => {
    const host = document.querySelector('#terminal-workspace')!;
    const pane = host.querySelector('scion-terminal-pane')!;
    const memo = window as typeof window & {
      __toastTestIdentity?: { host: Element; pane: Element };
    };
    return {
      hostSame: memo.__toastTestIdentity?.host === host,
      paneSame: memo.__toastTestIdentity?.pane === pane,
      paneCount: host.querySelectorAll('scion-terminal-pane').length,
    };
  });

  expect(identityCheck.hostSame).toBe(true);
  expect(identityCheck.paneSame).toBe(true);
  expect(identityCheck.paneCount).toBe(1);

  // Socket was never torn down or recreated
  expect(socket.attaches).toBe(1);
  expect(socket.closes).toBe(0);

  // No toast residue
  await expect.poll(() => toastStackCount(page), { timeout: 5000 }).toBe(0);

  // No NotFoundError from the toast lifecycle
  const notFoundErrors = collector.errors.filter((e) => e.message.includes('NotFoundError'));
  expect(notFoundErrors).toHaveLength(0);
});
