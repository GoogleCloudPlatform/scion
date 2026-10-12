// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Async agent create from the web form (ptone/scion#4664), end to end.
 *
 * Acts as a member test identity on a hub with
 * server.hub.async_agent_launch on (a throwaway local hub by default; see
 * state.ts for E2E_TARGET_URL). In order, sharing one identity, one
 * browser context and one project:
 *
 * - A1: submitting the create form answers 201 at once with
 *   agent.launch.active true, the browser opens the agent page, and the
 *   browser sends no POST .../agents/{id}/start (read from the browser's
 *   own request log).
 * - A2 + A3 (needs a real launch): without a reload, the agent page shows
 *   the launch banner and then the agent reaches running; the Status tab
 *   phases only move forward; the SSE status events carry launch
 *   {state, step, error, endReason} and end with the launch ended without
 *   an error. The running path is a FIXTURE: the shipped harness's no-auth
 *   fallback (drop-to-shell), with no credentials anywhere; the agent's
 *   appliedConfig must show noAuth and harnessAuth "none".
 * - Failure panel, no container runtime (only where agents cannot start):
 *   the broker fails the launch; the failed-create panel shows a known
 *   code and a message, Delete and Create again, and no Start or Resume.
 *   This is not A4: it does not exercise a bad image.
 * - A4 (needs a real launch): a template whose image tag does not exist
 *   gives the same panel after an async (201) submit.
 * - A5 (afterAll): the agents, the project, the fixture template and the
 *   test identity are deleted and checked absent; only then is the
 *   evidence (DOM timeline, SSE launch payloads, request log; no cookies
 *   or tokens) written to the test output directory.
 *
 * Checks that need a real launch are skipped, with the reason, where no
 * container runtime is usable; a skipped check never counts as passed.
 */

import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import {
  expect,
  test,
  type APIRequestContext,
  type BrowserContext,
  type Page,
} from '@playwright/test';

import { assertTestIdentity, redeemTestIdentity } from '../harness/auth.js';
import { readHubState, runScionCLI, type HubState } from '../harness/hub.js';
import {
  documentId,
  installPageRecorders,
  sseRecords,
  timelineRows,
  type SseLaunchRecord,
  type TimelineRow,
} from './page-recorder.js';
import { classifyRead, isGone, parseTimeoutMs, type ReadState } from './journey-lib.js';
import {
  FORBIDDEN_HEADER_ACTIONS,
  IMAGE_FAILURE_CODES,
  KNOWN_LAUNCH_ERROR_CODES,
  LAUNCH_PHASES,
  SELECTORS,
  confirmDialogButton,
  headerButtonTexts,
  incompleteCreateAgainLink,
  incompleteDeleteButton,
  launchErrorCodeOf,
  phaseBadge,
} from './selectors.js';
import {
  isRemoteTarget,
  readSuiteState,
  remoteTargetFromEnv,
  type JourneyTarget,
} from './state.js';

/** How long a launch may take to reach running or to fail. */
const LAUNCH_TIMEOUT = parseTimeoutMs(
  process.env.E2E_LAUNCH_TIMEOUT_MS,
  300_000,
  'E2E_LAUNCH_TIMEOUT_MS'
);

/** POST of the create (the form uses /api/v1/agents; the project path is accepted too). */
const CREATE_PATH = /^\/api\/v1\/(projects\/[^/]+\/)?agents$/;

function startPath(agentId: string): RegExp {
  return new RegExp(`^/api/v1/(projects/[^/]+/)?agents/${agentId}/start$`);
}

interface RequestRecord {
  t: number;
  method: string;
  path: string;
  status?: number;
}

/** A page with its browser-side request log and document load count. */
interface TrackedPage {
  page: Page;
  requests: RequestRecord[];
  documentLoads: number;
}

interface CreatedAgent {
  id: string;
  name: string;
  check: string;
}

interface SubmitResult {
  agentId: string;
  status: number;
  launch: { state?: string; active?: boolean; kind?: string } | undefined;
  template: string | undefined;
  submitAt: number;
  respondedAt: number;
  docIdBefore: string;
  loadsBefore: number;
}

/** Shared state of the serial journey. */
const journey: {
  target?: JourneyTarget;
  context?: BrowserContext;
  tmpDir?: string;
  email?: string;
  identityId?: string;
  projectId?: string;
  projectCreated: boolean;
  template?: { name: string; id: string };
  agents: CreatedAgent[];
  a1Page?: TrackedPage;
  a1Submit?: SubmitResult;
  evidence: Record<string, unknown>;
} = { projectCreated: false, agents: [], evidence: {} };

