/**
 * Playwright browser tests for per-user terminal list persistence.
 *
 * There is no hub in this harness, so the new route
 * (`/api/v1/users/me/terminal-workspace`) is faked in-memory, held in the
 * test process rather than in any one page or browser context. It is
 * installed on each context with context.route(), so the same fake state
 * survives a context close and is visible to a later, separate context —
 * standing in for the real hub's per-user row persisting across devices.
 *
 * Hub-side pruning (missing, soft-deleted, access-denied, error-derived) is
 * covered by the Go tests, not here. These tests only exercise what the
 * client does with a GET/PUT response shaped like the hub's.
 */
import { test, expect, type Page, type BrowserContext } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

// Every case does at least one cold page.goto() plus real PTY attaches,
// and some do two page loads or three attaches. Under CPU contention that
// exceeds the config's 30s test timeout, so allow 90s.
test.describe.configure({ timeout: 90_000 });

const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';
const agentC = '33333333-3333-4333-8333-333333333333';
const agentD = '44444444-4444-4444-8444-444444444444';

interface AgentFixture {
  id: string;
  name: string;
  phase: string;
  projectId: string;
}

const ALL_AGENTS: Record<string, AgentFixture> = {
  [agentA]: { id: agentA, name: 'alpha', phase: 'running', projectId: 'proj' },
  [agentB]: { id: agentB, name: 'beta', phase: 'running', projectId: 'proj' },
  [agentC]: { id: agentC, name: 'gamma', phase: 'running', projectId: 'proj' },
  [agentD]: { id: agentD, name: 'delta', phase: 'running', projectId: 'proj' },
};

interface WorkspacePut {
  agentIds: string[];
  frontmostAgentId: string | null;
}

/**
 * The in-memory stand-in for the hub's per-user row. Lives in the test
 * process (a plain object created once per test), not in any page or
 * context, so multiple browser contexts within one test share it — the
 * same way a real user's row is shared across browsers and devices.
 *
 * waitForState(predicate) resolves the instant `predicate()` is true,
 * checked immediately and again every time a PUT lands (a real network
 * event observed from inside the route handler below) — an event, not a
 * fixed-interval poll. It replaces every expect.poll() this fake used to
 * need on `puts`, `storedAgentIds` or `storedFrontmostAgentId`.
 */
interface WorkspaceFakeState {
  storedAgentIds: string[];
  storedFrontmostAgentId: string | null;
  revision: number;
  /** IDs the next GET (and every GET after it, until changed) should
   *  report as pruned: present in storedAgentIds but left out of the GET
   *  response, with pruned counting them. Mirrors the hub's "prune on read,
   *  no side effect" rule: storedAgentIds is not modified by a GET, only by
   *  a PUT. */
  prunedIds: ReadonlySet<string>;
  puts: WorkspacePut[];
  waitForState(predicate: () => boolean): Promise<void>;
  /** Internal: called by installWorkspaceFake after every PUT. Not meant to
   *  be called from test bodies. */
  notifyStateWaiters(): void;
}

function createWorkspaceFake(initial?: {
  agentIds?: string[];
  frontmostAgentId?: string | null;
  prunedIds?: ReadonlySet<string>;
}): WorkspaceFakeState {
  const waiters: Array<{ predicate: () => boolean; resolve: () => void }> = [];
  const state: WorkspaceFakeState = {
    storedAgentIds: initial?.agentIds ?? [],
    storedFrontmostAgentId: initial?.frontmostAgentId ?? null,
    revision: 0,
    prunedIds: initial?.prunedIds ?? new Set(),
    puts: [],
    waitForState(predicate) {
      if (predicate()) return Promise.resolve();
      return new Promise((resolve) => waiters.push({ predicate, resolve }));
    },
    notifyStateWaiters() {
      for (const waiter of [...waiters]) {
        if (!waiter.predicate()) continue;
        waiters.splice(waiters.indexOf(waiter), 1);
        waiter.resolve();
      }
    },
  };
  return state;
}

