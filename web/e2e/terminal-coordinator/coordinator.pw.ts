import { test, expect, type Page } from '@playwright/test';

const agent = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const other = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
async function start(
  page: Page,
  accountId = 'fixture-account',
  base = '/',
  deny = true,
  url = '/'
) {
  await page.route('**/api/v1/agents/**', (route) =>
    route.fulfill({
      json: route.request().url().includes('/pty')
        ? {}
        : { id: route.request().url().split('/').pop(), phase: 'running' },
    })
  );
  await page.routeWebSocket('**/api/v1/agents/**/pty?*', (socket) => {
    socket.onMessage(() => {});
  });
  await page.goto(url);
  await page.waitForFunction(() => !!window.fixture);
  await page.evaluate(
    ({ accountId, base, deny }) =>
      window.fixture.start(
        {
          hubUrl: location.origin + base,
          accountId,
        },
        deny
      ),
    { accountId, base, deny }
  );
}
const snapshot = (page: Page) => page.evaluate(() => window.fixture.snapshot());
const open = (page: Page, id = agent, requestId = 'request-one', timeoutMs = 1000) =>
  page.evaluate(({ id, requestId, timeoutMs }) => window.fixture.open(id, requestId, timeoutMs), {
    id,
    requestId,
    timeoutMs,
  });

test('simultaneous first requests elect one owner and one connected session', async ({
  context,
}) => {
  const a = await context.newPage();
  const b = await context.newPage();
  await Promise.all([start(a), start(b)]);
  const results = await Promise.all([open(a), open(b)]);
  expect(results.map((r) => r.status)).toEqual(['selected', 'selected']);
  await expect
    .poll(async () =>
      (await Promise.all([snapshot(a), snapshot(b)]))
        .flatMap((s) => s.sessions)
        .map((s) => s.connection)
    )
    .toEqual(['connected']);
  const states = await Promise.all([snapshot(a), snapshot(b)]);
  expect(states.filter((s) => s.owner)).toHaveLength(1);
  expect(states.reduce((n, s) => n + s.initialized, 0)).toBe(1);
  expect(states.reduce((n, s) => n + s.selections, 0)).toBe(1);
});

test('owner survives Chat and Dashboard; denial acknowledges selection without fallback', async ({
  context,
}) => {
  const a = await context.newPage();
  const b = await context.newPage();
  await Promise.all([start(a), start(b)]);
  await open(a);
  const generation = (await snapshot(a)).generation;
  for (const mode of ['Chat', 'Dashboard']) {
    await a.evaluate((mode) => window.fixture.mode(mode), mode);
    const result = await open(b, other, mode);
    expect(result).toMatchObject({ status: 'selected', focus: 'not-confirmed' });
    expect((await snapshot(a)).generation).toBe(generation);
    expect((await snapshot(b)).sessions).toEqual([]);
    await expect(a.locator('main')).toHaveText(`Terminal ${other}`);
  }
  expect((await snapshot(a)).sessions).toHaveLength(2);
  expect(a.url()).toBe('http://127.0.0.1:4519/');
});

test('duplicate delivery and new requests for an existing agent never reinitialize', async ({
  context,
}) => {
  const owner = await context.newPage();
  const requester = await context.newPage();
  await Promise.all([start(owner), start(requester)]);
  await open(owner);
  const state = await snapshot(owner);
  await requester.evaluate(
    ({ state, agent }) => {
      const channel = new BroadcastChannel(state.coordinationKey);
      for (let i = 0; i < 10; i++)
        channel.postMessage({
          key: state.coordinationKey,
          type: 'open',
          requestId: 'duplicate-wire',
          agentId: agent,
          generation: state.generation,
        });
      channel.close();
    },
    { state, agent }
  );
  await expect.poll(async () => (await snapshot(owner)).selections).toBe(2);
  expect(await open(requester, agent, 'duplicate-wire')).toMatchObject({ status: 'selected' });
  expect(await open(requester, other, 'request-one')).toMatchObject({
    status: 'request-id-conflict',
  });
  expect(await open(requester, agent.toUpperCase(), 'fresh-intent')).toMatchObject({
    status: 'selected',
  });
  await expect.poll(async () => (await snapshot(owner)).initialized).toBe(1);
  expect((await snapshot(owner)).selections).toBe(3);
  expect((await snapshot(requester)).sessions).toEqual([]);
});