function suffix(): string {
  return crypto.randomBytes(3).toString('hex');
}

function target(): JourneyTarget {
  if (!journey.target) throw new Error('journey target not set up');
  return journey.target;
}

function member(): APIRequestContext {
  if (!journey.context) throw new Error('journey context not set up');
  return journey.context.request;
}

function localHub(): HubState | undefined {
  return target().hub;
}

function resolveTarget(): JourneyTarget {
  if (isRemoteTarget()) return remoteTargetFromEnv();
  const suite = readSuiteState();
  if (!suite) throw new Error('No async create suite state; did global-setup run?');
  const hub = readHubState(suite.hubStateFile);
  if (!hub?.homeDir) throw new Error(`No local hub state at ${suite.hubStateFile}`);
  return {
    mode: 'local',
    baseURL: hub.baseURL,
    hub,
    launchCapable: suite.launchCapable,
    launchCapableReason: suite.launchCapableReason,
    noRuntimeConfirmed: !suite.launchCapable,
    badImage: suite.badImage,
    agentImage: suite.agentImage,
    harnessConfig: suite.harnessConfig,
  };
}

/** A request as the dev-auth admin of the local hub (fixtures and cleanup only). */
async function adminRequest(
  hub: HubState,
  method: string,
  urlPath: string
): Promise<{ status: number; body: unknown }> {
  const res = await fetch(new URL(urlPath, hub.baseURL), {
    method,
    headers: { Authorization: `Bearer ${hub.devToken}` },
  });
  const text = await res.text();
  let body: unknown = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    body = null;
  }
  return { status: res.status, body };
}

async function trackedPage(): Promise<TrackedPage> {
  const page = await journey.context!.newPage();
  await installPageRecorders(page);
  const tracked: TrackedPage = { page, requests: [], documentLoads: 0 };
  page.on('request', (req) => {
    const url = new URL(req.url());
    if (url.pathname.startsWith('/api/')) {
      tracked.requests.push({ t: Date.now(), method: req.method(), path: url.pathname });
    }
  });
  page.on('response', (res) => {
    const url = new URL(res.url());
    const rec = [...tracked.requests]
      .reverse()
      .find(
        (r) =>
          r.status === undefined && r.path === url.pathname && r.method === res.request().method()
      );
    if (rec) rec.status = res.status();
  });
  page.on('domcontentloaded', () => {
    tracked.documentLoads++;
  });
  return tracked;
}

/** Picks an option of a Shoelace select by its visible text. */
async function chooseOption(page: Page, selectSelector: string, text: string): Promise<void> {
  const select = page.locator(SELECTORS.createPage).locator(selectSelector);
  await select.click();
  await select.locator(SELECTORS.selectOption).filter({ hasText: text }).first().click();
}

/** Picks an option of a Shoelace select by its value. */
async function chooseOptionValue(page: Page, selectSelector: string, value: string): Promise<void> {
  const select = page.locator(SELECTORS.createPage).locator(selectSelector);
  await select.click();
  await select.locator(`${SELECTORS.selectOption}[value="${value}"]`).click();
  await expect(select).toHaveJSProperty('value', value);
}

/** Fills the create form and submits it; returns what the browser saw. */
async function submitCreateForm(
  tracked: TrackedPage,
  opts: { name: string; templateName?: string; task: string; check: string }
): Promise<SubmitResult> {
  const { page } = tracked;
  const t = target();
  await page.goto(`/agents/new?projectId=${encodeURIComponent(journey.projectId!)}`, {
    waitUntil: 'domcontentloaded',
  });
  const form = page.locator(SELECTORS.createPage);
  await expect(form.locator(SELECTORS.createName)).toBeVisible();
  await form.locator(SELECTORS.createNameInput).fill(opts.name);
  if (opts.templateName) await chooseOption(page, SELECTORS.createTemplate, opts.templateName);
  if (t.harnessConfig) await chooseOptionValue(page, SELECTORS.createHarness, t.harnessConfig);
  await form.locator(SELECTORS.createTaskInput).fill(opts.task);

  const createResponse = page.waitForResponse(
    (res) =>
      res.request().method() === 'POST' && CREATE_PATH.test(new URL(res.url()).pathname)
  );
  const docIdBefore = await documentId(page);
  const loadsBefore = tracked.documentLoads;
  const submitAt = Date.now();
  await form.locator(SELECTORS.createSubmit).click();
  const res = await createResponse;
  const respondedAt = Date.now();
  const status = res.status();
  const body = (await res.json().catch(() => ({}))) as {
    agent?: {
      id?: string;
      template?: string;
      launch?: { state?: string; active?: boolean; kind?: string };
    };
  };
  const agentId = body.agent?.id ?? '';
  if (agentId) journey.agents.push({ id: agentId, name: opts.name, check: opts.check });
  return {
    agentId,
    status,
    launch: body.agent?.launch,
    template: body.agent?.template,
    submitAt,
    respondedAt,
    docIdBefore,
    loadsBefore,
  };
}

