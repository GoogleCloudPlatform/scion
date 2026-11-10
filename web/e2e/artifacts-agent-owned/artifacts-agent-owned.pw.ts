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
 * Access to an artifact OWNED BY AN AGENT, graded at the UI per role (R1-B),
 * end to end on a local throwaway hub restarted without development
 * sign-in (spec R1B-journey-spec rev 4).
 *
 * Five member test identities, each in its own browser context: powner
 * (creates the home project: project owner), aowner (project member; creates
 * the agent, so the delegating user), padmin (project admin), member and
 * outsider (no project). aowner creates a credential-free agent from the web
 * form, types into its shell through the keys operation, asserts the agent
 * token's artifact scopes with `scion whoami --full`, and publishes with
 * `scion artifact publish` (a fixture, not a graded UI row). Rows, in order:
 *
 * - R1B-P1..P8: STOP rows (test mode, no dev sign-in read back, issuer,
 *   identities, experiment, broker, agent, token scopes). A failed STOP row
 *   records a FINDING and every later row is "not run" with that cause; a
 *   failed P8 also marks the run INVALID. No workaround of any kind.
 * - R1B-01..10: the artifact page per role before and after a review grant.
 * - R1B-11..16: aowner deletes the agent; the owner pass marks the artifact
 *   (15 s polls, 15 min deadline, no trigger); the page per role after it.
 *   aowner's controls after the delete are observations (Q-A5 open).
 * - R1B-17/18 (afterAll): cleanup by absence, then the evidence.
 *
 * Every row records its UI observation and the API facts it read. The UI
 * observation decides the row, and the UI and the API must agree; an absent
 * (omitempty) capability flag counts as false. Rows are soft. Every row
 * carries the recorded deviation (state.ts DEVIATION). Where no container
 * runtime is usable, every agent row is a declared skip (the b02 gate tier).
 */

import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as path from 'node:path';

import { expect, request, test, type Page } from '@playwright/test';

import { assertTestIdentity, redeemTestIdentity } from '../harness/auth.js';
import { keepHubState, readHubState } from '../harness/hub.js';
import {
  createJourney,
  errorCode,
  isShown,
  type RowRecord,
} from '../artifacts-access/journey-core.js';
import {
  annotationsOf,
  classifyRead,
  firstLine,
  isGone,
  recordRequests,
  redactPath,
  secretLikeStrings,
} from '../artifacts-access/journey-lib.js';
import {
  SELECTORS as AA,
  anyPageHeading,
  artifactsRow,
  artifactsRowAccess,
  escapeRegExp,
  headerTitle,
  historyPublishedBy,
  historyRow,
  historyEditButton,
  notFoundHeading,
  ownerMeta,
  tab,
  uploadVersionButton,
} from '../artifacts-access/selectors.js';
import { markdownFrame } from '../artifacts-access/render-selectors.js';
import { parseTimeoutMs } from '../async-create/journey-lib.js';
import { SELECTORS as AC, confirmDialogButton, phaseBadge } from '../async-create/selectors.js';
import {
  ARTIFACT_KEY,
  ARTIFACT_TITLE,
  CAPTURE_COMMAND,
  FILE_DONE,
  READY,
  SHELL_COMMANDS,
  WHOAMI_DONE,
  markerSeen,
  missingScopes,
  shellPromptShown,
  parsePublish,
  parseScopes,
} from './agent-shell.js';
import {
  AGENT_ROW_IDS,
  ORACLE_ROW_MAP,
  ROWS,
  TITLE_CLEANUP,
  TITLE_JOURNEY,
  noRuntimeReason,
} from './rows.js';
import {
  AO_HEADER_CONTROLS,
  AO_SELECTORS as AO,
  deleteConfirmButton,
  headerControl,
  openDialogSection,
  openShareDialog,
  reviewAdd,
  reviewSuggestion,
  rowOwnerDeletedBadge,
  saveReviewButton,
  type AoHeaderControl,
} from './selectors.js';
import {
  DEVIATION,
  IDENTITY_KEYS,
  readSuiteState,
  type IdentityKey,
  type LocalSuiteState,
} from './state.js';

const ENTRY = 'r1b-agent.md';
const REVIEW_MARK = '{>>R1-B review comment<<}';
const LAUNCH_TIMEOUT = parseTimeoutMs(
  process.env.E2E_LAUNCH_TIMEOUT_MS,
  300_000,
  'E2E_LAUNCH_TIMEOUT_MS'
);
/** The owner pass wait (grader: 15 s polls, 15 min deadline, no trigger). */
const OWNER_CHECK_TIMEOUT = parseTimeoutMs(
  process.env.E2E_OWNER_CHECK_TIMEOUT_MS,
  900_000,
  'E2E_OWNER_CHECK_TIMEOUT_MS'
);
const OWNER_CHECK_POLL_MS = 15_000;
/** How long the agent's bash prompt gets to appear after the agent runs: 180 s. */
const SHELL_READY_TIMEOUT = 180_000;
/**
 * How long each typed command's marker gets, once the prompt is up: 60 s.
 * Each command is one short line (printf, mkdir, whoami, one small-file
 * publish) whose marker shows within seconds; 60 s is a wide margin and keeps
 * the suite's worst case inside the lane's single-attempt budget (lane run
 * sheet, step 6 floor).
 */
const SHELL_COMMAND_TIMEOUT = 60_000;

const journey = createJourney<IdentityKey>({
  keys: IDENTITY_KEYS,
  rows: ROWS,
  remotePrefix: 'E2E_AO',
  purpose: 'e2e agent-owned artifact',
});
const { state } = journey;
const { api, row, rowRecord, notRun, finding, pageOf, who } = journey;

/** One artifact page snapshot: what the page showed. */
interface Snapshot {
  t: number;
  as: IdentityKey;
  step: string;
  path: string;
  rendered404: boolean;
  headerRendered: boolean;
  title: string | null;
  owner: string | null;
  ownerGone: string | null;
  ownerDeletedBadge: boolean;
  reviewPending: boolean;
  versionLabel: string | null;
  previewRendered: boolean;
  visible: AoHeaderControl[];
  hidden: AoHeaderControl[];
  uploadVersion: boolean | null;
  historyEdit: boolean | null;
  historyV1By: string | null;
}

/** The capability facts of GET /api/v1/artifacts/{id}; absent flags read as false. */
interface Caps {
  status: number;
  canPublish: boolean;
  canReview: boolean;
  canManage: boolean;
  canShare: boolean;
  canGrantReview: boolean;
  canRetire: boolean;
  shareAuthority: string | null;
  ownerDeleted: boolean;
  ownerKind: string | null;
  ownerRef: string | null;
  scopeRef: string | null;
  currentSeq: number | null;
  /** artifact.expiresAt as returned (null when absent). */
  expiresAt: unknown;
  /** The flag keys exactly as returned (raw fact). */
  raw: Record<string, unknown>;
}

const suite = {
  local: undefined as LocalSuiteState | undefined,
  agent: undefined as { id: string; name: string } | undefined,
  agentDeleted: false,
  artifact: undefined as { id: string } | undefined,
  artifactDeleted: false,
  snapshots: [] as Snapshot[],
  shell: { sent: [] as unknown[], captures: [] as unknown[] },
  ownerPolls: [] as Array<{ t: number; status: number; body: unknown }>,
  grantsAfterReview: null as Array<{ subjectRef: string; permission: string }> | null,
  /** artifact.expiresAt as powner read it in R1B-10 (raw); R1B-14 compares each role's read with it. */
  expiresAtBefore: undefined as unknown,
  invalid: null as string | null,
};

function local(): LocalSuiteState {
  if (!suite.local) throw new Error('no suite state; did global-setup run?');
  return suite.local;
}

function suffix(): string {
  return crypto.randomBytes(3).toString('hex');
}

function artifactId(): string {
  if (!suite.artifact) throw new Error('no artifact yet');
  return suite.artifact.id;
}

function projectId(): string {
  if (!state.projectId) throw new Error('no project yet');
  return state.projectId;
}

function artifactPath(id = artifactId()): string {
  return `/projects/${projectId()}/artifacts/${id}`;
}

/** As principal-names.ts shortId. */
function shortId(ref: string): string {
  return ref.length > 12 ? `${ref.slice(0, 8)}…` : ref;
}

/** Records a STOP: a FINDING, and every later row "not run" with the cause. */
function stop(id: string, text: string): void {
  finding(id, text);
  state.stopped ??= `not run: STOP at ${id} (${text})`;
}

/** After a STOP row: stop the run when it did not pass. */
function stopUnlessPassed(id: string, passed: boolean): void {
  if (passed) return;
  const r = state.rows[id];
  if (r?.status === 'not run') return;
  stop(id, r?.errors[0] ?? 'failed');
}

/** The declared skip of an agent row where no runtime is usable. */
function agentPre(): string | undefined {
  const l = suite.local;
  if (l && !l.launchCapable) return noRuntimeReason(l.launchCapableReason);
  return undefined;
}