async function installWorkspaceFake(
  context: BrowserContext,
  state: WorkspaceFakeState
): Promise<void> {
  await context.route('**/api/v1/users/me/terminal-workspace', (route) => {
    const req = route.request();
    if (req.method() === 'GET') {
      const survivors = state.storedAgentIds.filter((id) => !state.prunedIds.has(id));
      const frontmostAgentId =
        state.storedFrontmostAgentId && !state.prunedIds.has(state.storedFrontmostAgentId)
          ? state.storedFrontmostAgentId
          : null;
      void route.fulfill({
        json: {
          agentIds: survivors,
          frontmostAgentId,
          revision: state.revision,
          updatedAt: state.revision > 0 ? new Date().toISOString() : null,
          pruned: state.storedAgentIds.length - survivors.length,
        },
      });
      return;
    }
    if (req.method() === 'PUT') {
      const body = req.postDataJSON() as WorkspacePut;
      state.storedAgentIds = body.agentIds;
      state.storedFrontmostAgentId = body.frontmostAgentId;
      state.revision++;
      state.puts.push(body);
      void route.fulfill({
        json: {
          agentIds: state.storedAgentIds,
          frontmostAgentId: state.storedFrontmostAgentId,
          revision: state.revision,
          updatedAt: new Date().toISOString(),
          pruned: 0,
        },
      });
      state.notifyStateWaiters();
      return;
    }
    void route.fulfill({ status: 405, json: {} });
  });
}

/** Stubs auth, agents, pty and system-status — the same fixture shape as
 *  url-layout.pw.ts and ownership.pw.ts. Returns a live attach counter and
 *  waitForAttach(n), which resolves the instant the nth PTY socket opens
 *  (an event, not a poll) — deterministic regardless of how long the page's
 *  own bootstrap (bundle load, auth, coordinator, restore) takes under CPU
 *  contention, unlike a fixed-timeout expect.poll on the counter. */
async function setupAgents(
  page: Page,
  agents: Record<string, AgentFixture> = ALL_AGENTS
): Promise<{
  readonly attaches: number;
  attachedAgentIds: string[];
  waitForAttach(count: number): Promise<void>;
}> {
  let attaches = 0;
  const attachedAgentIds: string[] = [];
  const waiters: Array<{ count: number; resolve: () => void }> = [];
  const notifyWaiters = (): void => {
    for (const waiter of [...waiters]) {
      if (attaches < waiter.count) continue;
      waiters.splice(waiters.indexOf(waiter), 1);
      waiter.resolve();
    }
  };
  await installEventSourceStub(page);
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: 'fixture-user', email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => {
    void route.fulfill({
      json: Object.values(agents).map((a) => ({ ...a, _capabilities: { actions: ['attach'] } })),
    });
  });
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    const id =
      route
        .request()
        .url()
        .match(/\/api\/v1\/agents\/([^/?]+)/)?.[1] ?? agentA;
    void route.fulfill({
      status: agents[id] ? 200 : 404,
      json: agents[id] ?? { error: 'not found' },
    });
  });
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.routeWebSocket('**/pty?*', (socket) => {
    attaches++;
    const id = socket.url().match(/\/api\/v1\/agents\/([^/]+)\/pty/)?.[1];
    if (id) attachedAgentIds.push(id);
    notifyWaiters();
  });
  return {
    get attaches(): number {
      return attaches;
    },
    attachedAgentIds,
    waitForAttach(count: number): Promise<void> {
      if (attaches >= count) return Promise.resolve();
      return new Promise((resolve) => waiters.push({ count, resolve }));
    },
  };
}

async function navigateToTerminal(page: Page, agentId: string): Promise<void> {
  await page.evaluate(
    (path) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path } })),
    `/terminals/${agentId}`
  );
}