/** A1's assertions on a submit: 201, async launch accepted, agent page opened. */
async function expectAsyncSubmit(tracked: TrackedPage, submit: SubmitResult): Promise<void> {
  const formError = await tracked.page.locator(SELECTORS.createError).allInnerTexts();
  expect(submit.status, `POST create status (form error: ${formError.join(' | ')})`).toBe(201);
  expect(submit.agentId, 'agent id in the create response').toBeTruthy();
  expect(submit.launch, 'agent.launch in the create response').toBeDefined();
  expect(submit.launch?.active, 'agent.launch.active in the create response').toBe(true);
  expect(submit.launch?.kind).toBe('create');
  await expect(tracked.page).toHaveURL(new RegExp(`/agents/${submit.agentId}$`));
}

/** No POST .../start for the agent and no reload since the submit. */
async function expectNoStartNoReload(tracked: TrackedPage, submit: SubmitResult): Promise<void> {
  const starts = tracked.requests.filter(
    (r) => r.method === 'POST' && startPath(submit.agentId).test(r.path)
  );
  expect(starts, 'POST .../start sent by the browser').toEqual([]);
  expect(await documentId(tracked.page), 'same document since the submit (no reload)').toBe(
    submit.docIdBefore
  );
  expect(tracked.documentLoads, 'document loads since the submit').toBe(submit.loadsBefore);
}

/** The failed-create panel contract; returns the code shown. */
async function expectFailedCreatePanel(page: Page): Promise<{ code: string; message: string }> {
  await expect(page.locator(SELECTORS.incompleteBanner)).toBeVisible({ timeout: LAUNCH_TIMEOUT });
  // The panel may first show only "stopping" while the create winds down.
  await expect
    .poll(
      async () =>
        launchErrorCodeOf((await page.locator(SELECTORS.launchErrorCode).allInnerTexts()).join('')),
      { message: 'a launch error code in the panel', timeout: 120_000 }
    )
    .not.toBe('');
  const code = launchErrorCodeOf(await page.locator(SELECTORS.launchErrorCode).innerText());
  expect(KNOWN_LAUNCH_ERROR_CODES as readonly string[], `code ${code} is a known code`).toContain(
    code
  );
  const message = (await page.locator(SELECTORS.incompleteMessage).innerText()).trim();
  expect(message, 'the panel error message').not.toBe('');
  await expect(incompleteDeleteButton(page)).toBeVisible();
  await expect(incompleteCreateAgainLink(page)).toHaveAttribute(
    'href',
    `/agents/new?projectId=${encodeURIComponent(journey.projectId!)}`
  );
  const header = await headerButtonTexts(page);
  for (const action of FORBIDDEN_HEADER_ACTIONS) {
    expect(header, `header offers no "${action}"`).not.toContain(action);
  }
  return { code, message };
}

/**
 * What a direct GET of the agent says: gone only on a 404 or a soft-deleted
 * record (classifyRead). The member's session is used; after it ends,
 * the local hub's admin.
 */
async function agentReadState(
  request: APIRequestContext | null,
  agentId: string
): Promise<ReadState> {
  const urlPath = `/api/v1/agents/${agentId}`;
  if (request) {
    const res = await request.get(urlPath);
    const body: unknown = await res.json().catch(() => null);
    return classifyRead(res.status(), body, 'agent');
  }
  const hub = localHub();
  if (!hub) throw new Error('no request context and no local hub');
  const res = await adminRequest(hub, 'GET', urlPath);
  return classifyRead(res.status, res.body, 'agent');
}

async function agentGone(request: APIRequestContext | null, agentId: string): Promise<boolean> {
  return isGone(await agentReadState(request, agentId));
}