/** The capability facts as key. */
async function caps(key: IdentityKey, rec: RowRecord, label: string = key): Promise<Caps> {
  const res = await api(key, 'GET', `/api/v1/artifacts/${artifactId()}`, undefined, rec);
  const b = (res.body ?? {}) as Record<string, unknown> & {
    artifact?: Record<string, unknown>;
  };
  const a = b.artifact ?? {};
  const raw: Record<string, unknown> = {};
  for (const k of [
    'canPublish',
    'canReview',
    'canManage',
    'canShare',
    'canGrantReview',
    'canRetire',
    'shareAuthority',
  ]) {
    if (k in b) raw[k] = b[k];
  }
  if ('ownerDeleted' in a) raw.ownerDeleted = a.ownerDeleted;
  const out: Caps = {
    status: res.status,
    canPublish: b.canPublish === true,
    canReview: b.canReview === true,
    canManage: b.canManage === true,
    canShare: b.canShare === true,
    canGrantReview: b.canGrantReview === true,
    canRetire: b.canRetire === true,
    shareAuthority: typeof b.shareAuthority === 'string' ? b.shareAuthority : null,
    ownerDeleted: a.ownerDeleted === true,
    ownerKind: typeof a.ownerKind === 'string' ? a.ownerKind : null,
    ownerRef: typeof a.ownerRef === 'string' ? a.ownerRef : null,
    scopeRef: typeof a.scopeRef === 'string' ? a.scopeRef : null,
    currentSeq: typeof a.currentSeq === 'number' ? a.currentSeq : null,
    expiresAt: 'expiresAt' in a ? a.expiresAt : null,
    raw,
  };
  rec.ui[`${label}Caps`] = out;
  return out;
}

/**
 * The owner label the page must show to key, from key's own agent lookup,
 * read the way principal-names.ts reads it: the top-level displayName,
 * name or slug of a 200, else the short id. Recorded as a raw fact.
 */
async function expectedOwnerLabel(key: IdentityKey, rec: RowRecord): Promise<string> {
  const agentId = suite.agent!.id;
  const res = await api(key, 'GET', `/api/v1/agents/${agentId}`, undefined, rec);
  const b = (res.body ?? {}) as { displayName?: string; name?: string; slug?: string };
  const name = res.status === 200 ? b.displayName || b.name || b.slug || '' : '';
  const shown = `${name || shortId(agentId)} (agent)`;
  rec.ui[`${key}AgentLookup`] = { status: res.status, name: name || null, expected: shown };
  return shown;
}

/**
 * Opens the artifact page (or another path) as key and records what it
 * shows. Reads only after a positive anchor: the 404 heading, or the
 * header title plus the Version menu plus the markdown preview, so a page
 * that never rendered can never read as "hidden".
 */
async function snapshot(
  key: IdentityKey,
  step: string,
  opts: { history?: boolean; path?: string } = {}
): Promise<Snapshot> {
  const page = await pageOf(key);
  await page.goto(opts.path ?? artifactPath(), { waitUntil: 'domcontentloaded' });
  await anyPageHeading(page).waitFor({ state: 'visible' });
  const headerRendered = await isShown(headerTitle(page));
  const rendered404 = await isShown(notFoundHeading(page));
  let previewRendered = false;
  if (headerRendered) {
    await page.locator(AA.versionMenu).waitFor({ state: 'visible' });
    await page.locator(AA.markdownFrame).waitFor({ state: 'attached' });
    // Observed, not assumed: the fixture's heading rendered inside the markdown frame.
    previewRendered = await markdownFrame(page)
      .locator('h1', { hasText: ARTIFACT_TITLE })
      .waitFor({ state: 'visible' })
      .then(() => true)
      .catch(() => false);
  }
  const visible: AoHeaderControl[] = [];
  const hidden: AoHeaderControl[] = [];
  for (const c of AO_HEADER_CONTROLS) {
    if (await isShown(headerControl(page, c))) visible.push(c);
    else hidden.push(c);
  }
  let uploadVersion: boolean | null = null;
  let historyEdit: boolean | null = null;
  let historyV1By: string | null = null;
  if (opts.history && headerRendered) {
    await tab(page, 'history').click();
    await historyRow(page, 1).waitFor({ state: 'visible' });
    uploadVersion = await isShown(uploadVersionButton(page));
    historyEdit = await isShown(historyEditButton(page));
    historyV1By = ((await historyPublishedBy(historyRow(page, 1)).textContent()) ?? '')
      .replace(/\s+/g, ' ')
      .trim();
    await tab(page, 'preview').click();
  }
  const text = async (sel: string): Promise<string | null> =>
    (await page.locator(sel).count()) > 0
      ? ((await page.locator(sel).first().textContent()) ?? '').replace(/\s+/g, ' ').trim()
      : null;
  const snap: Snapshot = {
    t: Date.now(),
    as: key,
    step,
    path: redactPath(new URL(page.url()).pathname),
    rendered404,
    headerRendered,
    title: headerRendered ? ((await headerTitle(page).textContent()) ?? '').trim() : null,
    owner: headerRendered
      ? ((await ownerMeta(page).textContent()) ?? '').replace(/\s+/g, ' ').trim()
      : null,
    ownerGone: headerRendered ? await text(AO.ownerGone) : null,
    ownerDeletedBadge: headerRendered ? await isShown(page.locator(AO.ownerDeletedBadge)) : false,
    reviewPending: headerRendered
      ? await isShown(page.locator(AO.reviewPending).filter({ hasText: 'Review pending' }))
      : false,
    versionLabel: headerRendered ? await text(AA.versionMenu) : null,
    previewRendered,
    visible,
    hidden,
    uploadVersion,
    historyEdit,
    historyV1By,
  };
  suite.snapshots.push(snap);
  return snap;
}

/** Soft checks of a snapshot against the visible and hidden control sets. */
function expectControls(
  snap: Snapshot,
  want: { visible: AoHeaderControl[]; hidden: AoHeaderControl[]; uploadVersion?: boolean }
): void {
  expect.soft(snap.headerRendered, `${snap.as}: artifact header rendered`).toBe(true);
  expect.soft(snap.rendered404, `${snap.as}: no 404 page`).toBe(false);
  for (const c of want.visible) expect.soft(snap.visible, `${snap.as}: ${c} visible`).toContain(c);
  for (const c of want.hidden) expect.soft(snap.hidden, `${snap.as}: ${c} hidden`).toContain(c);
  if (want.uploadVersion !== undefined) {
    expect.soft(snap.uploadVersion, `${snap.as}: Upload new version`).toBe(want.uploadVersion);
  }
}

/** The page's owner label settles once names resolve: poll it to the expected text. */
async function expectOwnerLabel(key: IdentityKey, label: string, rec: RowRecord): Promise<void> {
  const page = await pageOf(key);
  await expect
    .soft(ownerMeta(page), `${key}: owner label`)
    .toHaveText(new RegExp(`^\\s*Owner:\\s*${escapeRegExp(label)}\\s*$`));
  rec.ui[`${key}OwnerLabel`] = ((await ownerMeta(page).textContent()) ?? '')
    .replace(/\s+/g, ' ')
    .trim();
}

/** Opens /artifacts as key and returns the artifact's row (access badge, owner-deleted badge), or null. */
async function listRow(
  key: IdentityKey
): Promise<{ access: string; ownerDeleted: boolean } | null> {
  const page = await pageOf(key);
  const listed = page.waitForResponse(
    (res) => res.request().method() === 'GET' && new URL(res.url()).pathname === '/api/v1/artifacts'
  );
  await page.goto('/artifacts', { waitUntil: 'domcontentloaded' });
  expect.soft((await listed).status(), `${key}: /artifacts list status`).toBe(200);
  await page
    .locator(`${AA.artifactsRow}, ${AA.artifactsEmpty}`)
    .first()
    .waitFor({ state: 'visible' });
  const r = artifactsRow(page, ARTIFACT_TITLE);
  if ((await r.count()) === 0) return null;
  return {
    access: ((await artifactsRowAccess(r).textContent()) ?? '').replace(/\s+/g, ' ').trim(),
    ownerDeleted: await isShown(rowOwnerDeletedBadge(r)),
  };
}

/** The non-disclosure check: the page and reads as for a missing artifact (status and code). */
async function expectSameAsMissing(key: IdentityKey, step: string, rec: RowRecord): Promise<void> {
  const real = await snapshot(key, step);
  const missing = crypto.randomUUID();
  const none = await snapshot(key, `${step} (missing id)`, { path: artifactPath(missing) });
  rec.ui.snapshot = real;
  rec.ui.missingSnapshot = none;
  for (const s of [real, none]) {
    expect.soft(s.rendered404, `${s.step}: 404 page`).toBe(true);
    expect.soft(s.headerRendered, `${s.step}: no artifact header`).toBe(false);
    expect.soft(s.visible, `${s.step}: no header controls`).toEqual([]);
  }
  const pairs: Array<Record<string, unknown>> = [];
  for (const sub of ['', '/versions', `/versions/1/files/${ENTRY}`]) {
    const a = await api(key, 'GET', `/api/v1/artifacts/${artifactId()}${sub}`, undefined, rec);
    const b = await api(key, 'GET', `/api/v1/artifacts/${missing}${sub}`, undefined, rec);
    pairs.push({ sub, real: [a.status, a.code], missing: [b.status, b.code] });
    expect.soft(a.status, `GET artifact${sub}`).toBe(404);
    expect
      .soft([a.status, a.code], `same answer as missing for ${sub || 'the artifact'}`)
      .toEqual([b.status, b.code]);
  }
  rec.ui.compared = pairs;
  const row = await listRow(key);
  rec.ui.listRow = row;
  expect.soft(row, `${key}: no /artifacts row`).toBeNull();
}

/** The grants as powner (a steward may list them): subject and permission. */
async function grantsAsPowner(
  rec: RowRecord
): Promise<Array<{ subjectRef: string; permission: string }>> {
  const res = await api(
    'powner',
    'GET',
    `/api/v1/artifacts/${artifactId()}/grants`,
    undefined,
    rec
  );
  expect.soft(res.status, 'powner lists the grants').toBe(200);
  const items =
    ((res.body ?? {}) as { grants?: Array<{ subjectRef: string; permission: string }> }).grants ??
    [];
  return items.map((g) => ({ subjectRef: g.subjectRef, permission: g.permission }));
}

