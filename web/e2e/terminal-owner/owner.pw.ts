import { expect, test, type Page } from '@playwright/test';

interface Result {
  status: string;
  generation?: string;
  focus?: string;
  desktopForeground?: string;
  text?: string;
}
interface State {
  key: string;
  lockName: string;
  owner: boolean;
  generation: string | null;
  executions: number;
  sessions: string[];
}
declare global {
  interface Window {
    fixture: {
      open(agentId: string, requestId: string, timeoutMs?: number): Promise<Result>;
      snapshot(): State;
      pause(value: boolean): void;
      stop(): void;
    };
  }
}

async function load(page: Page, query = '') {
  await page.goto(`/${query}`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => 'fixture' in window);
}

function open(page: Page, agentId = 'agent-a', requestId = 'request-a') {
  return page.evaluate(({ agentId, requestId }) => window.fixture.open(agentId, requestId), {
    agentId,
    requestId,
  });
}

function snapshot(page: Page) {
  return page.evaluate(() => window.fixture.snapshot());
}

test('simultaneous independent tabs elect one owner and execute a request once', async ({
  context,
}) => {
  const a = await context.newPage();
  const b = await context.newPage();
  await Promise.all([load(a), load(b)]);
  expect(await a.evaluate(() => window.opener)).toBeNull();
  expect(await b.evaluate(() => window.opener)).toBeNull();
  const results = await Promise.all([open(a), open(b)]);
  expect(results.map((result) => result.status)).toEqual(['selected', 'selected']);
  expect(results[0].generation).toBe(results[1].generation);
  const states = await Promise.all([snapshot(a), snapshot(b)]);
  expect(states.filter((state) => state.owner)).toHaveLength(1);
  expect(states.reduce((sum, state) => sum + state.executions, 0)).toBe(1);
  await open(b);
  expect(
    (await Promise.all([snapshot(a), snapshot(b)])).reduce(
      (sum, state) => sum + state.executions,
      0
    )
  ).toBe(1);
});

test('distinct requests reuse one session; conflicting request IDs are rejected', async ({
  context,
}) => {
  const a = await context.newPage();
  const b = await context.newPage();
  await Promise.all([load(a), load(b)]);
  await open(a);
  expect((await open(b, 'agent-a', 'request-b')).status).toBe('selected');
  expect((await open(b, 'agent-b', 'request-a')).status).toBe('request-id-conflict');
  expect(await snapshot(a)).toMatchObject({ executions: 2, sessions: ['agent-a'] });
});

test('denied focus still acknowledges selection without foreground success', async ({
  context,
}) => {
  const owner = await context.newPage();
  const caller = await context.newPage();
  await load(owner, '?denyFocus=1');
  await load(caller);
  await open(owner);
  expect(await open(caller, 'agent-b', 'request-b')).toMatchObject({
    status: 'selected',
    focus: 'not-confirmed',
    desktopForeground: 'unverified',
    text: 'Selected in your terminal workspace',
  });
  expect((await snapshot(caller)).owner).toBe(false);
});

test('delayed owner leaves requests pending, retries execute once after resume', async ({
  context,
}) => {
  const owner = await context.newPage();
  const caller = await context.newPage();
  await Promise.all([load(owner), load(caller)]);
  await open(owner);
  await owner.evaluate(() => window.fixture.pause(true));
  for (let attempt = 0; attempt < 2; attempt++) {
    expect((await open(caller, 'agent-b', 'delayed')).status).toBe('pending');
    expect((await snapshot(caller)).owner).toBe(false);
  }
  await owner.evaluate(() => window.fixture.pause(false));
  expect((await open(caller, 'agent-b', 'delayed')).status).toBe('selected');
  expect((await snapshot(owner)).executions).toBe(2);
});