/** The final state of the agent page: failed-create panel, running, or neither yet. */
async function finalPageState(page: Page): Promise<'failed' | 'running' | ''> {
  if ((await page.locator(SELECTORS.incompleteBanner).count()) > 0) return 'failed';
  const badge = phaseBadge(page);
  if ((await badge.count()) > 0 && (await badge.getAttribute('status')) === 'running') {
    return 'running';
  }
  return '';
}

/** Deletes the agent from the failed-create panel and checks it is gone. */
async function deleteFromPanel(page: Page, agentId: string): Promise<void> {
  await incompleteDeleteButton(page).click();
  await confirmDialogButton(page).click();
  await expect
    .poll(() => agentGone(member(), agentId), {
      message: 'the agent is gone after Delete',
      timeout: 120_000,
    })
    .toBe(true);
}

/** The bad image fixture template: seeded on a local hub, given on a remote one. */
async function badImageTemplate(): Promise<string> {
  const t = target();
  if (t.mode === 'remote') {
    if (!t.badImageTemplate) throw new Error('set E2E_BAD_IMAGE_TEMPLATE for a remote target');
    return t.badImageTemplate;
  }
  if (journey.template) return journey.template.name;
  const hub = localHub()!;
  const name = `e2e-bad-image-${suffix()}`;
  const dir = path.join(hub.homeDir!, '.scion', 'templates', name);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(
    path.join(dir, 'scion-agent.yaml'),
    [
      'schema_version: "1"',
      'description: "e2e fixture: an image tag that does not exist"',
      `image: "${t.badImage!}"`,
      '',
    ].join('\n')
  );
  runScionCLI(hub, ['templates', 'push', name]);
  fs.rmSync(dir, { recursive: true, force: true });
  const list = await adminRequest(hub, 'GET', '/api/v1/templates?limit=100');
  const found = ((list.body as { templates?: Array<{ id: string; name: string; status?: string }> })
    ?.templates ?? []).find((tpl) => tpl.name === name);
  if (!found) throw new Error(`fixture template ${name} not found after push`);
  expect(found.status, 'fixture template status').toBe('active');
  journey.template = { name, id: found.id };
  return name;
}

function phaseRank(phase: string): number {
  return (LAUNCH_PHASES as readonly string[]).indexOf(phase);
}

/** Phases in order of appearance, consecutive repeats dropped. */
function phaseSequence(phases: string[]): string[] {
  return phases.filter((p, i) => p && p !== phases[i - 1]);
}

function expectForwardOnly(sequence: string[], what: string): void {
  for (const phase of sequence) {
    expect(
      phaseRank(phase),
      `${what}: ${sequence.join(' → ')} holds only launch phases`
    ).toBeGreaterThanOrEqual(0);
  }
  for (let i = 1; i < sequence.length; i++) {
    const label = `${what}: ${sequence.join(' → ')} never moves back`;
    expect(phaseRank(sequence[i]), label).toBeGreaterThan(phaseRank(sequence[i - 1]));
  }
}