/**
 * Types a query into the Grant review access dialog's people input and
 * returns the people-search answer for exactly that query (status, and
 * whether the user id is in it).
 */
async function searchReviewPeople(
  page: Page,
  query: string,
  userId: string
): Promise<{ status: number; inAnswer: boolean }> {
  const answer = page.waitForResponse((res) => {
    if (res.request().method() !== 'GET') return false;
    const u = new URL(res.url());
    return u.pathname === '/api/v1/users' && u.searchParams.get('search') === query;
  });
  await page.locator(AO.reviewUserInput).fill(query);
  const res = await answer;
  const body = (await res.json().catch(() => ({}))) as { users?: Array<{ id: string }> };
  return { status: res.status(), inAnswer: (body.users ?? []).some((u) => u.id === userId) };
}

// ---- Agent shell (fixture F3) ------------------------------------------

/** Types text into the agent's shell as aowner (keys operation), then Enter. */
async function typeLine(step: string, text: string): Promise<void> {
  const agentId = suite.agent!.id;
  for (const keys of [text, 'Enter']) {
    const res = await api('aowner', 'POST', `/api/v1/agents/${agentId}/keys`, { keys });
    suite.shell.sent.push({ t: Date.now(), step, keys, status: res.status, code: res.code });
    if (res.status !== 200) {
      throw new Error(`keys (${step}) answered ${res.status} ${String(res.code)}`);
    }
  }
}

/** The terminal capture as aowner (the route `scion look` uses), kept after a secret check. */
async function capture(step: string): Promise<string> {
  const res = await api('aowner', 'POST', `/api/v1/agents/${suite.agent!.id}/exec`, {
    command: CAPTURE_COMMAND,
    timeout: 10,
  });
  const output = ((res.body ?? {}) as { output?: string }).output ?? '';
  const leaks = secretLikeStrings(output);
  suite.shell.captures.push({
    t: Date.now(),
    step,
    status: res.status,
    code: res.code,
    output: leaks.length === 0 ? output : `[withheld: ${leaks.length} secret-shaped string(s)]`,
  });
  return output;
}

/** Polls the capture (every 3 s, one deadline) until done(capture) holds; returns that capture. */
async function waitCapture(
  step: string,
  done: (c: string) => boolean,
  timeout: number
): Promise<string> {
  let last = '';
  await expect
    .poll(
      async () => {
        last = await capture(step);
        return done(last);
      },
      {
        message: `${step}: the agent shell shows the expected output`,
        intervals: [3_000],
        timeout,
      }
    )
    .toBe(true);
  return last;
}

/**
 * Waits for the agent's bash prompt before typing anything (keys sent
 * before the shell exists would be lost), then types the ready marker once
 * and waits for it. No retype.
 */
async function waitShellReady(): Promise<void> {
  // One deadline; no prompt by then is a STOP of the run (the caller records it as F3 with a FINDING).
  try {
    await waitCapture('prompt', shellPromptShown, SHELL_READY_TIMEOUT);
  } catch {
    throw new Error(
      `STOP: no bash prompt in the agent's terminal within ${SHELL_READY_TIMEOUT} ms`
    );
  }
  await typeLine('ready', SHELL_COMMANDS.ready);
  await waitCapture('ready', (c) => markerSeen(c, READY), SHELL_COMMAND_TIMEOUT);
}

// ---- The journey -------------------------------------------------------