/** Rail order, as agent names (this file's fixture names, e.g. "alpha"). A
 *  Locator, not resolved text: callers assert on it with toHaveText(),
 *  Playwright's own auto-retrying web-first assertion, rather than
 *  re-evaluating this in a manual expect.poll(). */
function railNameEntries(page: Page): ReturnType<Page['locator']> {
  return page.locator('#terminal-workspace .terminal-rail-item .terminal-agent-name');
}

/** How long to give the first rendered-rail check after a cold page.goto():
 *  under this container's CPU contention, full bootstrap (bundle load,
 *  auth, coordinator claim, restore GET, merge, render) can occasionally
 *  take longer than Playwright's 5s assertion default. Later checks in the
 *  same page, after it is already warm, keep that default. */
const FIRST_RENDER_TIMEOUT = 15_000;

async function clickRailEntry(page: Page, name: string): Promise<void> {
  await page.locator(`[aria-label^="Show terminal for ${name} "]`).click();
}

/** This file's fixture names for agent IDs, for failure messages. */
function agentNames(ids: readonly string[]): string {
  return `[${ids.map((id) => ALL_AGENTS[id]?.name ?? id).join(', ')}]`;
}

/** The PUT bodies, with fixture names, for failure messages. */
function describePuts(puts: readonly WorkspacePut[]): string {
  if (puts.length === 0) return 'none';
  return puts
    .map(
      (put) =>
        `${agentNames(put.agentIds)} frontmost ${put.frontmostAgentId ? agentNames([put.frontmostAgentId]) : 'none'}`
    )
    .join('; ');
}

/** How long one bounded wait in a test body may take before it fails with
 *  its own message. Well inside the file's 90s test timeout, so a missing
 *  attach or PUT fails the test naming what is missing, instead of letting
 *  the test time out with no message. */
const EVENT_WAIT_TIMEOUT = 20_000;

/**
 * Resolves with `promise`, or fails with `message()` if it has not resolved
 * within `timeoutMs`. A deadline on an event wait (waitForAttach,
 * waitForState), not a fixed sleep: it resolves the instant the event does.
 * The message is built at the deadline, so it reports the state then.
 */