test.describe.serial('async agent create from the web form', () => {
  test.beforeAll(async ({ browser }) => {
    const t = resolveTarget();
    journey.target = t;
    journey.evidence.target = {
      mode: t.mode,
      launchCapable: t.launchCapable,
      launchCapableReason: t.launchCapableReason,
    };
    journey.evidence.fixture = {
      runningPath: 'FIXTURE: no-auth drop-to-shell (no harness credentials)',
      harnessConfig: t.harnessConfig ?? 'form default',
      agentImage: t.agentImage,
      badImage: t.badImage ?? `template ${t.badImageTemplate ?? '(none)'}`,
    };
    console.info(
      `[journey] target ${t.mode}; launch capable: ${String(t.launchCapable)} ` +
        `(${t.launchCapableReason})`
    );

    // Task-owned private directory for the token and code files (resolved:
    // web-code refuses a path under a symlinked directory).
    journey.tmpDir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-ac-')));
    fs.chmodSync(journey.tmpDir, 0o700);

    let codeFile: string;
    if (t.mode === 'local') {
      const hub = t.hub!;
      const tokenFile = path.join(journey.tmpDir, 'identity.token');
      const issued = JSON.parse(
        runScionCLI(hub, [
          'hub',
          'test-identity',
          'issue',
          '--role',
          'member',
          '--purpose',
          'e2e async web create',
          '--out',
          tokenFile,
          '--json',
        ])
      ) as { identity: { id: string; email: string } };
      fs.rmSync(tokenFile, { force: true });
      journey.identityId = issued.identity.id;
      journey.email = issued.identity.email;
      codeFile = path.join(journey.tmpDir, 'code.txt');
      runScionCLI(hub, ['hub', 'test-identity', 'web-code', issued.identity.id, '--out', codeFile]);
    } else {
      codeFile = t.codeFile ?? '';
      journey.email = t.identityEmail ?? '';
    }

    journey.context = await browser.newContext({ baseURL: t.baseURL });
    await redeemTestIdentity(journey.context, { codeFile });
    await assertTestIdentity(journey.context.request, { email: journey.email!, role: 'member' });

    if (t.projectId) {
      journey.projectId = t.projectId;
    } else {
      const res = await member().post('/api/v1/projects', {
        data: { name: `e2e-async-create-${suffix()}` },
      });
      expect(res.status(), 'member creates a hub-managed project').toBe(201);
      const project = (await res.json()) as { id: string };
      journey.projectId = project.id;
      journey.projectCreated = true;
    }
  });

  test('A1: the web form create takes the async path and returns at once', async () => {
    const tracked = await trackedPage();
    journey.a1Page = tracked;
    const submit = await submitCreateForm(tracked, {
      name: `e2e-async-${suffix()}`,
      task: 'e2e async create journey',
      check: 'A1',
    });
    journey.a1Submit = submit;
    journey.evidence.a1 = {
      status: submit.status,
      launch: submit.launch,
      responseMs: submit.respondedAt - submit.submitAt,
    };
    await expectAsyncSubmit(tracked, submit);
    await expectNoStartNoReload(tracked, submit);
    if (!target().launchCapable) {
      // A2 (which re-checks the whole window) is skipped here: follow the
      // launch to its end, then check the window from submit to the end.
      await expect
        .poll(() => finalPageState(tracked.page), {
          message: 'the A1 launch reaches a final state',
          timeout: LAUNCH_TIMEOUT,
        })
        .not.toBe('');
      journey.evidence.a1FinalState = await finalPageState(tracked.page);
      await expectNoStartNoReload(tracked, submit);
    }
    await assertTestIdentity(tracked.page, { email: journey.email!, role: 'member' });
  });

  test('A2 + A3: launch progress updates live and the agent reaches running', async () => {
    const t = target();
    test.skip(
      !t.launchCapable,
      `A2/A3 NOT COVERED: needs a broker that can start containers (${t.launchCapableReason})`
    );
    const tracked = journey.a1Page!;
    const submit = journey.a1Submit!;
    const { page } = tracked;

    await expect(phaseBadge(page)).toHaveAttribute('status', 'running', {
      timeout: LAUNCH_TIMEOUT,
    });
    await expect(page.locator(SELECTORS.launchBanner)).toHaveCount(0);
    // The status event that ends the launch may trail the phase change.
    await expect
      .poll(
        async () => {
          const records = (await sseRecords(page, submit.agentId)).filter((r) => r.launch);
          return records.at(-1)?.launch?.state ?? '';
        },
        { message: 'an SSE status event with the launch ended', timeout: 60_000 }
      )
      .toBe('ended');

    const timeline: TimelineRow[] = await timelineRows(page, submit.agentId);
    const sse: SseLaunchRecord[] = await sseRecords(page, submit.agentId);
    journey.evidence.a2 = {
      submitAt: submit.submitAt,
      timeline,
      sse,
      requests: tracked.requests,
    };

    // A2: the launch banner was shown live, then went away; no error panel.
    expect(
      timeline.some((r) => r.launchBanner),
      'launch banner shown during the launch'
    ).toBe(true);
    expect(timeline.some((r) => r.incompleteBanner), 'no failed-create panel').toBe(false);
    expectForwardOnly(phaseSequence(timeline.map((r) => r.phase)), 'Status tab phases');

    // A2: SSE launch payloads.
    const withLaunch = sse.filter((r) => r.launch);
    expect(withLaunch.length, 'SSE status events with a launch').toBeGreaterThan(0);
    for (const r of withLaunch) {
      expect(['active', 'ended']).toContain(r.launch!.state);
      expect(r.launch!.kind).toBe('create');
    }
    expect(
      withLaunch.some((r) => r.launch!.state === 'active'),
      'an SSE event with the launch active'
    ).toBe(true);
    const last = withLaunch.at(-1)!.launch!;
    expect(last.state).toBe('ended');
    expect(last.error ?? '', 'no launch error at the end').toBe('');
    expect(['succeeded', 'running_observed']).toContain(last.endReason);
    expectForwardOnly(phaseSequence(sse.map((r) => r.phase ?? '')), 'SSE phases');

    // The running path is the credential-free no-auth fallback: the agent
    // must have started without credentials.
    const agentRes = await member().get(`/api/v1/agents/${submit.agentId}`);
    expect(agentRes.status()).toBe(200);
    const agentBody = (await agentRes.json()) as {
      agent?: { appliedConfig?: { noAuth?: boolean; harnessAuth?: string } };
      appliedConfig?: { noAuth?: boolean; harnessAuth?: string };
    };
    const applied = agentBody.appliedConfig ?? agentBody.agent?.appliedConfig;
    journey.evidence.a2NoAuth = { noAuth: applied?.noAuth, harnessAuth: applied?.harnessAuth };
    expect(applied?.noAuth, 'appliedConfig.noAuth: started without credentials').toBe(true);
    expect(applied?.harnessAuth, 'appliedConfig.harnessAuth').toBe('none');

    // A1b over the whole launch, and no reload.
    await expectNoStartNoReload(tracked, submit);
    await page.close();
  });

  test('failure panel, no-runtime launch failure (not A4)', async () => {
    const t = target();
    test.skip(
      t.launchCapable,
      'Agents can start here: the failed-create panel is checked by A4 with a bad image'
    );
    test.skip(
      !t.noRuntimeConfirmed,
      'Not known to lack a container runtime (set E2E_LAUNCH_CAPABLE=0 for a remote target)'
    );
    test.setTimeout(LAUNCH_TIMEOUT + 360_000);
    const template = await badImageTemplate();
    const tracked = await trackedPage();
    const submit = await submitCreateForm(tracked, {
      name: `e2e-async-noruntime-${suffix()}`,
      templateName: template,
      task: 'e2e async create journey, no runtime',
      check: 'failure-panel-no-runtime',
    });
    await expectAsyncSubmit(tracked, submit);
    expect(submit.template, 'the agent uses the fixture template').toBe(template);
    const panel = await expectFailedCreatePanel(tracked.page);
    await expectNoStartNoReload(tracked, submit);
    journey.evidence.failurePanelNoRuntime = {
      status: submit.status,
      launch: submit.launch,
      code: panel.code,
      messageLength: panel.message.length,
      timeline: await timelineRows(tracked.page, submit.agentId),
      sse: await sseRecords(tracked.page, submit.agentId),
      requests: tracked.requests,
    };
    await deleteFromPanel(tracked.page, submit.agentId);
    await tracked.page.close();
  });

  test('A4: an image tag that does not exist shows the failed-create panel', async () => {
    const t = target();
    test.skip(
      !t.launchCapable,
      `A4 NOT COVERED: needs a broker that can start containers (${t.launchCapableReason})`
    );
    test.setTimeout(LAUNCH_TIMEOUT + 360_000);
    const template = await badImageTemplate();
    const tracked = await trackedPage();
    const submit = await submitCreateForm(tracked, {
      name: `e2e-async-badimage-${suffix()}`,
      templateName: template,
      task: 'e2e async create journey, bad image',
      check: 'A4',
    });
    // The async path first: 201 with the launch active, not a 4xx at submit.
    await expectAsyncSubmit(tracked, submit);
    expect(submit.template, 'the agent uses the fixture template').toBe(template);
    const panel = await expectFailedCreatePanel(tracked.page);
    expect(
      IMAGE_FAILURE_CODES as readonly string[],
      `code ${panel.code} is an image failure code`
    ).toContain(panel.code);
    await expectNoStartNoReload(tracked, submit);
    journey.evidence.a4 = {
      status: submit.status,
      launch: submit.launch,
      code: panel.code,
      messageLength: panel.message.length,
      timeline: await timelineRows(tracked.page, submit.agentId),
      sse: await sseRecords(tracked.page, submit.agentId),
      requests: tracked.requests,
    };
    await deleteFromPanel(tracked.page, submit.agentId);
    await tracked.page.close();
  });

  // eslint-disable-next-line no-empty-pattern -- Playwright needs the fixtures argument
  test.afterAll('A5: cleanup is verified by absence', async ({}, testInfo) => {
    const residue: string[] = [];
    const violations: string[] = [];
    const cleanup: Record<string, unknown> = {};
    const t = journey.target;
    const hub = t?.hub;

    // A1's no-/start rule over the whole window, whatever the mode: from
    // the submit until the page closes here.
    if (journey.a1Page && journey.a1Submit?.agentId) {
      const starts = journey.a1Page.requests.filter(
        (r) => r.method === 'POST' && startPath(journey.a1Submit!.agentId).test(r.path)
      );
      cleanup.a1StartRequestsWholeWindow = starts.length;
      if (starts.length > 0) violations.push(`A1 browser sent ${starts.length} POST .../start`);
    }

    for (const page of journey.context?.pages() ?? []) await page.close();

    // Agents, as the member.
    for (const agent of journey.context ? journey.agents : []) {
      if (!(await agentGone(member(), agent.id))) {
        const res = await member().delete(`/api/v1/agents/${agent.id}`);
        cleanup[`agent ${agent.check}`] = `DELETE ${res.status()}`;
      }
      const gone = await expect
        .poll(() => agentGone(member(), agent.id), { timeout: 120_000 })
        .toBe(true)
        .then(
          () => true,
          () => false
        );
      cleanup[`agent ${agent.check} state`] = await agentReadState(member(), agent.id);
      if (!gone) residue.push(`agent ${agent.id} (${agent.check})`);
    }

    // The project, when this run created it.
    if (journey.projectCreated && journey.projectId) {
      const res = await member().delete(`/api/v1/projects/${journey.projectId}`);
      cleanup.project = `DELETE ${res.status()}`;
      const projectState = async (): Promise<ReadState> => {
        const get = await member().get(`/api/v1/projects/${journey.projectId}`);
        return classifyRead(get.status(), await get.json().catch(() => null), 'project');
      };
      const gone = await expect
        .poll(async () => isGone(await projectState()), { timeout: 60_000 })
        .toBe(true)
        .then(
          () => true,
          () => false
        );
      cleanup.projectState = await projectState();
      if (!gone) residue.push(`project ${journey.projectId}`);
    }

    // The fixture template (local hub).
    if (hub && journey.template) {
      const del = await adminRequest(hub, 'DELETE', `/api/v1/templates/${journey.template.id}`);
      cleanup.template = `DELETE ${del.status}`;
      const get = await adminRequest(hub, 'GET', `/api/v1/templates/${journey.template.id}`);
      const templateState = classifyRead(get.status, get.body, 'template');
      cleanup.templateState = templateState;
      if (!isGone(templateState)) residue.push(`template ${journey.template.name}`);
    }

    // The test identity (local hub). Without --purge the delete is refused
    // while the identity still owns agents or projects.
    if (hub && journey.identityId) {
      try {
        runScionCLI(hub, ['hub', 'test-identity', 'delete', journey.identityId]);
        cleanup.identity = 'deleted';
      } catch (err) {
        cleanup.identity = `delete failed: ${String((err as Error).message)}`;
      }
      if (journey.context) {
        const me = await journey.context.request.get('/auth/me', { maxRedirects: 0 });
        cleanup.authMeAfterDelete = me.status();
        if (me.status() !== 401) residue.push('test identity session still answers /auth/me');
        await me.dispose();
      }
      const listed = runScionCLI(hub, [
        'hub',
        'test-identity',
        'list',
        '--include-expired',
        '--json',
      ]);
      if (listed.includes(journey.identityId)) residue.push(`test identity ${journey.identityId}`);
    } else if (t?.mode === 'remote') {
      cleanup.identity =
        'remote target: NOT verified here; the runner deletes the identity out of band, ' +
        'and this evidence is written before that';
    }

    await journey.context?.close();
    if (journey.tmpDir) fs.rmSync(journey.tmpDir, { recursive: true, force: true });

    // Evidence only now, after the identity is deleted.
    journey.evidence.cleanup = { ...cleanup, residue, violations };
    const evidenceFile = testInfo.outputPath('async-create-evidence.json');
    fs.writeFileSync(evidenceFile, JSON.stringify(journey.evidence, null, 2));
    console.info(`[journey] evidence: ${evidenceFile}`);
    console.info(`[journey] cleanup: ${JSON.stringify(journey.evidence.cleanup)}`);

    expect(violations, 'A1: no POST .../start over the whole window').toEqual([]);
    expect(residue, 'A5: nothing left behind').toEqual([]);
  });
});