test.describe.serial('agent-owned artifact access per role (R1-B)', () => {
  // eslint-disable-next-line no-empty-pattern -- Playwright needs the fixtures argument
  test.afterEach(({}, testInfo) => {
    if (testInfo.title === TITLE_JOURNEY && testInfo.error?.message) {
      state.testError ??= firstLine(testInfo.error.message);
    }
    journey.recordResult(testInfo);
  });

  test.beforeAll(async ({ browser }) => {
    journey.seedRows();
    try {
      const l = readSuiteState();
      if (!l) throw new Error('no suite state; did global-setup run?');
      suite.local = l;
      const hub = readHubState(l.hubStateFile);
      if (!hub) throw new Error(`no hub state at ${l.hubStateFile}`);
      state.target = { mode: 'local', baseURL: l.baseURL, hub };
      state.evidence.oracleRowMap = ORACLE_ROW_MAP;
      state.evidence.target = {
        mode: 'local',
        baseURL: l.baseURL,
        hubId: l.hubId,
        devAuth: hub.devAuth ?? null,
        deviation: DEVIATION,
        launchCapable: l.launchCapable,
        launchCapableReason: l.launchCapableReason,
      };
      state.evidence.fixture = {
        harnessConfig: l.harnessConfig,
        expectedAgentImage: l.agentImage,
        pinnedAgentImage: l.pinnedAgentImage,
        runningPath: 'no-auth drop-to-shell (no harness credentials; credential guard passed)',
        shellDriver: 'keys operation plus terminal capture, as aowner (Q-A1)',
      };

      // P4: redeem each identity's single-use code in its own context.
      for (const key of IDENTITY_KEYS) {
        const issued = l.identities[key];
        const context = await browser.newContext({ baseURL: l.baseURL });
        const requests = recordRequests(context);
        await redeemTestIdentity(context, { codeFile: issued.codeFile });
        await assertTestIdentity(context.request, { email: issued.email, role: 'member' });
        const me = await context.request.get('/api/v1/auth/me');
        const meBody = (await me.json()) as { id?: string; displayName?: string };
        await me.dispose();
        if (!meBody.id) throw new Error(`no user id for identity ${key}`);
        state.ids[key] = {
          key,
          id: meBody.id,
          email: issued.email,
          displayName: issued.displayName || meBody.displayName || '',
          context,
          requests,
        };
      }
    } catch (err) {
      state.setupError = `beforeAll: ${firstLine(err)}`;
      throw err;
    }
  });

  test(TITLE_JOURNEY, async ({ browser }) => {
    test.setTimeout(LAUNCH_TIMEOUT + OWNER_CHECK_TIMEOUT + 25 * 60_000);
    const l = local();

    // ---- Preconditions (STOP rows) -------------------------------------
    stopUnlessPassed(
      'R1B-P1',
      await row('R1B-P1', async (r) => {
        const anon = await request.newContext({ baseURL: l.baseURL });
        try {
          const res = await anon.get('/api/v1/test-infra/status', { maxRedirects: 0 });
          const body = (await res.json().catch(() => null)) as Record<string, unknown> | null;
          r.ui.testInfraStatus = { status: res.status(), body };
          expect(res.status(), 'test-infra status answers').toBe(200);
          expect.soft(body?.testIdentities, 'testIdentities').toBe(true);
          expect.soft(body?.testIdentityWebSessions, 'testIdentityWebSessions').toBe(true);
          expect.soft(body?.testHubAdmin, 'testHubAdmin').toBe(false);
        } finally {
          await anon.dispose();
        }
        r.ui.startFlags = l.startFlags;
        r.ui.hubId = l.hubId;
        // Class test is not on the wire: --test-mode=member refuses to start
        // without class test and the marker for the pinned hub id, so a hub
        // answering testIdentities true carries both (testIdentityWebSessions
        // true also needs the marker's web session key).
        r.ui.classAndMarker =
          'implied: the harness starts --test-mode=member:<hub id> with SCION_SERVER_HUB_CLASS=test ' +
          'on a database marked for that hub id; the hub refuses that start otherwise';
        expect.soft(l.startFlags, 'started with --test-mode').toContain('--test-mode');
        const banners: Record<string, boolean> = {};
        for (const key of IDENTITY_KEYS) {
          const page = await pageOf(key);
          await page.goto('/', { waitUntil: 'domcontentloaded' });
          banners[key] = await page
            .locator(AO.testHubBanner)
            .first()
            .waitFor({ state: 'visible' })
            .then(() => true)
            .catch(() => false);
        }
        r.ui.testHubBanner = banners;
        for (const key of IDENTITY_KEYS) {
          expect.soft(banners[key], `${key}: test-hub banner visible`).toBe(true);
        }
      })
    );

    stopUnlessPassed(
      'R1B-P2',
      await row('R1B-P2', async (r) => {
        r.ui.readBack = l.p2;
        expect.soft(l.p2.devAuthAfterRestart, 'harness state devAuth').toBe(false);
        expect
          .soft(l.p2.oldDevTokenAuthMe.status, 'the old dev token is refused on /api/v1/auth/me')
          .toBe(401);
        expect
          .soft(l.p2.oldDevTokenProjects.status, 'the old dev token is refused on /api/v1/projects')
          .toBe(401);
        const context = await browser.newContext({ baseURL: l.baseURL });
        try {
          const me = await context.request.get('/api/v1/auth/me', { maxRedirects: 0 });
          r.ui.anonymousApiAuthMe = me.status();
          expect.soft(me.status(), 'no session: /api/v1/auth/me').toBe(401);
          await me.dispose();
          const page = await context.newPage();
          await page.goto('/', { waitUntil: 'domcontentloaded' });
          await page.waitForLoadState('domcontentloaded');
          // The login page or the 401 page (spec P2); which one is recorded.
          await expect(
            page.locator(`${AO.loginPage}, ${AO.unauthorizedPage}`).first(),
            'no session: a login or 401 page'
          ).toBeAttached();
          const landed = new URL(page.url()).pathname;
          r.ui.anonymousLanding = landed;
          const landing =
            (await page.locator(AO.unauthorizedPage).count()) > 0 ? '401 page' : 'login page';
          r.ui.anonymousLandingPage = landing;
          // The login page is a standalone route without navigation; a 401 page renders inside the app
          // shell, which always has the navigation, so the check applies to the login landing only.
          if (landing === 'login page') {
            expect.soft(await page.locator('scion-nav').count(), 'no signed-in navigation').toBe(0);
          }
          const after = await context.request.get('/auth/me', { maxRedirects: 0 });
          r.ui.anonymousAuthMeAfterLanding = after.status();
          expect.soft(after.status(), 'still not signed in after the landing').toBe(401);
          await after.dispose();
        } finally {
          await context.close();
        }
      })
    );

    stopUnlessPassed(
      'R1B-P3',
      await row('R1B-P3', (r) => {
        r.ui.issuer = 'local hub issuer before the restart (recorded deviation)';
        r.ui.issued = Object.fromEntries(
          IDENTITY_KEYS.map((key) => [
            key,
            {
              id: l.identities[key].id,
              expiresAt: l.identities[key].expiresAt,
              codeAgeAtRestartMs: l.p2.restartedAt - l.identities[key].codeMintedAt,
            },
          ])
        );
        for (const key of IDENTITY_KEYS) {
          expect.soft(l.identities[key].id, `${key} issued`).not.toBe('');
        }
      })
    );

    stopUnlessPassed(
      'R1B-P4',
      await row('R1B-P4', async (r) => {
        const checks: Record<string, unknown> = {};
        for (const key of IDENTITY_KEYS) {
          const identity = who(key);
          const info = await assertTestIdentity(identity.context.request, {
            email: identity.email,
            role: 'member',
          });
          checks[key] = { id: identity.id, role: info.role, expiresAt: info.expiresAt };
          expect
            .soft(identity.id, `${key} id matches the issued identity`)
            .toBe(l.identities[key].id);
        }
        r.ui.sessions = checks;
      })
    );

    stopUnlessPassed(
      'R1B-P5',
      await row('R1B-P5', async (r) => {
        const res = await api('aowner', 'GET', '/api/v1/artifacts?mine=1', undefined, r);
        r.ui.experimentAfterEnable = l.experimentAfterEnable;
        expect.soft(res.status, 'GET /api/v1/artifacts as aowner').toBe(200);
        const page = await pageOf('aowner');
        await page.goto('/', { waitUntil: 'domcontentloaded' });
        r.ui.navArtifacts = await page
          .locator(AA.navArtifacts)
          .filter({ visible: true })
          .first()
          .waitFor({ state: 'visible' })
          .then(() => true)
          .catch(() => false);
        expect.soft(r.ui.navArtifacts, 'sidebar shows Artifacts').toBe(true);
      })
    );

    // ---- F1: project and memberships (fixtures, not graded) -------------
    const memberships: Record<string, unknown> = {};
    state.evidence.membershipFixtures = memberships;
    if (!state.stopped) {
      try {
        const res = await api('powner', 'POST', '/api/v1/projects', {
          name: `e2e-r1b-home-${suffix()}`,
        });
        if (res.status !== 201) throw new Error(`powner creates the project: ${res.status}`);
        state.projectId = (res.body as { id: string }).id;
        const roles = await api(
          'powner',
          'GET',
          `/api/v1/projects/${state.projectId}/members/assignable-roles`
        );
        const items =
          ((roles.body ?? {}) as { items?: Array<{ id: string; name: string }> }).items ?? [];
        for (const [key, roleName] of [
          ['aowner', 'project-member'],
          ['member', 'project-member'],
          ['padmin', 'project-admin'],
        ] as const) {
          const role = items.find((x) => x.name === roleName);
          if (!role) throw new Error(`no assignable role ${roleName} (${roles.status})`);
          const add = await api('powner', 'POST', `/api/v1/projects/${state.projectId}/members`, {
            roleDefinitionId: role.id,
            principalType: 'user',
            principalId: who(key).email,
          });
          memberships[key] = { role: roleName, status: add.status, code: add.code };
          if (add.status !== 201 && add.status !== 200) {
            throw new Error(`add ${key} as ${roleName}: ${add.status} ${String(add.code)}`);
          }
          state.bindings.push({
            projectId: state.projectId,
            key,
            id: (add.body as { id: string }).id,
          });
        }
        state.evidence.projects = { home: { id: state.projectId, createdBy: 'powner' } };
      } catch (err) {
        stop('F1', `project and memberships could not be set up: ${firstLine(err)}`);
      }
    }
    state.evidence.identities = Object.fromEntries(
      IDENTITY_KEYS.map((key) => [
        key,
        {
          id: state.ids[key]?.id ?? null,
          role: {
            powner: 'creates the home project (project owner)',
            aowner: 'home project-member; creates the agent (delegating user)',
            padmin: 'home project-admin',
            member: 'home project-member; later the review grantee',
            outsider: 'no project, no grant',
          }[key],
        },
      ])
    );

    // ---- Agent rows: declared skips without a runtime --------------------
    const noRuntime = agentPre();
    if (noRuntime) {
      notRun(AGENT_ROW_IDS, noRuntime);
      test.info().annotations.push({ type: 'NOT RUN', description: noRuntime });
      return;
    }

    stopUnlessPassed(
      'R1B-P6',
      await row('R1B-P6', async (r) => {
        r.ui.launchCapable = { capable: l.launchCapable, reason: l.launchCapableReason };
        const res = await api(
          'powner',
          'GET',
          `/api/v1/projects/${projectId()}/providers`,
          undefined,
          r
        );
        const b = (res.body ?? {}) as { providers?: unknown[]; items?: unknown[] };
        const providers = b.providers ?? b.items ?? [];
        r.ui.providers = providers;
        expect.soft(res.status, 'providers list').toBe(200);
        expect.soft(providers.length, 'a provider broker serves the project').toBeGreaterThan(0);
      })
    );

    stopUnlessPassed(
      'R1B-P7',
      await row('R1B-P7', async (r) => {
        const page = await pageOf('aowner');
        const name = `r1b-pub-${suffix()}`;
        await page.goto(`/agents/new?projectId=${encodeURIComponent(projectId())}`, {
          waitUntil: 'domcontentloaded',
        });
        const form = page.locator(AC.createPage);
        await expect(form.locator(AC.createName)).toBeVisible();
        await form.locator(AC.createNameInput).fill(name);
        const harness = form.locator(AC.createHarness);
        await harness.click();
        await harness.locator(`${AC.selectOption}[value="${l.harnessConfig}"]`).first().click();
        await expect(harness).toHaveJSProperty('value', l.harnessConfig);
        await form.locator(AC.createTaskInput).fill('provision only');
        if (l.pinnedAgentImage) {
          // A pinned image (E2E_AGENT_IMAGE, e.g. by digest): the form's Container image field (config.image).
          await form.locator(AC.createAdvancedHeader).click();
          const imageInput = form.locator(AC.createImageInput);
          await expect(imageInput).toBeVisible();
          await imageInput.fill(l.pinnedAgentImage);
        }
        const created = page.waitForResponse(
          (res) =>
            res.request().method() === 'POST' &&
            /^\/api\/v1\/(projects\/[^/]+\/)?agents$/.test(new URL(res.url()).pathname)
        );
        await form.locator(AC.createSubmit).click();
        const res = await created;
        const body = (await res.json().catch(() => ({}))) as { agent?: { id?: string } };
        r.api.push({
          as: 'aowner',
          method: 'POST',
          path: redactPath(new URL(res.url()).pathname),
          status: res.status(),
          code: null,
        });
        expect(res.status(), 'agent create').toBe(201);
        const id = body.agent?.id ?? '';
        expect(id, 'agent id').not.toBe('');
        suite.agent = { id, name };
        await expect(phaseBadge(page), 'agent reaches running').toHaveAttribute(
          'status',
          'running',
          { timeout: LAUNCH_TIMEOUT }
        );
        r.ui.phase = 'running';
        const read = await api('aowner', 'GET', `/api/v1/agents/${id}`, undefined, r);
        type AgentView = {
          createdBy?: string;
          ownerId?: string;
          projectId?: string;
          appliedConfig?: {
            noAuth?: boolean;
            harnessAuth?: string;
            harnessConfig?: string;
            image?: string;
          };
        };
        const outer = (read.body ?? {}) as AgentView & { agent?: AgentView };
        const agent = outer.agent ?? outer;
        r.ui.agent = {
          id,
          name,
          createdBy: agent.createdBy ?? null,
          ownerId: agent.ownerId ?? null,
          projectId: agent.projectId ?? null,
          appliedConfig: agent.appliedConfig ?? null,
        };
        state.evidence.agent = r.ui.agent;
        const aowner = who('aowner');
        expect.soft([aowner.id, aowner.email], 'created by aowner').toContain(agent.createdBy);
        expect.soft(agent.projectId, 'agent project').toBe(projectId());
        expect.soft(agent.appliedConfig?.noAuth, 'started without credentials').toBe(true);
        if (l.pinnedAgentImage) {
          expect.soft(agent.appliedConfig?.image, 'runs the pinned image').toBe(l.pinnedAgentImage);
        }
      })
    );

    // F3: the shell, then P8 before any publish.
    let whoCapture = '';
    if (!state.stopped) {
      try {
        await waitShellReady();
        await typeLine('file', SHELL_COMMANDS.file);
        await waitCapture('file', (c) => markerSeen(c, FILE_DONE), SHELL_COMMAND_TIMEOUT);
        await typeLine('whoami', SHELL_COMMANDS.whoami);
        whoCapture = await waitCapture(
          'whoami',
          (c) => markerSeen(c, WHOAMI_DONE),
          SHELL_COMMAND_TIMEOUT
        );
      } catch (err) {
        stop('F3', `the agent shell could not be driven: ${firstLine(err)}`);
      }
    }
    const p8 = await row('R1B-P8', (r) => {
      const scopes = parseScopes(whoCapture);
      const missing = missingScopes(scopes);
      r.ui.scopesLine = scopes;
      r.ui.missing = missing;
      expect(scopes, 'whoami printed a Scopes line').not.toBeNull();
      expect(missing, 'both artifact scopes in the agent token').toEqual([]);
    });
    if (!p8 && state.rows['R1B-P8']?.status === 'failed') {
      suite.invalid = 'R1B-P8: the agent token lacks an artifact scope before publish';
      stop('R1B-P8', `${suite.invalid}; no workaround`);
    }

    // F3/F4 and R1B-01: the publish from inside the agent.
    stopUnlessPassed(
      'R1B-01',
      await row('R1B-01', async (r) => {
        await typeLine('publish', SHELL_COMMANDS.publish);
        const out = await waitCapture(
          'publish',
          (c) => parsePublish(c).exit !== null,
          SHELL_COMMAND_TIMEOUT
        );
        const pub = parsePublish(out);
        r.ui.publish = pub;
        if (pub.exit !== 0) {
          throw new Error(`STOP: scion artifact publish exited ${String(pub.exit)}; no workaround`);
        }
        expect(pub.id, 'the publish printed a scion://artifact reference').not.toBeNull();
        suite.artifact = { id: pub.id! };
        const c = await caps('aowner', r);
        expect.soft(c.status, 'aowner reads the artifact').toBe(200);
        expect.soft(c.ownerKind, 'owned by an agent').toBe('agent');
        expect.soft(c.ownerRef, 'owned by this agent').toBe(suite.agent!.id);
        expect.soft(c.scopeRef, 'homed in the project').toBe(projectId());
        expect.soft(c.currentSeq, 'v1').toBe(1);
        const list = await api('aowner', 'GET', '/api/v1/artifacts?mine=1', undefined, r);
        const item = (
          (list.body ?? {}) as { artifacts?: Array<{ id: string; key?: string; access?: string }> }
        ).artifacts?.find((a) => a.id === pub.id);
        r.ui.listItem = item ?? null;
        expect.soft(item?.key, 'listed with its key').toBe(ARTIFACT_KEY);
        const listed = await listRow('aowner');
        r.ui.listRow = listed;
        expect.soft(listed, 'aowner: the artifact is listed in /artifacts').not.toBeNull();
        const snap = await snapshot('aowner', 'R1B-01');
        r.ui.snapshot = snap;
        expect.soft(snap.title, 'title').toBe(ARTIFACT_TITLE);
        expect.soft(snap.previewRendered, 'markdown preview rendered').toBe(true);
      })
    );

    // ---- Before any grant, agent live ------------------------------------
    await row('R1B-02', async (r) => {
      const snap = await snapshot('aowner', 'R1B-02', { history: true });
      r.ui.snapshot = snap;
      expectControls(snap, {
        visible: ['Grant review access', 'Retention…', 'Delete…', 'Download'],
        hidden: ['Edit', 'Review', 'Share', 'Move…'],
        uploadVersion: false,
      });
      expect.soft(snap.historyEdit, 'History Edit hidden').toBe(false);
      // The creator can read its agent, so the frozen label is the agent's name (spec R1B-02), never a short id.
      const aownerLabel = await expectedOwnerLabel('aowner', r);
      expect
        .soft((r.ui.aownerAgentLookup as { status: number }).status, 'aowner can read its agent')
        .toBe(200);
      expect
        .soft(aownerLabel, 'the owner label names the agent')
        .toBe(`${suite.agent!.name} (agent)`);
      await expectOwnerLabel('aowner', aownerLabel, r);
      const c = await caps('aowner', r);
      expect.soft(c.canPublish, 'canPublish').toBe(false);
      expect.soft(c.canReview, 'canReview').toBe(false);
      expect.soft(c.canManage, 'canManage').toBe(false);
      expect.soft(c.canGrantReview, 'canGrantReview').toBe(true);
      expect.soft(c.canShare, 'canShare').toBe(true);
      expect.soft(c.shareAuthority, 'shareAuthority').toBe('delegator');
      expect.soft(c.canRetire, 'canRetire').toBe(true);
      expect.soft(c.ownerDeleted, 'ownerDeleted').toBe(false);
      const listed = await listRow('aowner');
      r.ui.listRow = listed;
      expect
        .soft(listed?.access, 'aowner: /artifacts row badged Project (not Owned)')
        .toBe('Project');
    });

    await row('R1B-03', async (r) => {
      const snap = await snapshot('powner', 'R1B-03', { history: true });
      r.ui.snapshot = snap;
      expectControls(snap, {
        visible: ['Grant review access', 'Retention…', 'Delete…', 'Download'],
        hidden: ['Edit', 'Review', 'Share', 'Move…'],
        uploadVersion: false,
      });
      await expectOwnerLabel('powner', await expectedOwnerLabel('powner', r), r);
      const c = await caps('powner', r);
      expect.soft(c.canPublish, 'canPublish').toBe(false);
      expect.soft(c.canManage, 'canManage').toBe(false);
      expect.soft(c.canGrantReview, 'canGrantReview').toBe(true);
      expect.soft(c.canShare, 'canShare').toBe(true);
      expect.soft(c.shareAuthority, 'shareAuthority').toBe('project_owner');
      expect.soft(c.canRetire, 'canRetire').toBe(true);
    });

    await row('R1B-04', async (r) => {
      const snap = await snapshot('padmin', 'R1B-04', { history: true });
      r.ui.snapshot = snap;
      expectControls(snap, {
        visible: ['Grant review access', 'Delete…', 'Download'],
        hidden: ['Edit', 'Review', 'Share', 'Retention…', 'Move…'],
        uploadVersion: false,
      });
      await expectOwnerLabel('padmin', await expectedOwnerLabel('padmin', r), r);
      const c = await caps('padmin', r);
      expect.soft(c.canPublish, 'canPublish').toBe(false);
      expect.soft(c.canManage, 'canManage').toBe(false);
      expect.soft(c.canShare, 'canShare').toBe(false);
      expect.soft(c.shareAuthority, 'shareAuthority').toBeNull();
      expect.soft(c.canGrantReview, 'canGrantReview').toBe(true);
      expect.soft(c.canRetire, 'canRetire').toBe(true);
      const grants = await api(
        'padmin',
        'GET',
        `/api/v1/artifacts/${artifactId()}/grants`,
        undefined,
        r
      );
      expect
        .soft([grants.status, grants.code], 'padmin: grants list refused')
        .toEqual([403, 'forbidden']);
    });

    await row('R1B-05', async (r) => {
      const snap = await snapshot('member', 'R1B-05', { history: true });
      r.ui.snapshot = snap;
      expectControls(snap, {
        visible: ['Download'],
        hidden: [
          'Edit',
          'Review',
          'Share',
          'Grant review access',
          'Retention…',
          'Delete…',
          'Move…',
        ],
        uploadVersion: false,
      });
      expect.soft(snap.historyEdit, 'History Edit hidden').toBe(false);
      expect.soft(snap.previewRendered, 'preview rendered').toBe(true);
      await expectOwnerLabel('member', await expectedOwnerLabel('member', r), r);
      const c = await caps('member', r);
      for (const flag of [
        'canPublish',
        'canReview',
        'canManage',
        'canShare',
        'canGrantReview',
        'canRetire',
      ] as const) {
        expect.soft(c[flag], `member ${flag}`).toBe(false);
      }
      const grants = await api(
        'member',
        'GET',
        `/api/v1/artifacts/${artifactId()}/grants`,
        undefined,
        r
      );
      expect
        .soft([grants.status, grants.code], 'member: grants list refused')
        .toEqual([403, 'forbidden']);
      const listed = await listRow('member');
      r.ui.listRow = listed;
      expect.soft(listed?.access, 'member: /artifacts row badged Project').toBe('Project');
    });

    await row('R1B-06', (r) => expectSameAsMissing('outsider', 'R1B-06', r));

    await row('R1B-07', async (r) => {
      for (const key of ['aowner', 'member'] as const) {
        const expected = await expectedOwnerLabel(key, r);
        const snap = await snapshot(key, `R1B-07 (${key})`, { history: true });
        r.ui[`${key}Snapshot`] = snap;
        const page = await pageOf(key);
        await tab(page, 'history').click();
        await expect
          .soft(historyPublishedBy(historyRow(page, 1)), `${key}: v1 published by the agent`)
          .toContainText(expected);
        r.ui[`${key}V1By`] = ((await historyPublishedBy(historyRow(page, 1)).textContent()) ?? '')
          .replace(/\s+/g, ' ')
          .trim();
        await expect
          .soft(historyRow(page, 1).locator('sl-badge', { hasText: 'current' }), 'v1 current')
          .toBeVisible();
      }
      const versions = await api(
        'member',
        'GET',
        `/api/v1/artifacts/${artifactId()}/versions`,
        undefined,
        r
      );
      const v1 = (
        (versions.body ?? {}) as {
          versions?: Array<{
            seq: number;
            kind?: string;
            createdByKind?: string;
            createdByRef?: string;
          }>;
        }
      ).versions?.find((v) => v.seq === 1);
      r.ui.v1 = v1 ?? null;
      expect.soft(v1?.createdByKind, 'v1 createdByKind').toBe('agent');
      expect.soft(v1?.createdByRef, 'v1 createdByRef').toBe(suite.agent?.id);
      expect.soft(v1?.kind, 'v1 kind').toBe('publish');
    });

    // ---- Review access on an agent-owned artifact ------------------------
    await row('R1B-08a', async (r) => {
      const page = await pageOf('aowner');
      await snapshot('aowner', 'R1B-08a');
      await headerControl(page, 'Grant review access').click();
      await expect(page.locator(AO.reviewSection), 'Grant review access dialog').toBeVisible();
      // Exactly one share dialog is open, and it is Grant review access; its sections are counted
      // in it alone (a steward's page also holds the closed Retention dialog).
      await expect(openShareDialog(page), 'one open share dialog').toHaveCount(1);
      r.ui.dialogLabel = await openShareDialog(page)
        .locator('sl-dialog[open]')
        .getAttribute('label');
      expect
        .soft(r.ui.dialogLabel, 'the open dialog is Grant review access')
        .toBe('Grant review access');
      r.ui.dialogSections = {
        links: await openDialogSection(page, 'links-heading').count(),
        people: await openDialogSection(page, 'people-heading').count(),
        retention: await openDialogSection(page, 'retention-heading').count(),
        review: await openDialogSection(page, 'review-heading').count(),
      };
      expect.soft(r.ui.dialogSections, 'only the review part').toEqual({
        links: 0,
        people: 0,
        retention: 0,
        review: 1,
      });
      r.ui.readNote = await page
        .locator(AO.reviewSection)
        .getByText('Review access is only for people who can already read this artifact')
        .isVisible();
      expect.soft(r.ui.readNote, 'note: only for people who can already read').toBe(true);
      const member = who('member');
      r.ui.search = await searchReviewPeople(page, member.email, member.id);
      const suggestion = reviewSuggestion(page, member.email);
      await expect(suggestion, 'people search offers the member').toBeVisible();
      await suggestion.click();
      const granted = page.waitForResponse(
        (q) =>
          q.request().method() === 'POST' &&
          new URL(q.url()).pathname === `/api/v1/artifacts/${artifactId()}/grants`
      );
      await reviewAdd(page).click();
      const g = await granted;
      const gBody = (await g.json().catch(() => ({}))) as unknown;
      r.api.push({
        as: 'aowner',
        method: 'POST',
        path: `/api/v1/artifacts/${artifactId()}/grants`,
        status: g.status(),
        code: errorCode(gBody),
      });
      expect.soft(g.status(), 'review grant POST').toBe(201);
      await expect
        .soft(page.locator(AO.reviewGranted), 'the dialog confirms the person')
        .toBeVisible();
      r.ui.confirmation = ((await page.locator(AO.reviewGranted).first().textContent()) ?? '')
        .replace(/\s+/g, ' ')
        .trim();
      await page.keyboard.press('Escape');
      const grants = await grantsAsPowner(r);
      r.ui.grants = grants;
      expect
        .soft(grants, 'member holds review')
        .toContainEqual({ subjectRef: `user:${member.id}`, permission: 'review' });
    });

    await row('R1B-08b', async (r) => {
      const page = await pageOf('padmin');
      const outsider = who('outsider');
      await snapshot('padmin', 'R1B-08b');
      // UI (recorded): what the dialog does with a person who cannot read.
      const ui: Record<string, unknown> = {};
      r.ui.dialog = ui;
      try {
        await headerControl(page, 'Grant review access').click();
        await expect(page.locator(AO.reviewSection)).toBeVisible();
        // Decided from the people-search answer for this query, not a timeout.
        const searched = await searchReviewPeople(page, outsider.email, outsider.id);
        ui.search = searched;
        const suggestion = reviewSuggestion(page, outsider.email);
        if (searched.inAnswer) await expect(suggestion, 'the answer is rendered').toBeVisible();
        ui.offered = (await suggestion.count()) > 0;
        if (ui.offered) {
          await suggestion.click();
          const answer = page.waitForResponse(
            (q) =>
              q.request().method() === 'POST' &&
              new URL(q.url()).pathname === `/api/v1/artifacts/${artifactId()}/grants`
          );
          await reviewAdd(page).click();
          const a = await answer;
          ui.postStatus = a.status();
          ui.postCode = errorCode(await a.json().catch(() => null));
          // After the answer, wait for one outcome, then record which appeared.
          await expect(
            page.locator(AO.dialogError).or(page.locator(AO.reviewGranted)).first(),
            'the dialog shows an outcome'
          ).toBeVisible();
          ui.errorShown = await isShown(page.locator(AO.dialogError));
          ui.errorText = ui.errorShown
            ? ((await page.locator(AO.dialogError).first().textContent()) ?? '').trim()
            : null;
          ui.confirmationShown = await isShown(page.locator(AO.reviewGranted));
        }
        await page.keyboard.press('Escape');
      } catch (err) {
        ui.error = firstLine(err);
      }
      // API (graded).
      const res = await api(
        'padmin',
        'POST',
        `/api/v1/artifacts/${artifactId()}/grants`,
        { subjectKind: 'principal', subjectRef: `user:${outsider.id}`, permission: 'review' },
        r
      );
      expect
        .soft([res.status, res.code], 'padmin: review grant to a non-reader')
        .toEqual([403, 'review_subject_cannot_read']);
      const grants = await grantsAsPowner(r);
      r.ui.grants = grants;
      expect
        .soft(
          grants.some((g) => g.subjectRef === `user:${outsider.id}`),
          'no grant for the outsider'
        )
        .toBe(false);
    });

    await row('R1B-09', async (r) => {
      const page = await pageOf('member');
      const snap = await snapshot('member', 'R1B-09', { history: true });
      r.ui.snapshotBefore = snap;
      expectControls(snap, {
        visible: ['Review', 'Download'],
        hidden: ['Edit', 'Share', 'Grant review access', 'Retention…', 'Delete…', 'Move…'],
        uploadVersion: false,
      });
      const before = await caps('member', r, 'memberBefore');
      expect.soft(before.canReview, 'canReview').toBe(true);
      expect.soft(before.canPublish, 'canPublish').toBe(false);
      await headerControl(page, 'Review').click();
      const editor = page.locator(AO.reviewEditor);
      await expect(editor).toBeVisible();
      await editor.click();
      await page.keyboard.press('Control+End');
      // insertText: one input, so editor auto-closing does not double braces.
      await page.keyboard.insertText(REVIEW_MARK);
      await expect(page.locator(AO.reviewCounts), 'one comment mark').toContainText('1 comment');
      const created = page.waitForResponse(
        (q) =>
          q.request().method() === 'POST' &&
          new URL(q.url()).pathname === `/api/v1/artifacts/${artifactId()}/versions`
      );
      await saveReviewButton(page).click();
      const c = await created;
      r.api.push({
        as: 'member',
        method: 'POST',
        path: `/api/v1/artifacts/${artifactId()}/versions`,
        status: c.status(),
        code: null,
      });
      expect.soft(c.status(), 'review version created').toBeLessThan(300);
      await expect
        .poll(async () => (await caps('member', r, 'memberAfter')).currentSeq, {
          message: 'v2 is current',
          timeout: 30_000,
        })
        .toBe(2);
      const after = await snapshot('member', 'R1B-09 after');
      r.ui.snapshotAfter = after;
      expect.soft(after.reviewPending, 'Review pending badge').toBe(true);
      expect.soft(after.versionLabel, 'version menu on v2').toContain('v2');
      const versions = await api(
        'member',
        'GET',
        `/api/v1/artifacts/${artifactId()}/versions`,
        undefined,
        r
      );
      const v2 = (
        (versions.body ?? {}) as {
          versions?: Array<{
            seq: number;
            kind?: string;
            createdByKind?: string;
            createdByRef?: string;
          }>;
        }
      ).versions?.find((v) => v.seq === 2);
      r.ui.v2 = v2 ?? null;
      expect.soft(v2?.kind, 'v2 kind').toBe('review');
      expect.soft(v2?.createdByKind, 'v2 by a user').toBe('user');
      expect.soft(v2?.createdByRef, 'v2 by the member').toBe(who('member').id);
      const owner = await caps('member', r, 'memberOwnerCheck');
      expect.soft(owner.ownerKind, 'owner still an agent').toBe('agent');
      expect.soft(owner.ownerRef, 'owner unchanged').toBe(suite.agent?.id);
    });

    await row('R1B-10', async (r) => {
      for (const key of ['aowner', 'powner'] as const) {
        const snap = await snapshot(key, `R1B-10 (${key})`, { history: true });
        r.ui[`${key}Snapshot`] = snap;
        expectControls(snap, {
          visible: ['Grant review access', 'Retention…', 'Delete…', 'Download'],
          hidden: ['Edit', 'Review', 'Share', 'Move…'],
          uploadVersion: false,
        });
        expect.soft(snap.historyEdit, `${key}: History Edit hidden`).toBe(false);
        expect.soft(snap.reviewPending, `${key}: Review pending`).toBe(true);
        await expectOwnerLabel(key, await expectedOwnerLabel(key, r), r);
        const c = await caps(key, r);
        expect.soft(c.ownerKind, `${key}: owner kind`).toBe('agent');
        expect.soft(c.ownerRef, `${key}: owner unchanged`).toBe(suite.agent?.id);
        if (key === 'powner') suite.expiresAtBefore = c.expiresAt;
      }
      r.ui.expiresAtBefore = suite.expiresAtBefore;
      suite.grantsAfterReview = await grantsAsPowner(r);
      r.ui.grants = suite.grantsAfterReview;
    });

    // ---- The agent is deleted ---------------------------------------------
    await row('R1B-11', async (r) => {
      const page = await pageOf('aowner');
      const agentId = suite.agent!.id;
      // Positive anchor first: the project's agent list shows the agent before the delete.
      const agentLink = page.locator(`${AO.projectPage} a[href="/agents/${agentId}"]`);
      await page.goto(`/projects/${projectId()}`, { waitUntil: 'domcontentloaded' });
      await expect(
        agentLink,
        'the project agent list shows the agent before the delete'
      ).toBeVisible();
      r.ui.listedBefore = true;
      await page.goto(`/agents/${agentId}`, { waitUntil: 'domcontentloaded' });
      const del = page.locator(AO.agentHeaderDelete);
      await expect(del, 'agent page offers Delete').toBeVisible();
      const deletes: Array<number | null> = [];
      page.on('response', (res) => {
        if (
          res.request().method() === 'DELETE' &&
          new URL(res.url()).pathname === `/api/v1/agents/${agentId}`
        ) {
          deletes.push(res.status());
        }
      });
      await del.click();
      await confirmDialogButton(page).click();
      let forced = false;
      await expect
        .poll(
          async () => {
            const read = await api('aowner', 'GET', `/api/v1/agents/${agentId}`);
            if (isGone(classifyRead(read.status, read.body, 'agent'))) return true;
            const force = confirmDialogButton(page, 'Force Delete');
            if (!forced && (await isShown(force))) {
              forced = true;
              await force.first().click();
            }
            return false;
          },
          { message: 'the agent is gone after Delete', timeout: 120_000 }
        )
        .toBe(true);
      suite.agentDeleted = true;
      const read = await api('aowner', 'GET', `/api/v1/agents/${agentId}`, undefined, r);
      r.ui.agentAfterDelete = {
        status: read.status,
        state: classifyRead(read.status, read.body, 'agent'),
      };
      r.ui.pageDeleteStatuses = deletes;
      r.ui.forced = forced;
      state.evidence.agentDelete = { pageDeleteStatuses: deletes, forced, at: Date.now() };
      expect.soft(forced, 'deleted without a force').toBe(false);
      // The agent leaves the project's agent list: the list rendered (an agent row or the empty state), then no link
      // to the agent.
      await page.goto(`/projects/${projectId()}`, { waitUntil: 'domcontentloaded' });
      await expect(
        page.locator(AO.projectAgentListRendered).first(),
        'the project agent list rendered'
      ).toBeVisible();
      await expect.soft(agentLink, 'the agent left the project agent list').toHaveCount(0);
      r.ui.listedAfter = (await agentLink.count()) > 0;
      r.ui.emptyStateAfter = await isShown(page.locator(`${AO.projectPage} .empty-state`));
    });

    await row(
      'R1B-12',
      async (r) => {
        const m = await caps('member', r);
        r.ui.memberOwnerDeletedAt = { t: Date.now(), ownerDeleted: m.ownerDeleted, raw: m.raw };
        r.ui.aownerSnapshot = await snapshot('aowner', 'R1B-12 (aowner, observation)');
        await caps('aowner', r);
      },
      { pre: () => (suite.agentDeleted ? undefined : 'needs R1B-11 (the agent deleted)') }
    );

    const marked = await row(
      'R1B-13',
      async (r) => {
        const started = Date.now();
        const deadline = started + OWNER_CHECK_TIMEOUT;
        let seen = false;
        while (Date.now() < deadline) {
          const res = await api('member', 'GET', `/api/v1/artifacts/${artifactId()}`);
          suite.ownerPolls.push({ t: Date.now(), status: res.status, body: res.body });
          const a = ((res.body ?? {}) as { artifact?: { ownerDeleted?: boolean } }).artifact;
          if (a?.ownerDeleted === true) {
            seen = true;
            break;
          }
          await new Promise((res2) => setTimeout(res2, OWNER_CHECK_POLL_MS));
        }
        r.ui.wait = { started, ended: Date.now(), polls: suite.ownerPolls.length, seen };
        expect(seen, `ownerDeleted within ${OWNER_CHECK_TIMEOUT} ms`).toBe(true);
      },
      { pre: () => (suite.agentDeleted ? undefined : 'needs R1B-11 (the agent deleted)') }
    );
    const markPre = (): string | undefined =>
      marked ? undefined : 'needs R1B-13 (the owner-deleted mark)';

    await row(
      'R1B-14',
      async (r) => {
        for (const key of ['aowner', 'powner', 'padmin', 'member'] as const) {
          const expected = await expectedOwnerLabel(key, r);
          const snap = await snapshot(key, `R1B-14 (${key})`);
          r.ui[`${key}Snapshot`] = snap;
          expect.soft(snap.headerRendered, `${key}: still reads`).toBe(true);
          expect.soft(snap.previewRendered, `${key}: preview renders`).toBe(true);
          expect.soft(snap.ownerDeletedBadge, `${key}: Owner deleted badge`).toBe(true);
          expect.soft(snap.ownerGone, `${key}: owner name struck through`).not.toBeNull();
          await expectOwnerLabel(key, expected, r);
          const c = await caps(key, r);
          expect.soft(c.ownerDeleted, `${key}: ownerDeleted`).toBe(true);
          expect.soft(c.currentSeq, `${key}: versions unchanged`).toBe(2);
          expect.soft(c.expiresAt, `${key}: expiry unchanged`).toEqual(suite.expiresAtBefore);
          const listed = await listRow(key);
          r.ui[`${key}ListRow`] = listed;
          expect
            .soft(listed?.ownerDeleted, `${key}: /artifacts row shows Owner deleted`)
            .toBe(true);
        }
        const grants = await grantsAsPowner(r);
        r.ui.grants = grants;
        expect.soft(grants, 'grants unchanged').toEqual(suite.grantsAfterReview);
      },
      { pre: markPre }
    );

    await row('R1B-15', (r) => expectSameAsMissing('outsider', 'R1B-15', r), { pre: markPre });

    await row(
      'R1B-16',
      async (r) => {
        const powner = await snapshot('powner', 'R1B-16 (powner)', { history: true });
        r.ui.powner = powner;
        expectControls(powner, {
          visible: ['Grant review access', 'Retention…', 'Delete…', 'Download'],
          hidden: ['Edit', 'Review', 'Share', 'Move…'],
          uploadVersion: false,
        });
        expect.soft(powner.historyEdit, 'powner: History Edit hidden').toBe(false);
        const pc = await caps('powner', r);
        expect.soft(pc.canShare, 'powner canShare').toBe(true);
        expect.soft(pc.shareAuthority, 'powner shareAuthority').toBe('project_owner');
        expect.soft(pc.canGrantReview, 'powner canGrantReview').toBe(true);
        expect.soft(pc.canRetire, 'powner canRetire').toBe(true);
        const padmin = await snapshot('padmin', 'R1B-16 (padmin)', { history: true });
        r.ui.padmin = padmin;
        expectControls(padmin, {
          visible: ['Grant review access', 'Delete…', 'Download'],
          hidden: ['Edit', 'Review', 'Share', 'Retention…', 'Move…'],
          uploadVersion: false,
        });
        expect.soft(padmin.historyEdit, 'padmin: History Edit hidden').toBe(false);
        const ac = await caps('padmin', r);
        expect.soft(ac.canGrantReview, 'padmin canGrantReview').toBe(true);
        expect.soft(ac.canRetire, 'padmin canRetire').toBe(true);
        expect.soft(ac.canShare, 'padmin canShare').toBe(false);
        const member = await snapshot('member', 'R1B-16 (member)');
        r.ui.member = member;
        expectControls(member, {
          visible: ['Review'],
          hidden: ['Edit', 'Share', 'Grant review access', 'Retention…', 'Delete…', 'Move…'],
        });
        const mc = await caps('member', r);
        expect.soft(mc.canReview, 'member canReview').toBe(true);
        // aowner: an observation while Q-A5 is open (no assertion).
        const aowner = await snapshot('aowner', 'R1B-16 (aowner, observation)');
        const aCaps = await caps('aowner', r, 'aownerObservation');
        r.ui.aownerObservation = {
          note: 'not graded (Q-A5 open); main ends the delegating-user clauses at the agent delete',
          snapshot: aowner,
          caps: aCaps,
        };
      },
      { pre: markPre }
    );

    for (const key of IDENTITY_KEYS) {
      const identity = state.ids[key];
      if (identity?.page && !identity.page.isClosed()) {
        await assertTestIdentity(identity.page, { email: identity.email });
      }
    }
  });

  // ---- Cleanup by absence, then the evidence (R1B-17/18) ----------------
  // eslint-disable-next-line no-empty-pattern -- Playwright needs the fixtures argument
  test.afterAll(TITLE_CLEANUP, async ({}, testInfo) => {
    const residue: string[] = [];
    const cleanup: Record<string, unknown> = {};
    const states: Record<string, string> = {};
    const r17 = rowRecord('R1B-17');
    delete r17.reason;

    // The artifact: padmin deletes it from the page (graded), else powner
    // through the API (recorded as a fallback, which fails the row).
    if (suite.artifact && state.ids.padmin) {
      const id = suite.artifact.id;
      const ui: Record<string, unknown> = {};
      cleanup.artifactUiDelete = ui;
      try {
        const page = await pageOf('padmin');
        await snapshot('padmin', 'R1B-17');
        await headerControl(page, 'Delete…').click();
        await expect(page.locator(AO.deleteDialog)).toBeVisible();
        const answer = page.waitForResponse(
          (q) =>
            q.request().method() === 'DELETE' &&
            new URL(q.url()).pathname === `/api/v1/artifacts/${id}`
        );
        // The page says it was deleted: the success toast (artifact-detail.ts onDeleted), watched from the click on.
        const toast = page
          .locator(AO.successToast)
          .filter({ hasText: `Deleted “${ARTIFACT_TITLE}”.` })
          .first()
          .waitFor({ state: 'visible' })
          .then(() => true)
          .catch(() => false);
        await deleteConfirmButton(page).click();
        const a = await answer;
        ui.status = a.status();
        ui.deletedToast = await toast;
        if (!ui.deletedToast)
          residue.push('artifact UI delete: the page did not say it was deleted');
        await expect(page).toHaveURL(new RegExp(`/projects/${escapeRegExp(projectId())}/?$`));
        ui.landed = redactPath(new URL(page.url()).pathname);
        if (a.status() !== 204) residue.push(`artifact UI delete answered ${a.status()}`);
        else suite.artifactDeleted = true;
      } catch (err) {
        ui.error = firstLine(err);
        residue.push(`artifact UI delete: ${firstLine(err)}`);
      }
      if (!suite.artifactDeleted && state.ids.powner) {
        try {
          const del = await api('powner', 'DELETE', `/api/v1/artifacts/${id}`);
          cleanup.artifactApiFallback = del.status;
          suite.artifactDeleted = del.status === 204;
        } catch (err) {
          residue.push(`cleanup error (artifact API delete): ${firstLine(err)}`);
        }
      }
      for (const key of IDENTITY_KEYS) {
        if (!state.ids[key]) continue;
        try {
          const res = await api(key, 'GET', `/api/v1/artifacts/${id}`);
          states[`artifact (${key} read)`] = classifyRead(res.status, res.body, 'artifact');
        } catch (err) {
          residue.push(`cleanup error (artifact read as ${key}): ${firstLine(err)}`);
        }
      }
      for (const key of IDENTITY_KEYS) {
        if (!state.ids[key]) continue;
        try {
          const snap = await snapshot(key, `R1B-17 after delete (${key})`, {
            path: artifactPath(id),
          });
          states[`artifact page (${key})`] = snap.rendered404 ? 'not_found' : 'present';
        } catch (err) {
          residue.push(`cleanup error (artifact page as ${key}): ${firstLine(err)}`);
        }
      }
    }

    for (const identity of Object.values(state.ids)) {
      for (const page of identity?.context.pages() ?? []) await page.close();
    }

    // The agent, if a STOP left it running.
    if (suite.agent && state.ids.aowner) {
      const agentId = suite.agent.id;
      try {
        let read = await api('aowner', 'GET', `/api/v1/agents/${agentId}`);
        if (!isGone(classifyRead(read.status, read.body, 'agent'))) {
          const del = await api('aowner', 'DELETE', `/api/v1/agents/${agentId}`);
          cleanup.agentApiDelete = del.status;
          read = await api('aowner', 'GET', `/api/v1/agents/${agentId}`);
        }
        states.agent = classifyRead(read.status, read.body, 'agent');
      } catch (err) {
        residue.push(`cleanup error (agent): ${firstLine(err)}`);
      }
    }

    // Memberships, re-read by absence, then the project, as powner.
    if (state.projectId && state.ids.powner) {
      const pid = state.projectId;
      const removals: Record<string, number | string> = {};
      for (const b of state.bindings) {
        try {
          const res = await api('powner', 'DELETE', `/api/v1/projects/${pid}/members/${b.id}`);
          removals[b.key] = res.status;
          if (res.status >= 300) residue.push(`membership ${b.key}: DELETE ${res.status}`);
        } catch (err) {
          removals[b.key] = `error: ${firstLine(err)}`;
          residue.push(`cleanup error (membership ${b.key}): ${firstLine(err)}`);
        }
      }
      cleanup.memberRemovals = removals;
      try {
        const res = await api('powner', 'GET', `/api/v1/projects/${pid}/members`);
        const items =
          ((res.body ?? {}) as { items?: Array<{ id: string; principalId?: string }> }).items ?? [];
        // Positive anchor: powner's own binding must be listed.
        const anchored =
          res.status === 200 && items.some((i) => i.principalId === who('powner').id);
        cleanup.membersAnchor = anchored;
        for (const b of state.bindings) {
          const userId = state.ids[b.key]?.id;
          const present = items.some(
            (i) => i.id === b.id || (!!userId && i.principalId === userId)
          );
          states[`binding ${b.key}`] = anchored ? (present ? 'present' : 'absent') : 'unknown';
        }
      } catch (err) {
        residue.push(`cleanup error (members): ${firstLine(err)}`);
      }
      try {
        const del = await api('powner', 'DELETE', `/api/v1/projects/${pid}`);
        cleanup.projectDelete = del.status;
        if (del.status >= 300) residue.push(`project: DELETE ${del.status}`);
        const res = await api('powner', 'GET', `/api/v1/projects/${pid}`);
        states.project = classifyRead(res.status, res.body);
      } catch (err) {
        residue.push(`cleanup error (project): ${firstLine(err)}`);
      }
    }

    // Identities and the experiment: no issuer credential exists after the
    // restart, so they end with the throwaway hub at teardown.
    cleanup.identities =
      'local hub: no issuer credential after the restart; the identities and the hub.artifacts ' +
      'override end when the global teardown discards the hub (after this evidence is written)';
    // Scheduled, not done: the global teardown runs after this evidence is written, and keeps the
    // hub's temp dir (database included) when E2E_KEEP_HUB_STATE=1.
    cleanup.keepHubState = keepHubState();
    cleanup.hubDiscardScheduled = !keepHubState();
    // The key the frozen oracle and the spec name (local mode records hubDiscarded: true); it holds the
    // same value as hubDiscardScheduled (the discard happens at global teardown, after this evidence).
    cleanup.hubDiscarded = !keepHubState();

    for (const [what, s] of Object.entries(states)) {
      if (s !== 'absent' && !isGone(s as Parameters<typeof isGone>[0])) {
        residue.push(`final: ${what} is ${s}`);
      }
    }
    cleanup.final = { states };
    state.evidence.identityChecksAfterRows = await journey.identityChecks();

    r17.status = residue.length === 0 && !cleanup.artifactApiFallback ? 'passed' : 'failed';
    r17.errors = [...residue];
    if (cleanup.artifactApiFallback)
      r17.errors.push('the artifact was deleted by the API fallback');
    r17.ui = { cleanup };

    state.evidence.requests = Object.fromEntries(
      Object.values(state.ids)
        .filter((i) => !!i)
        .map((i) => [i.key, i.requests])
    );
    state.evidence.stateSequence = suite.snapshots;
    state.evidence.agentShell = suite.shell;
    state.evidence.ownerCheckWait = suite.ownerPolls;
    state.evidence.invalid = suite.invalid;
    for (const identity of Object.values(state.ids)) await identity?.context.close();

    const cause = state.setupError ?? state.testError ?? state.stopped ?? state.hardFailures[0];
    for (const r of Object.values(state.rows)) {
      if (r.status === 'not run' && r.reason === 'not reached' && cause) {
        r.reason = `not reached: ${cause}`;
      }
    }

    state.evidence.artifact = suite.artifact
      ? { id: suite.artifact.id, deleted: suite.artifactDeleted }
      : null;
    state.evidence.findings = state.findings;
    state.evidence.hardFailures = state.hardFailures;
    state.evidence.setupError = state.setupError ?? null;
    state.evidence.testError = state.testError ?? null;
    state.evidence.cleanup = { ...cleanup, residue };
    const results = (state.evidence.results ??= {}) as Record<string, unknown>;
    results[TITLE_JOURNEY] ??= { status: 'not run', skipReason: cause ?? null };
    results[TITLE_CLEANUP] = {
      status: r17.status,
      residue: residue.length,
      annotations: annotationsOf(testInfo.annotations),
    };

    // R1B-18: the evidence holds nothing secret-shaped (checked on the
    // serialised evidence, with the row in it, before it is written).
    const r18 = rowRecord('R1B-18');
    delete r18.reason;
    r18.ui = { traces: 'off', video: 'off', screenshots: 'off', har: 'not recorded' };
    // Every row carries the recorded deviation.
    const withDeviation = (): Record<string, unknown> =>
      Object.fromEntries(
        Object.entries(state.rows).map(([id, r]) => [id, { ...r, deviation: DEVIATION }])
      );
    state.evidence.rows = withDeviation();
    const leaks = secretLikeStrings(JSON.stringify(state.evidence));
    r18.status = leaks.length === 0 ? 'passed' : 'failed';
    r18.ui.secretLikeMatches = leaks.length;
    r18.errors = leaks.map((o) => `secret-like string at offset ${o}`);
    // Again, so the evidence holds R1B-18's own result.
    state.evidence.rows = withDeviation();
    const body = JSON.stringify(state.evidence, null, 2);
    const evidenceFile = path.join(
      testInfo.project.outputDir,
      'artifacts-agent-owned-evidence.json'
    );
    fs.mkdirSync(path.dirname(evidenceFile), { recursive: true });
    fs.writeFileSync(evidenceFile, body);
    console.info(`[journey] evidence: ${evidenceFile}`);
    console.info(`[journey] cleanup: ${JSON.stringify(state.evidence.cleanup)}`);

    expect(leaks, 'R1B-18: no secret-like strings in the evidence').toEqual([]);
    expect(residue, 'R1B-17: nothing left after the cleanup').toEqual([]);
    expect(state.hardFailures, 'no row threw').toEqual([]);
  });
});