test('held lock without an acknowledgment remains pending on repeated retries', async ({
  context,
}) => {
  const holder = await context.newPage();
  const requester = await context.newPage();
  await Promise.all([start(holder), start(requester)]);
  const { coordinationKey } = await snapshot(holder);
  await holder.evaluate(async (key) => {
    await new Promise<void>((resolve) => {
      void navigator.locks.request(key, async () => {
        resolve();
        await new Promise(() => {});
      });
    });
  }, coordinationKey);
  for (let i = 0; i < 3; i++) {
    expect(await open(requester, agent, 'held', 80)).toMatchObject({ status: 'pending' });
    expect((await snapshot(requester)).owner).toBe(false);
    expect((await snapshot(requester)).sessions).toEqual([]);
  }
  expect(await requester.evaluate(async () => (await navigator.locks.query()).held?.length)).toBe(
    1
  );
});

test('wrong request, agent, generation and malformed acknowledgments cannot resolve an open', async ({
  context,
}) => {
  const holder = await context.newPage();
  const requester = await context.newPage();
  await Promise.all([start(holder), start(requester)]);
  const { coordinationKey } = await snapshot(holder);
  await holder.evaluate(
    async ({ key, agent, other }) => {
      await new Promise<void>((resolve) => {
        void navigator.locks.request(key, async () => {
          resolve();
          await new Promise(() => {});
        });
      });
      const channel = new BroadcastChannel(key);
      channel.onmessage = ({ data }: MessageEvent<Record<string, unknown>>) => {
        if (data.type === 'discover')
          channel.postMessage({ ...data, type: 'owner', generation: 'actual-owner' });
        if (data.type === 'open') {
          const ack = { ...data, type: 'ack', status: 'selected', focus: 'not-confirmed' };
          for (const patch of [
            { requestId: 'wrong' },
            { agentId: other },
            { generation: 'stale-owner' },
            { status: 'unknown' },
            { focus: 'focused' },
            { key: 'wrong-scope' },
            { agentId: agent, generation: null },
          ])
            channel.postMessage({ ...ack, ...patch });
          channel.postMessage(null);
          channel.postMessage({ type: 'ack' });
        }
      };
    },
    { key: coordinationKey, agent, other }
  );
  expect(await open(requester, agent, 'validate', 250)).toMatchObject({ status: 'pending' });
  expect((await snapshot(requester)).sessions).toEqual([]);
});

test('account and deployment base path isolate ownership; normalized URLs share it', async ({
  context,
}) => {
  const pages = await Promise.all(Array.from({ length: 4 }, () => context.newPage()));
  await Promise.all([
    start(pages[0], 'account-a', '/hub'),
    start(pages[1], 'account-a', '/hub/'),
    start(pages[2], 'account-b', '/hub/'),
    start(pages[3], 'account-a', '/other/'),
  ]);
  expect((await Promise.all(pages.map((p) => open(p)))).every((r) => r.status === 'selected')).toBe(
    true
  );
  const states = await Promise.all(pages.map(snapshot));
  expect(states.filter((s) => s.owner)).toHaveLength(3);
  expect(states[0].coordinationKey).toBe(states[1].coordinationKey);
  expect(new Set(states.map((s) => s.coordinationKey)).size).toBe(3);
});

for (const missing of [
  'locks',
  'channel',
  'secure-context',
  'channel-constructor',
  'lock-rejection',
]) {
  test(`unsupported ${missing} is explicit and never attaches`, async ({ page }) => {
    await page.addInitScript((missing) => {
      if (missing === 'locks') Object.defineProperty(navigator, 'locks', { value: undefined });
      if (missing === 'channel')
        Object.defineProperty(window, 'BroadcastChannel', { value: undefined });
      if (missing === 'secure-context')
        Object.defineProperty(window, 'isSecureContext', { value: false });
      if (missing === 'channel-constructor')
        Object.defineProperty(window, 'BroadcastChannel', {
          value: class {
            constructor() {
              throw new DOMException('Denied', 'SecurityError');
            }
          },
        });
      if (missing === 'lock-rejection')
        Object.defineProperty(navigator, 'locks', {
          value: { request: () => Promise.reject(new DOMException('Denied', 'SecurityError')) },
        });
    }, missing);
    await start(page);
    expect(await open(page)).toMatchObject({ status: 'unsupported' });
    expect((await snapshot(page)).sessions).toEqual([]);
    expect((await snapshot(page)).supported).toBe(false);
  });
}