async function within<T>(
  promise: Promise<T>,
  timeoutMs: number,
  message: () => string
): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const deadline = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${message()} (after ${timeoutMs}ms)`)), timeoutMs);
  });
  try {
    return await Promise.race([promise, deadline]);
  } finally {
    clearTimeout(timer);
  }
}

// ===========================================================================
// Case 1: round trip across two browser contexts
// ===========================================================================
test('round trip: entries saved by one context are restored, in order, by another; only the frontmost attaches', async ({
  browser,
}) => {
  // This test does more real work than the others (two full browser
  // contexts, three sequential terminal opens plus a context teardown,
  // then a fresh context restoring from the shared fake) — see the
  // file-level test.describe.configure(timeout) above. Attach counts and
  // the fake's saved state are waited on as events, not polls
  // (waitForAttach, waitForState, below); the rendered-rail checks are
  // Playwright's own auto-retrying assertions, given a longer explicit
  // timeout on the first one after each cold page.goto() (FIRST_RENDER_TIMEOUT).
  const state = createWorkspaceFake();

  const context1 = await browser.newContext();
  const page1 = await context1.newPage();
  await installWorkspaceFake(context1, state);
  const socket1 = await setupAgents(page1);

  // Open three terminals in the owner tab: alpha, then beta, then gamma.
  // Each becomes frontmost in turn, so the final saved frontmost is gamma.
  await page1.goto(`/terminals/${agentA}`);
  await socket1.waitForAttach(1);
  await navigateToTerminal(page1, agentB);
  await socket1.waitForAttach(2);
  await navigateToTerminal(page1, agentC);
  await socket1.waitForAttach(3);

  // Wait for the saved state to reach its final shape. The exact number of
  // PUTs along the way depends on real-time debounce coalescing between the
  // three navigations above (the debounce/baseline behaviour itself is
  // covered precisely by vitest's fake timers); what matters here is that
  // it converges to the live rail. waitForState re-checks this predicate
  // every time a PUT lands, so it resolves the instant it becomes true
  // instead of sampling on an interval.
  await state.waitForState(
    () =>
      state.storedFrontmostAgentId === agentC &&
      state.storedAgentIds.length === 3 &&
      state.storedAgentIds.every((id, i) => id === [agentA, agentB, agentC][i])
  );

  await context1.close();

  // A second, unrelated context restores the same list.
  const context2 = await browser.newContext();
  const page2 = await context2.newPage();
  await installWorkspaceFake(context2, state);
  const socket2 = await setupAgents(page2);

  await page2.goto('/terminals');
  await expect(railNameEntries(page2)).toHaveText(['alpha', 'beta', 'gamma'], {
    timeout: FIRST_RENDER_TIMEOUT,
  });
  // Only the frontmost (gamma) auto-connects.
  await socket2.waitForAttach(1);
  expect(socket2.attachedAgentIds).toEqual([agentC]);
  await expect(page2).toHaveURL(`/terminals/${agentC}`);

  // Selecting an idle entry (alpha) connects it — a second attach.
  await clickRailEntry(page2, 'alpha');
  await socket2.waitForAttach(2);

  await context2.close();
});

// ===========================================================================
// Case 2: pruning write-back
// ===========================================================================
test('pruning write-back: a GET reporting pruned entries restores only the survivors and writes them back once', async ({
  browser,
}) => {
  const state = createWorkspaceFake({
    agentIds: [agentA, agentB, agentC],
    frontmostAgentId: agentB,
    prunedIds: new Set([agentC]),
  });

  const context = await browser.newContext();
  const page = await context.newPage();
  await installWorkspaceFake(context, state);
  const socket = await setupAgents(page);

  await page.goto('/terminals');

  await expect(railNameEntries(page)).toHaveText(['alpha', 'beta'], {
    timeout: FIRST_RENDER_TIMEOUT,
  });
  await socket.waitForAttach(1); // only the saved (surviving) frontmost, beta
  expect(socket.attachedAgentIds).toEqual([agentB]);

  // Exactly one follow-up PUT, carrying only the two survivors.
  await state.waitForState(() => state.puts.length >= 1);
  expect(state.puts[0]).toEqual({ agentIds: [agentA, agentB], frontmostAgentId: agentB });

  // No further PUT from anything the restore itself did.
  await page.waitForTimeout(1200);
  expect(state.puts.length).toBe(1);

  await context.close();
});

// ===========================================================================
// Case 3: URL intent
// ===========================================================================
test('URL intent: opening an unsaved agent path appends it as frontmost, exactly one PUT', async ({
  browser,
}) => {
  const state = createWorkspaceFake({
    agentIds: [agentA, agentB, agentC],
    frontmostAgentId: agentC,
  });

  const context = await browser.newContext();
  const page = await context.newPage();
  await installWorkspaceFake(context, state);
  const socket = await setupAgents(page);

  // delta (agentD) was never saved.
  await page.goto(`/terminals/${agentD}`);

  await expect(railNameEntries(page)).toHaveText(['alpha', 'beta', 'gamma', 'delta'], {
    timeout: FIRST_RENDER_TIMEOUT,
  });
  // Only delta attaches — an explicit URL connects nothing else: alpha,
  // beta and gamma are restored idle.
  await socket.waitForAttach(1);
  expect(socket.attachedAgentIds).toEqual([agentD]);

  await state.waitForState(() => state.puts.length >= 1);
  expect(state.puts[0]).toEqual({
    agentIds: [agentA, agentB, agentC, agentD],
    frontmostAgentId: agentD,
  });

  // Settle past the debounce and re-check: a trailing duplicate PUT would
  // not be caught by the poll above, which returns as soon as the first
  // one arrives.
  await page.waitForTimeout(1200);
  expect(state.puts.length).toBe(1);

  await context.close();
});

// ===========================================================================
// Case 4: owner only
// ===========================================================================
test('owner only: PUTs come only from the owning page, never the non-owner', async ({
  browser,
}) => {
  const state = createWorkspaceFake();
  const context = await browser.newContext();
  await installWorkspaceFake(context, state);

  const owner = await context.newPage();
  const other = await context.newPage();
  const ownerSocket = await setupAgents(owner);
  const otherSocket = await setupAgents(other);

  const ownerPuts: string[] = [];
  const otherPuts: string[] = [];
  owner.on('request', (req) => {
    if (req.method() === 'PUT' && req.url().includes('/terminal-workspace'))
      ownerPuts.push(req.url());
  });
  other.on('request', (req) => {
    if (req.method() === 'PUT' && req.url().includes('/terminal-workspace'))
      otherPuts.push(req.url());
  });

  await owner.goto(`/terminals/${agentA}`);
  await within(
    ownerSocket.waitForAttach(1),
    EVENT_WAIT_TIMEOUT,
    () =>
      `owner page did not attach alpha; owner attached ${agentNames(ownerSocket.attachedAgentIds)}`
  );

  // The non-owner tab loads an agent path, so the owner selects that agent
  // and the non-owner shows "Terminals open in another window": it cannot
  // become owner, so it never restores
  // or writes. The coordinator forwards this open to the owner tab
  // (unchanged, existing cross-tab behaviour), which the owner then
  // writes back — the state fake will already show a PUT or two by the
  // time the owner makes its own change below.
  await other.goto(`/terminals/${agentB}`);
  await expect(other.locator('#terminal-workspace')).toContainText(
    'Terminals open in another window',
    {
      timeout: FIRST_RENDER_TIMEOUT,
    }
  );
  expect(otherSocket.attaches, 'non-owner page attached a terminal').toBe(0);

  // Wait for the owner to attach the forwarded beta before it opens gamma.
  // The non-owner's message above appears once the owner has selected beta,
  // which can be before beta's pane has been shown and measured; a pane
  // attaches only then (terminal-pane initTerminal waits for a measurable
  // pane). Opening gamma hides beta's pane, so a beta that has not attached
  // by then stays pending and never attaches (ptone/scion#4638).
  await within(
    ownerSocket.waitForAttach(2),
    EVENT_WAIT_TIMEOUT,
    () =>
      `owner page did not attach the forwarded beta; owner attached ${agentNames(ownerSocket.attachedAgentIds)}`
  );
  expect(ownerSocket.attachedAgentIds, 'owner page attaches before its own change').toEqual([
    agentA,
    agentB,
  ]);

  // A change made directly in the owner tab (agentC, not yet open, so this
  // is not satisfied by anything above) produces its own PUT: wait for a
  // PUT that lands after this point and whose body reflects it, not just
  // any PUT the owner has ever sent.
  const putsBeforeChange = state.puts.length;
  await navigateToTerminal(owner, agentC);
  // Owner attaches: A (direct open), B (forwarded from the non-owner's
  // visit, above), then C (this change).
  await within(
    ownerSocket.waitForAttach(3),
    EVENT_WAIT_TIMEOUT,
    () =>
      `owner page did not attach gamma after opening it; owner attached ${agentNames(ownerSocket.attachedAgentIds)}`
  );
  await within(
    state.waitForState(
      () => state.puts.length > putsBeforeChange && state.puts.at(-1)?.frontmostAgentId === agentC
    ),
    EVENT_WAIT_TIMEOUT,
    () =>
      `no PUT with frontmost gamma after the owner opened gamma; PUTs since then: ${describePuts(state.puts.slice(putsBeforeChange))}; all PUTs: ${describePuts(state.puts)}`
  );

  expect(otherPuts, `non-owner page sent PUTs; stored PUTs: ${describePuts(state.puts)}`).toEqual(
    []
  );
  expect(ownerPuts.length, 'owner page sent no PUT').toBeGreaterThan(0);

  await context.close();
});