test('debugger-suspended owner keeps its lock; timeout cannot create another owner', async ({
  context,
}) => {
  const owner = await context.newPage();
  const caller = await context.newPage();
  await Promise.all([load(owner), load(caller)]);
  await open(owner);
  const before = await snapshot(owner);
  const cdp = await context.newCDPSession(owner);
  // Prove JS suspension with a Debugger.paused event. This is not evidence of
  // Chrome's automatic page-freeze policy (see README's unavailable matrix).
  await cdp.send('Debugger.enable');
  const paused = new Promise<void>((resolve) => cdp.once('Debugger.paused', () => resolve()));
  const pausedTask = cdp.send('Runtime.evaluate', { expression: 'debugger;' });
  await paused;
  try {
    expect((await open(caller, 'agent-b', 'suspended')).status).toBe('pending');
    expect((await snapshot(caller)).owner).toBe(false);
    const held = await caller.evaluate(async () => (await navigator.locks.query()).held);
    expect(held?.filter((lock) => lock.name === before.lockName)).toHaveLength(1);
  } finally {
    await cdp.send('Debugger.resume');
    await pausedTask;
    await cdp.send('Debugger.disable');
    await cdp.detach();
  }
  expect((await open(caller, 'agent-b', 'suspended')).generation).toBe(before.generation);
});

test('owner exit allows a later claimant with a fresh generation', async ({ context }) => {
  const owner = await context.newPage();
  const caller = await context.newPage();
  await Promise.all([load(owner), load(caller)]);
  const original = await open(owner);
  await owner.close();
  const result = await open(caller, 'agent-b', 'after-exit');
  expect(result.status).toBe('selected');
  expect(result.generation).not.toBe(original.generation);
  expect((await snapshot(caller)).owner).toBe(true);
});

test('account, base path and actual hub origin isolate ownership', async ({ context }) => {
  const pages = await Promise.all(Array.from({ length: 4 }, () => context.newPage()));
  await Promise.all([
    load(pages[0]),
    load(pages[1], '?account=other'),
    load(pages[2], '?base=/other/'),
  ]);
  await pages[3].goto('http://localhost:4517/', { waitUntil: 'domcontentloaded' });
  await pages[3].waitForFunction(() => 'fixture' in window);
  const results = await Promise.all(pages.map((page) => open(page)));
  expect(results.every((result) => result.status === 'selected')).toBe(true);
  expect(new Set(results.map((result) => result.generation)).size).toBe(4);
  expect((await Promise.all(pages.map(snapshot))).every((state) => state.owner)).toBe(true);
});

for (const missing of ['locks', 'BroadcastChannel', 'secureContext']) {
  test(`unsupported ${missing} does not claim or select`, async ({ page }) => {
    await page.addInitScript((missing) => {
      if (missing === 'locks') Object.defineProperty(navigator, 'locks', { value: undefined });
      if (missing === 'BroadcastChannel')
        Object.defineProperty(window, 'BroadcastChannel', { value: undefined });
      if (missing === 'secureContext')
        Object.defineProperty(window, 'isSecureContext', { value: false });
    }, missing);
    await load(page);
    expect((await open(page)).status).toBe('unsupported');
    expect(await snapshot(page)).toMatchObject({ owner: false, executions: 0, sessions: [] });
  });
}

test('stale generation messages cannot execute a selection', async ({ context }) => {
  const owner = await context.newPage();
  const caller = await context.newPage();
  await Promise.all([load(owner), load(caller)]);
  await open(owner);
  const state = await snapshot(owner);
  await caller.evaluate(({ key, lockName }) => {
    const channel = new BroadcastChannel(lockName);
    channel.postMessage({
      key,
      type: 'open',
      generation: 'departed-owner',
      agentId: 'stale',
      requestId: 'stale',
    });
    channel.close();
  }, state);
  // A subsequent round trip is a barrier after the stale message.
  await open(caller, 'agent-b', 'valid');
  expect(await snapshot(owner)).toMatchObject({ executions: 2, sessions: ['agent-a', 'agent-b'] });
});