test('document teardown disposes sessions before another tab can claim', async ({ context }) => {
  const owner = await context.newPage();
  const requester = await context.newPage();
  await Promise.all([start(owner), start(requester)]);
  await open(owner);
  await expect.poll(async () => (await snapshot(owner)).sessions[0]?.connection).toBe('connected');
  await owner.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide')));
  expect((await snapshot(owner)).disposed).toBe(1);
  expect(await open(owner)).toMatchObject({ status: 'stopped' });
  expect(await open(requester)).toMatchObject({ status: 'selected' });
  expect((await snapshot(requester)).owner).toBe(true);
});

test('late selection acknowledgment is retained for retry without another initialization', async ({
  context,
}) => {
  const owner = await context.newPage();
  const requester = await context.newPage();
  await Promise.all([start(owner), start(requester)]);
  await open(owner);
  await owner.evaluate(() => window.fixture.blockSelection());
  expect(await open(requester, other, 'delayed', 80)).toMatchObject({ status: 'pending' });
  expect((await snapshot(requester)).owner).toBe(false);
  await owner.evaluate(() => window.fixture.releaseSelection());
  expect(await open(requester, other, 'delayed')).toMatchObject({ status: 'selected' });
  await expect.poll(async () => (await snapshot(owner)).initialized).toBe(2);
  expect((await snapshot(owner)).selections).toBe(2);
});

test('teardown aborts a delayed selection and cannot request late focus', async ({ page }) => {
  await start(page);
  await page.evaluate(() => window.fixture.blockSelection());
  expect(await open(page, agent, 'canceled', 80)).toMatchObject({ status: 'pending' });
  await page.evaluate(() => {
    window.fixture.stop();
    window.fixture.releaseSelection();
  });
  expect(await open(page)).toMatchObject({ status: 'stopped' });
  expect((await snapshot(page)).selections).toBe(0);
  expect((await snapshot(page)).focusAttempts).toBe(0);
  expect((await snapshot(page)).owner).toBe(false);
});

test('obsolete owner generations cannot create sessions or change selection', async ({
  context,
}) => {
  const owner = await context.newPage();
  const requester = await context.newPage();
  await Promise.all([start(owner), start(requester)]);
  await open(owner);
  const state = await snapshot(owner);
  await requester.evaluate(
    ({ state, other }) => {
      const channel = new BroadcastChannel(state.coordinationKey);
      channel.postMessage({
        key: state.coordinationKey,
        type: 'open',
        requestId: 'obsolete',
        agentId: other,
        generation: 'obsolete-generation',
      });
      channel.close();
    },
    { state, other }
  );
  await open(requester, agent, 'barrier');
  expect((await snapshot(owner)).sessions.map((s) => s.agentId)).toEqual([agent]);
  expect((await snapshot(owner)).selections).toBe(2);
});

test('default focus result is a document observation separate from selection', async ({ page }) => {
  await start(page, 'fixture-account', '/', false);
  await page.bringToFront();
  const result = await open(page);
  expect(result.status).toBe('selected');
  const observed = await page.evaluate(
    () => document.visibilityState === 'visible' && document.hasFocus()
  );
  expect(result.focus).toBe(observed ? 'document-focused' : 'not-confirmed');
});

test('invalid trusted scope and agent inputs fail before any session is created', async ({
  page,
}) => {
  await start(page);
  const rejected = await page.evaluate(async () => {
    const invalidScopes = [
      { hubUrl: 'https://user:password@example.test/', accountId: 'a' },
      { hubUrl: 'https://example.test/?account=b', accountId: 'a' },
      { hubUrl: 'https://example.test/#fragment', accountId: 'a' },
      { hubUrl: 'file:///tmp/hub', accountId: 'a' },
      { hubUrl: location.origin, accountId: '   ' },
    ];
    let count = 0;
    for (const scope of invalidScopes) {
      try {
        window.fixture.start(scope);
      } catch {
        count++;
      }
    }
    try {
      await window.fixture.open('../not-a-uuid', 'invalid');
    } catch {
      count++;
    }
    return count;
  });
  expect(rejected).toBe(6);
  expect((await snapshot(page)).sessions).toEqual([]);
});

test('R1 teardown attempts every session after a disposer throws and retains the lock', async ({
  context,
}) => {
  const owner = await context.newPage();
  const requester = await context.newPage();
  await Promise.all([start(owner), start(requester)]);
  await open(owner);
  await open(owner, other, 'second');
  await expect
    .poll(async () => (await snapshot(owner)).sessions.map((s) => s.connection))
    .toEqual(['connected', 'connected']);
  const result = await owner.evaluate(() => {
    window.fixture.captureSessions();
    window.fixture.throwOnDispose(0);
    let failure: { name: string; count: number } | null = null;
    try {
      window.fixture.stop();
    } catch (error) {
      failure = {
        name: error instanceof Error ? error.name : 'unknown',
        count: error instanceof AggregateError ? error.errors.length : 1,
      };
    }
    window.fixture.stop();
    return { failure, ...window.fixture.teardownState() };
  });
  expect(result.disposalAttempts).toEqual([0, 1]);
  expect(result.sends).toEqual([false, false]);
  expect(result.connections[1]).toBe('closed');
  expect(result.failure).toEqual({ name: 'AggregateError', count: 1 });
  expect(await open(requester, agent, 'still-held', 100)).toMatchObject({ status: 'pending' });
  expect((await snapshot(requester)).owner).toBe(false);
  expect((await snapshot(requester)).sessions).toEqual([]);
  expect(await requester.evaluate(async () => (await navigator.locks.query()).held?.length)).toBe(
    1
  );
});

test('R2 actual insecure HTTP default-ID opens return unsupported and stopped without UUID API', async ({
  page,
  request,
}) => {
  const javascript = await (await request.get('http://127.0.0.1:4519/fixture.js')).text();
  await page.route('http://insecure.example/**', (route) =>
    route.fulfill(
      route.request().url().endsWith('/fixture.js')
        ? { contentType: 'text/javascript', body: javascript }
        : {
            contentType: 'text/html',
            body: '<!doctype html><main></main><script type="module" src="/fixture.js"></script>',
          }
    )
  );
  await start(page, 'fixture-account', '/', true, 'http://insecure.example/');
  expect(
    await page.evaluate(() => ({ secure: isSecureContext, uuid: typeof crypto.randomUUID }))
  ).toEqual({ secure: false, uuid: 'undefined' });
  expect(await page.evaluate((agent) => window.fixture.open(agent), agent)).toMatchObject({
    status: 'unsupported',
    requestId: 'unsubmitted',
  });
  expect(await open(page, agent, 'supplied-unsupported')).toMatchObject({
    status: 'unsupported',
    requestId: 'supplied-unsupported',
  });
  await page.evaluate(() => window.fixture.stop());
  expect(await page.evaluate((agent) => window.fixture.open(agent), agent)).toMatchObject({
    status: 'stopped',
    requestId: 'unsubmitted',
  });
  expect(await open(page, agent, 'supplied-stopped')).toMatchObject({
    status: 'stopped',
    requestId: 'supplied-stopped',
  });
  expect((await snapshot(page)).initialized).toBe(0);
  expect((await snapshot(page)).sessions).toEqual([]);
});

test('R2 secure default-ID opens retain UUID generation and stopped opens need no UUID API', async ({
  page,
}) => {
  await start(page);
  const result = await page.evaluate((agent) => window.fixture.open(agent), agent);
  expect(result.status).toBe('selected');
  expect(result.requestId).toMatch(
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
  );
  await page.evaluate(() => {
    window.fixture.stop();
    Object.defineProperty(crypto, 'randomUUID', { value: undefined });
  });
  expect(await page.evaluate((agent) => window.fixture.open(agent), agent)).toMatchObject({
    status: 'stopped',
    requestId: 'unsubmitted',
  });
});
