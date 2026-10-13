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
 * Artifact access, graded at the UI per role (R1-A), end to end on a
 * throwaway hub with no broker.
 *
 * Five member test identities, each in its own browser context: owner,
 * member and padmin (members of the artifact's home project, padmin as
 * project admin), grantee (member of a second fixture project `side` with
 * the owner, NOT of the home project) and outsider (no project). Rows
 * (R1-xx, the spec's section 8), in order:
 *
 * - R1-00: hub.artifacts off: no Artifacts surfaces, the API answers 404,
 *   and the CLI names the experiment. Then the experiment is enabled
 *   through the admin experiments API.
 * - R1-01: smoke: the owner publishes a markdown file through the web New
 *   artifact dialog. A refused publish STOPS the run with a FINDING (no
 *   scope or role workaround); every later row is then "not run".
 * - R1-02..05: the artifact page per role: controls visible and hidden, and
 *   the API capability flags and probes. The outsider gets the 404 page
 *   and the same 404 as for a missing artifact.
 * - R1-06x (observation, not graded): whether the Share people search
 *   offers a user who shares no project with the owner.
 * - R1-06/07: the owner grants the grantee Can edit in the Share dialog;
 *   the grantee publishes v2 from the Edit view; the owner stays the owner.
 * - R1-08: Grant review access is hidden on a user-owned artifact.
 * - R1-B1..B5 (agent-owned artifact): NOT RUN here, skipped with the reason
 *   (they need a broker and a publishing agent).
 * - R1-09/10 (afterAll): cleanup verified by absence, then the evidence.
 *
 * Each row records its UI observation and the API statuses. The UI
 * observation decides the row; a passing API check alone never passes it.
 * Row assertions are soft, so one failed row does not hide the others; a
 * row whose precondition failed is recorded as "not run" with the reason.
 */

import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import { expect, test, type BrowserContext, type Page } from '@playwright/test';

import { assertTestIdentity, redeemTestIdentity } from '../harness/auth.js';
import { readHubState, runScionCLI } from '../harness/hub.js';
import {
  annotationsOf,
  classifyRead,
  firstLine,
  isGone,
  recordRequests,
  redactPath,
  secretLikeStrings,
  type ReadState,
  type RequestRecord,
} from './journey-lib.js';
import {
  HEADER_CONTROLS,
  SELECTORS,
  anyPageHeading,
  artifactsRow,
  artifactsRowAccess,
  editPublishButton,
  escapeRegExp,
  grantRow,
  grantRowControls,
  grantRowPermission,
  grantRowRemove,
  headerButton,
  headerTitle,
  historyCurrentBadge,
  historyEditButton,
  historyPublishedBy,
  historyRow,
  homeGrantRow,
  newArtifactButton,
  notFoundHeading,
  ownerMeta,
  permissionOption,
  publishSubmit,
  shareAddButton,
  shareCloseButton,
  shareDialog,
  shareSuggestion,
  tab,
  uploadVersionButton,
  type HeaderControl,
} from './selectors.js';
import {
  IDENTITY_KEYS,
  isRemoteTarget,
  remoteTargetFromEnv,
  stateFile,
  type IdentityKey,
  type JourneyTarget,
} from './state.js';

const EXPERIMENT = 'hub.artifacts';
const ENTRY = 'r1-access.md';
const ENTRY_TEXT = [
  '# R1 access fixture',
  '',
  'A small markdown document published by the owner through the web.',
  '',
  'It is read by the project, granted to one more person, and edited once.',
  '',
].join('\n');
const APPENDED_LINE = 'Appended by the grantee.';
const R1B_REASON =
  'NOT RUN: needs an agent-owned artifact fixture (a broker and an agent that publishes, ' +
  "created from a member session, ptone/scion#4569); main's e2e harness has no runtime " +
  'broker option. Rows frozen; run them with that fixture.';

/** One identity of the journey. */
interface Identity {
  key: IdentityKey;
  id: string;
  email: string;
  displayName: string;
  context: BrowserContext;
  page?: Page;
  requests: RequestRecord[];
}

/** One API call as recorded in the evidence. No bodies. */
interface ApiRecord {
  as: string;
  method: string;
  path: string;
  status: number;
  code: string | null;
}

/** What the artifact page showed one identity at one moment. */
interface PageSnapshot {
  t: number;
  as: IdentityKey;
  step: string;
  path: string;
  rendered404: boolean;
  headerRendered: boolean;
  title: string | null;
  owner: string | null;
  versionLabel: string | null;
  previewRendered: boolean;
  visible: HeaderControl[];
  hidden: HeaderControl[];
  /** History tab: Upload new version shown (null: not looked at). */
  uploadVersion: boolean | null;
  /** History tab: Edit shown (single-version block only; null: not looked at). */
  historyEdit: boolean | null;
}

type RowStatus = 'passed' | 'failed' | 'not run';

/** One row of the spec's section 8 in the evidence. */
interface RowRecord {
  id: string;
  role: string;
  title: string;
  graded: boolean;
  status: RowStatus;
  reason?: string;
  ui: Record<string, unknown>;
  api: ApiRecord[];
  errors: string[];
  /** A leg of the row could not run: the row cannot pass (oracle rule 3). */
  legNotRun?: string;
}

interface CapabilityFlags {
  canManage: boolean;
  canPublish: boolean;
  canReview: boolean;
  canGrantReview: boolean;
}

/** Shared state of the journey. */
const journey: {
  target?: JourneyTarget;
  tmpDir?: string;
  ids: Partial<Record<IdentityKey, Identity>>;
  projects: { home?: string; side?: string };
  bindings: Array<{ project: 'home' | 'side'; projectId: string; key: IdentityKey; id: string }>;
  artifact?: { id: string; title: string; ownerRefAtPublish?: string | undefined };
  /** Set when a precondition failure stops the remaining rows. */
  stopped?: string;
  /** First line of a beforeAll failure (identities, projects, memberships). */
  setupError?: string;
  /** First line of an R1-A failure outside any row (e.g. the experiment toggle). */
  testError?: string;
  rows: Record<string, RowRecord>;
  hardFailures: string[];
  findings: Array<{ id: string; text: string }>;
  snapshots: PageSnapshot[];
  evidence: Record<string, unknown>;
} = {
  ids: {},
  projects: {},
  bindings: [],
  rows: {},
  hardFailures: [],
  findings: [],
  snapshots: [],
  evidence: {},
};

function suffix(): string {
  return crypto.randomBytes(3).toString('hex');
}

function target(): JourneyTarget {
  if (!journey.target) throw new Error('journey target not set up');
  return journey.target;
}

function who(key: IdentityKey): Identity {
  const identity = journey.ids[key];
  if (!identity) throw new Error(`identity ${key} not set up`);
  return identity;
}

function artifactId(): string {
  if (!journey.artifact) throw new Error('no artifact published');
  return journey.artifact.id;
}

function artifactPath(): string {
  return `/projects/${journey.projects.home}/artifacts/${artifactId()}`;
}

function resolveTarget(): JourneyTarget {
  if (isRemoteTarget()) return remoteTargetFromEnv();
  const hub = readHubState(stateFile());
  if (!hub?.homeDir) throw new Error(`No local hub state at ${stateFile()}`);
  return { mode: 'local', baseURL: hub.baseURL, hub };
}

/** A response body as JSON, or null when it is empty or not JSON. */
function parseJson(text: string): unknown {
  if (!text) return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return null;
  }
}

function errorCode(body: unknown): string | null {
  const b = (body ?? {}) as { error?: { code?: unknown } | string; code?: unknown };
  if (b.error && typeof b.error === 'object' && typeof b.error.code === 'string') {
    return b.error.code;
  }
  return typeof b.code === 'string' ? b.code : null;
}

/** An API call in an identity's session (its browser context's cookies). */
async function api(
  key: IdentityKey,
  method: 'GET' | 'POST' | 'DELETE' | 'PUT',
  urlPath: string,
  data?: unknown,
  rec?: RowRecord
): Promise<{ status: number; code: string | null; body: unknown }> {
  const res = await who(key).context.request.fetch(urlPath, {
    method,
    ...(data !== undefined ? { data } : {}),
    maxRedirects: 0,
  });
  const body = parseJson(await res.text());
  await res.dispose();
  const out = { status: res.status(), code: errorCode(body), body };
  rec?.api.push({
    as: key,
    method,
    path: redactPath(new URL(urlPath, target().baseURL).pathname),
    status: out.status,
    code: out.code,
  });
  return out;
}

/** The admin bearer token: the local hub's dev token, or the remote admin token. */
function adminToken(): string | undefined {
  const t = target();
  return t.hub?.devToken ?? t.adminToken;
}

/** A request as the hub admin (fixtures only: the experiment toggle). */
async function adminRequest(
  method: string,
  urlPath: string,
  data?: unknown
): Promise<{ status: number; body: unknown }> {
  const token = adminToken();
  if (!token) throw new Error('no admin token for this target');
  const res = await fetch(new URL(urlPath, target().baseURL), {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(data !== undefined ? { 'Content-Type': 'application/json' } : {}),
    },
    ...(data !== undefined ? { body: JSON.stringify(data) } : {}),
  });
  const body = parseJson(await res.text());
  return { status: res.status, body };
}

interface ExperimentState {
  revision: number;
  enabled: boolean | null;
  override: boolean | null;
}

async function readExperiment(): Promise<ExperimentState> {
  const res = await adminRequest('GET', '/api/v1/admin/experiments');
  if (res.status !== 200) throw new Error(`GET /api/v1/admin/experiments answered ${res.status}`);
  const body = res.body as {
    revision: number;
    experiments: Array<{ name: string; enabled: boolean; override: boolean | null }>;
  };
  const entry = body.experiments.find((e) => e.name === EXPERIMENT);
  return {
    revision: body.revision,
    enabled: entry ? entry.enabled : null,
    override: entry ? entry.override : null,
  };
}

async function setExperiment(value: boolean | null): Promise<ExperimentState> {
  const before = await readExperiment();
  const res = await adminRequest('PUT', '/api/v1/admin/experiments', {
    overrides: { [EXPERIMENT]: value },
    expected_revision: before.revision,
  });
  if (res.status !== 200) throw new Error(`PUT /api/v1/admin/experiments answered ${res.status}`);
  return readExperiment();
}

/** A row of the spec's section 8: who acts, what it checks, whether it is graded. */
interface RowSpec {
  role: string;
  title: string;
  graded?: boolean;
  /** Where the row runs (for the oracle row map); default the R1-A test. */
  where?: 'a' | 'b' | 'cleanup';
}

/** Every row, declared once: it seeds the evidence and the oracle row map. */
const ROWS: Record<string, RowSpec> = {
  'R1-00a': {
    role: 'owner',
    title: 'experiment off: no Artifacts sidebar item, no project Artifacts segment',
  },
  'R1-00b': { role: 'owner', title: 'experiment off: /artifacts renders the 404 page' },
  'R1-00c': {
    role: 'admin CLI',
    title: 'experiment off: the CLI names the hub.artifacts experiment',
  },
  'R1-01': { role: 'owner', title: 'smoke: New artifact publishes and lands on the artifact' },
  'R1-02': {
    role: 'owner',
    title: 'owner sees Edit, Review, Share, Download, Upload new version and History Edit',
  },
  'R1-03a': { role: 'outsider', title: 'outsider gets the 404 page and no controls' },
  'R1-03b': { role: 'outsider', title: 'outsider: no row in /artifacts' },
  'R1-03c': {
    role: 'outsider',
    title: 'outsider: the same 404 page and answers as for a missing artifact',
  },
  'R1-04a': { role: 'member', title: 'member reads the artifact' },
  'R1-04b': {
    role: 'member',
    title: 'member: no Edit, Review, Share, Grant review access, Upload new version, History Edit',
  },
  'R1-04c': { role: 'member', title: 'member: /artifacts row badged Project' },
  'R1-05': {
    role: 'padmin',
    title: 'project admin reads; no Edit, Review, Share, Grant review access, Upload new version',
  },
  'R1-06a': { role: 'grantee', title: 'grantee before the grant: 404 page' },
  'R1-06x': {
    role: 'owner (searching for outsider)',
    title: 'observation: people search for a user sharing no project with the owner',
    graded: false,
  },
  'R1-06b': { role: 'owner', title: 'Share dialog: grantee added as Can edit' },
  'R1-06c': {
    role: 'grantee',
    title: 'grantee after the grant: Edit, Review, Upload new version; no Share',
  },
  'R1-07a': { role: 'grantee', title: 'grantee: Edit, then Publish new version lands on v2' },
  'R1-07b': {
    role: 'owner',
    title: 'owner after v2: still the owner; v2 published by the grantee',
  },
  'R1-07c': { role: 'member', title: 'member after the grant and v2: still read-only' },
  'R1-08': {
    role: 'owner, member, padmin, grantee',
    title: 'Grant review access hidden on a user-owned artifact',
  },
  'R1-B1': {
    role: 'owner (delegating user)',
    title: 'agent-owned: Grant review access visible; Share, Edit, Review hidden',
    where: 'b',
  },
  'R1-B2': {
    role: 'padmin',
    title: 'agent-owned: Grant review access visible; Share hidden',
    where: 'b',
  },
  'R1-B3': {
    role: 'member',
    title: 'agent-owned: Grant review access and Share hidden',
    where: 'b',
  },
  'R1-B4': {
    role: 'owner -> member',
    title: 'Grant review access dialog gives Can review',
    where: 'b',
  },
  'R1-B5': {
    role: 'member',
    title: 'review grantee sees Review only and saves a review; owner unchanged',
    where: 'b',
  },
  'R1-09': {
    role: '-',
    title: 'cleanup by absence: artifact, memberships, both projects, identities; experiment reset',
    where: 'cleanup',
  },
  'R1-10': {
    role: '-',
    title: 'evidence written after the cleanup; no traces, HAR, video, tokens, cookies or codes',
    where: 'cleanup',
  },
};

/** Reason a seeded row keeps when nothing reached it. */
const NOT_REACHED = 'not reached';

/** The row record of id (seeded from ROWS). */
function rowRecord(id: string): RowRecord {
  const spec = ROWS[id];
  if (!spec) throw new Error(`row ${id} is not in the row table`);
  journey.rows[id] ??= {
    id,
    role: spec.role,
    title: spec.title,
    graded: spec.graded ?? true,
    status: 'not run',
    reason: NOT_REACHED,
    ui: {},
    api: [],
    errors: [],
  };
  return journey.rows[id];
}

/** Seeds every row as "not run" (not reached), so none can go missing. */
function seedRows(): void {
  for (const id of Object.keys(ROWS)) rowRecord(id);
}

/** Marks rows "not run" with a reason (a failed precondition or a skip). */
function notRun(ids: string[], reason: string): void {
  for (const id of ids) {
    const r = rowRecord(id);
    r.status = 'not run';
    r.reason = reason;
  }
}

/**
 * Runs one row as a test step. Its assertions are soft: a failed row is
 * recorded and the journey goes on. A throw (a step that could not be
 * done) fails the row too and is collected for the final check. A row
 * whose required rows did not pass, or that runs after a stop, is "not run".
 */
async function row(
  id: string,
  fn: (r: RowRecord) => Promise<void> | void,
  opts: { requires?: string[] } = {}
): Promise<boolean> {
  const r = rowRecord(id);
  if (journey.stopped) {
    r.status = 'not run';
    r.reason = journey.stopped;
    return false;
  }
  const blocked = (opts.requires ?? []).filter((x) => journey.rows[x]?.status !== 'passed');
  if (blocked.length > 0) {
    r.status = 'not run';
    r.reason = `needs ${blocked.join(', ')} to pass`;
    return false;
  }
  delete r.reason;
  const info = test.info();
  const before = info.errors.length;
  let threw: unknown = null;
  try {
    await test.step(`${id}: ${r.title}`, () => fn(r));
  } catch (err) {
    threw = err;
  }
  const softErrors = info.errors.slice(before).map((e) => firstLine(e.message ?? ''));
  r.errors.push(...softErrors);
  if (threw) {
    r.errors.push(firstLine(threw));
    // An ungraded row (an observation) never fails the journey; it has no
    // soft checks either.
    if (r.graded) journey.hardFailures.push(`${id}: ${firstLine(threw)}`);
  }
  if (threw || softErrors.length > 0) {
    r.status = 'failed';
  } else if (r.legNotRun) {
    r.status = 'not run';
    r.reason = r.legNotRun;
  } else {
    r.status = 'passed';
  }
  return r.status === 'passed';
}

/** Records a FINDING: in the evidence and as a Playwright annotation. */
function finding(id: string, text: string): void {
  journey.findings.push({ id, text });
  test.info().annotations.push({ type: 'FINDING', description: `${id}: ${text}` });
}

/**
 * Reveals the project page's Files area: it renders as a placeholder and
 * is replaced by the real section once scrolled into view
 * (project-detail.ts:1407-1445). Waits until the placeholder is gone.
 */
async function revealFiles(page: Page): Promise<void> {
  await page.locator(SELECTORS.projectPage).waitFor({ state: 'attached' });
  const placeholder = page.locator(SELECTORS.filesPlaceholder);
  const anyFiles = [
    SELECTORS.filesPlaceholder,
    SELECTORS.filesTabGroup,
    SELECTORS.filesSegmentArtifacts,
  ].join(', ');
  await page.locator(anyFiles).first().waitFor({ state: 'attached' });
  if ((await placeholder.count()) > 0) await placeholder.scrollIntoViewIfNeeded();
  await expect(placeholder, 'Files placeholder replaced by the Files area').toHaveCount(0);
}

/** The identity's page, opened once. */
async function pageOf(key: IdentityKey): Promise<Page> {
  const identity = who(key);
  if (!identity.page || identity.page.isClosed()) {
    identity.page = await identity.context.newPage();
  }
  return identity.page;
}

async function isShown(locator: ReturnType<Page['locator']>): Promise<boolean> {
  return (await locator.count()) > 0 && (await locator.first().isVisible());
}

/**
 * Opens (or reloads) the artifact page as key and records what it shows:
 * the 404 page, or the header with each graded control visible or hidden,
 * and, with history, whether the History tab offers Upload new version
 * and Edit. opts.path opens another artifact page (a missing artifact).
 *
 * It waits for one of two positive anchors before reading anything: the
 * 404 heading, or the header title plus the Version menu plus the markdown
 * preview (rendered only once the entry text loaded, which is also when
 * Edit and Review are decided). So a page that never rendered can never
 * read as "hidden".
 */
async function snapshotArtifactPage(
  key: IdentityKey,
  step: string,
  opts: { history?: boolean; path?: string } = {}
): Promise<PageSnapshot> {
  const page = await pageOf(key);
  await page.goto(opts.path ?? artifactPath(), { waitUntil: 'domcontentloaded' });
  await anyPageHeading(page).waitFor({ state: 'visible' });
  const headerRendered = await isShown(headerTitle(page));
  const rendered404 = await isShown(notFoundHeading(page));
  let previewRendered = false;
  if (headerRendered) {
    await page.locator(SELECTORS.versionMenu).waitFor({ state: 'visible' });
    await page.locator(SELECTORS.markdownFrame).waitFor({ state: 'attached' });
    previewRendered = true;
  }
  const visible: HeaderControl[] = [];
  const hidden: HeaderControl[] = [];
  for (const control of HEADER_CONTROLS) {
    if (await isShown(headerButton(page, control))) visible.push(control);
    else hidden.push(control);
  }
  let uploadVersion: boolean | null = null;
  let historyEdit: boolean | null = null;
  if (opts.history && headerRendered) {
    await tab(page, 'history').click();
    await historyRow(page, 1).waitFor({ state: 'visible' });
    uploadVersion = await isShown(uploadVersionButton(page));
    historyEdit = await isShown(historyEditButton(page));
    await tab(page, 'preview').click();
  }
  const snap: PageSnapshot = {
    t: Date.now(),
    as: key,
    step,
    path: redactPath(new URL(page.url()).pathname),
    rendered404,
    headerRendered,
    title: headerRendered ? ((await headerTitle(page).textContent()) ?? '').trim() : null,
    owner: headerRendered ? ((await ownerMeta(page).textContent()) ?? '').trim() : null,
    versionLabel: headerRendered
      ? ((await page.locator(SELECTORS.versionMenu).textContent()) ?? '')
          .replace(/\s+/g, ' ')
          .trim()
      : null,
    previewRendered,
    visible,
    hidden,
    uploadVersion,
    historyEdit,
  };
  journey.snapshots.push(snap);
  return snap;
}

/** Soft checks of a snapshot against the visible and hidden control sets. */
function expectControls(
  snap: PageSnapshot,
  want: {
    visible: HeaderControl[];
    hidden: HeaderControl[];
    uploadVersion?: boolean;
    historyEdit?: boolean;
  }
): void {
  expect.soft(snap.headerRendered, `${snap.as}: artifact header rendered`).toBe(true);
  expect.soft(snap.rendered404, `${snap.as}: no 404 page`).toBe(false);
  for (const c of want.visible) {
    expect.soft(snap.visible, `${snap.as}: ${c} visible`).toContain(c);
  }
  for (const c of want.hidden) {
    expect.soft(snap.hidden, `${snap.as}: ${c} hidden`).toContain(c);
  }
  if (want.uploadVersion !== undefined) {
    expect
      .soft(
        snap.uploadVersion,
        `${snap.as}: Upload new version ${want.uploadVersion ? 'visible' : 'hidden'}`
      )
      .toBe(want.uploadVersion);
  }
  if (want.historyEdit !== undefined) {
    expect
      .soft(snap.historyEdit, `${snap.as}: History Edit ${want.historyEdit ? 'visible' : 'hidden'}`)
      .toBe(want.historyEdit);
  }
}

/** The capability flags of GET /api/v1/artifacts/{id} as key. */
async function capabilities(
  key: IdentityKey,
  rec: RowRecord
): Promise<{ status: number; flags: CapabilityFlags; body: unknown }> {
  const res = await api(key, 'GET', `/api/v1/artifacts/${artifactId()}`, undefined, rec);
  const b = (res.body ?? {}) as Partial<CapabilityFlags>;
  const flags: CapabilityFlags = {
    canManage: !!b.canManage,
    canPublish: !!b.canPublish,
    canReview: !!b.canReview,
    canGrantReview: !!b.canGrantReview,
  };
  rec.ui[`${key}Flags`] = flags;
  return { status: res.status, flags, body: res.body };
}

/** Opens /artifacts as key and returns the access badge of the artifact's row, or null. */
async function artifactsListAccess(key: IdentityKey): Promise<string | null> {
  const page = await pageOf(key);
  const listed = page.waitForResponse(
    (res) => res.request().method() === 'GET' && new URL(res.url()).pathname === '/api/v1/artifacts'
  );
  await page.goto('/artifacts', { waitUntil: 'domcontentloaded' });
  expect.soft((await listed).status(), `${key}: /artifacts list status`).toBe(200);
  await page
    .locator(`${SELECTORS.artifactsRow}, ${SELECTORS.artifactsEmpty}`)
    .first()
    .waitFor({ state: 'visible' });
  const r = artifactsRow(page, journey.artifact!.title);
  if ((await r.count()) === 0) return null;
  return ((await artifactsRowAccess(r).textContent()) ?? '').replace(/\s+/g, ' ').trim();
}

/** The owner's "Owner:" label as another identity would see it. */
function ownerLabels(): string[] {
  const o = who('owner');
  return [o.displayName, o.email].filter((x) => x !== '');
}

/** Body of a publish create for a probe (one small file; never finalized). */
function probePublishBody(): unknown {
  const data = Buffer.from('probe\n');
  return {
    entry: 'probe.md',
    files: [
      {
        path: 'probe.md',
        size: data.length,
        sha256: crypto.createHash('sha256').update(data).digest('hex'),
      },
    ],
  };
}

/** Issues the five identities (local) or reads them from the environment, and redeems each. */
async function setUpIdentities(browserNewContext: () => Promise<BrowserContext>): Promise<void> {
  const t = target();
  for (const key of IDENTITY_KEYS) {
    let codeFile: string;
    let email: string;
    let displayName = '';
    if (t.mode === 'local') {
      const hub = t.hub!;
      const tokenFile = path.join(journey.tmpDir!, `${key}.token`);
      const issued = JSON.parse(
        runScionCLI(hub, [
          'hub',
          'test-identity',
          'issue',
          '--role',
          'member',
          '--purpose',
          `e2e artifact access (${key})`,
          '--out',
          tokenFile,
          '--json',
        ])
      ) as { identity: { id: string; email: string; displayName?: string } };
      // The browser path needs no API token.
      fs.rmSync(tokenFile, { force: true });
      email = issued.identity.email;
      displayName = issued.identity.displayName ?? '';
      codeFile = path.join(journey.tmpDir!, `${key}.code`);
      runScionCLI(hub, ['hub', 'test-identity', 'web-code', issued.identity.id, '--out', codeFile]);
    } else {
      ({ codeFile, email } = t.identities![key]);
    }
    const context = await browserNewContext();
    const requests = recordRequests(context);
    await redeemTestIdentity(context, { codeFile });
    await assertTestIdentity(context.request, { email, role: 'member' });
    const me = await context.request.get('/api/v1/auth/me');
    const meBody = (await me.json()) as { id?: string; displayName?: string };
    await me.dispose();
    if (!meBody.id) throw new Error(`no user id for identity ${key}`);
    journey.ids[key] = {
      key,
      id: meBody.id,
      email,
      displayName: displayName || meBody.displayName || '',
      context,
      requests,
    };
  }
}

/** Creates a hub-managed project as the owner. */
async function createProject(name: string): Promise<string> {
  const res = await api('owner', 'POST', '/api/v1/projects', { name });
  if (res.status !== 201) throw new Error(`owner creates project ${name}: ${res.status}`);
  return (res.body as { id: string }).id;
}

/** Adds key to a project with a built-in project role, as the owner (fixture). */
async function addMember(
  project: 'home' | 'side',
  key: IdentityKey,
  roleName: 'project-member' | 'project-admin',
  fixtures: Record<string, unknown>
): Promise<void> {
  const projectId = journey.projects[project]!;
  const roles = await api('owner', 'GET', `/api/v1/projects/${projectId}/members/assignable-roles`);
  const items = ((roles.body ?? {}) as { items?: Array<{ id: string; name: string }> }).items ?? [];
  const role = items.find((r) => r.name === roleName);
  if (!role) throw new Error(`no assignable role ${roleName} (status ${roles.status})`);
  const res = await api('owner', 'POST', `/api/v1/projects/${projectId}/members`, {
    roleDefinitionId: role.id,
    principalType: 'user',
    principalId: who(key).email,
  });
  fixtures[`${key}@${project}`] = { role: roleName, status: res.status };
  if (res.status !== 201 && res.status !== 200) {
    throw new Error(`add ${key} to ${project} as ${roleName}: ${res.status} ${String(res.code)}`);
  }
  journey.bindings.push({ project, projectId, key, id: (res.body as { id: string }).id });
}

const TITLE_A = 'R1-A: artifact access per role, graded at the UI';
const R1B_IDS = Object.keys(ROWS).filter((id) => ROWS[id].where === 'b');
const r1bTitle = (id: string): string => `${id}: agent-owned artifact`;
const TITLE_CLEANUP = 'R1-09/R1-10: cleanup by absence, then the evidence';

/** Every row mapped to the test that carries it. */
const ORACLE_ROW_MAP: Record<string, { test: string; note: string }> = Object.fromEntries(
  Object.entries(ROWS).map(([id, spec]) => {
    if (spec.where === 'b') return [id, { test: r1bTitle(id), note: R1B_REASON }];
    if (spec.where === 'cleanup') {
      return [id, { test: TITLE_CLEANUP, note: `the afterAll hook: evidence.rows["${id}"]` }];
    }
    const kind = spec.graded === false ? 'observation, not graded' : 'a test step';
    return [id, { test: TITLE_A, note: `evidence.rows["${id}"] (${kind})` }];
  })
);

// ---- R1-B: agent-owned artifact (frozen rows, NOT RUN here) ------------
// Declared before the serial R1-A group and outside it, so each body always
// runs and records its reason, whatever happens to R1-A.
test.describe('agent-owned artifact rows (R1-B, not run)', () => {
  for (const id of R1B_IDS) {
    test(r1bTitle(id), () => {
      const spec = ROWS[id];
      notRun([id], R1B_REASON);
      test.info().annotations.push({
        type: 'NOT RUN',
        description: `${id} (${spec.role}): ${spec.title}`,
      });
      const results = (journey.evidence.results ??= {}) as Record<string, unknown>;
      results[r1bTitle(id)] = {
        status: 'not run',
        skipReason: R1B_REASON,
        annotations: annotationsOf(test.info().annotations),
      };
      test.skip(true, R1B_REASON);
    });
  }
});

test.describe.serial('artifact access per role (R1)', () => {
  // eslint-disable-next-line no-empty-pattern -- Playwright needs the fixtures argument
  test.afterEach(({}, testInfo) => {
    if (testInfo.title === TITLE_A && testInfo.error?.message) {
      journey.testError ??= firstLine(testInfo.error.message);
    }
    const results = (journey.evidence.results ??= {}) as Record<string, unknown>;
    results[testInfo.title] = {
      status: testInfo.status,
      skipReason: testInfo.annotations.find((a) => a.type === 'skip')?.description ?? null,
      durationMs: testInfo.duration,
      annotations: annotationsOf(testInfo.annotations),
    };
  });

  test.beforeAll(async ({ browser }) => {
    // Every row starts as "not run" (not reached), so none can go missing.
    seedRows();
    try {
      const t = resolveTarget();
      journey.target = t;
      journey.evidence.oracleRowMap = ORACLE_ROW_MAP;
      journey.evidence.target = {
        mode: t.mode,
        baseURL: t.baseURL,
        hubId: t.hub?.hubId ?? null,
        experimentToggle: !!adminToken(),
      };
      journey.evidence.fixture = {
        broker: 'none (R1-A needs no broker, runtime or credentials)',
        publishSurface: 'web New artifact dialog on the project page (File mode)',
        projects: 'home (owner, member, padmin), side (owner, grantee); outsider in none',
      };
      journey.tmpDir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-aa-')));
      fs.chmodSync(journey.tmpDir, 0o700);

      await setUpIdentities(() => browser.newContext({ baseURL: t.baseURL }));

      const fixtures: Record<string, unknown> = {};
      journey.projects.home = await createProject(`e2e-r1-home-${suffix()}`);
      journey.projects.side = await createProject(`e2e-r1-side-${suffix()}`);
      await addMember('home', 'member', 'project-member', fixtures);
      await addMember('home', 'padmin', 'project-admin', fixtures);
      await addMember('side', 'grantee', 'project-member', fixtures);
      journey.evidence.projects = {
        home: { id: journey.projects.home },
        side: { id: journey.projects.side },
      };
      journey.evidence.identities = Object.fromEntries(
        IDENTITY_KEYS.map((key) => [
          key,
          {
            id: who(key).id,
            role: {
              owner: 'artifact owner; owner of home and side',
              member: 'home project-member, no grant',
              padmin: 'home project-admin, no grant',
              grantee: 'side project-member; not in home; gets Can edit through the UI',
              outsider: 'no project, no grant',
            }[key],
          },
        ])
      );
      journey.evidence.membershipFixtures = fixtures;
    } catch (err) {
      journey.setupError = `beforeAll: ${firstLine(err)}`;
      throw err;
    }
  });

  test(TITLE_A, async () => {
    const t = target();

    // ---- R1-00: hub.artifacts off ------------------------------------
    const experiment: Record<string, unknown> = {};
    journey.evidence.experiment = experiment;
    if (!adminToken()) {
      notRun(
        ['R1-00a', 'R1-00b', 'R1-00c'],
        'remote target without E2E_ARTIFACTS_TOGGLE_EXPERIMENT=1: the experiment is not toggled'
      );
    } else {
      experiment.before = await readExperiment();
      await row('R1-00a', async (r) => {
        const before = experiment.before as ExperimentState;
        r.ui.experimentBefore = before;
        expect(before.override, 'no stored override of hub.artifacts before the run').toBeNull();
        expect.soft(before.enabled, 'hub.artifacts is off').toBe(false);
        const page = await pageOf('owner');
        await page.goto(`/projects/${journey.projects.home}`, { waitUntil: 'domcontentloaded' });
        await page.locator(SELECTORS.navProjects).filter({ visible: true }).first().waitFor();
        await revealFiles(page);
        // Positive anchor of the revealed Files area (with artifacts off it
        // shows its file tabs); only then is the segment's absence meaningful.
        await expect(page.locator(SELECTORS.filesTabGroup), 'Files area revealed').toBeAttached();
        r.ui.filesAnchor = 'files-tab-group attached, placeholder gone';
        r.ui.navArtifacts = await page.locator(SELECTORS.navArtifacts).count();
        r.ui.filesSegmentArtifacts = await page.locator(SELECTORS.filesSegmentArtifacts).count();
        expect.soft(r.ui.navArtifacts, 'no Artifacts sidebar item').toBe(0);
        expect.soft(r.ui.filesSegmentArtifacts, 'no Artifacts segment in Files').toBe(0);
      });
      await row('R1-00b', async (r) => {
        const page = await pageOf('owner');
        await page.goto('/artifacts', { waitUntil: 'domcontentloaded' });
        r.ui.rendered404 = await notFoundHeading(page)
          .waitFor({ state: 'visible' })
          .then(() => true)
          .catch(() => false);
        expect.soft(r.ui.rendered404, '/artifacts renders the 404 page').toBe(true);
        const list = await api('owner', 'GET', '/api/v1/artifacts?mine=1', undefined, r);
        expect.soft(list.status, 'GET /api/v1/artifacts?mine=1 while off').toBe(404);
        const one = await api(
          'owner',
          'GET',
          `/api/v1/artifacts/${crypto.randomUUID()}`,
          undefined,
          r
        );
        expect.soft(one.status, 'GET /api/v1/artifacts/<random> while off').toBe(404);
      });
      const cliHub = t.hub;
      if (!cliHub) {
        notRun(['R1-00c'], 'remote target: no local CLI');
      } else {
        await row('R1-00c', (r) => {
          const hub = cliHub;
          let exit: string;
          let hint = '';
          try {
            runScionCLI(hub, ['artifact', 'get', crypto.randomUUID()]);
            exit = '0';
          } catch (err) {
            const msg = err instanceof Error ? err.message : String(err);
            exit = /exit (\S+)\)/.exec(msg)?.[1] ?? 'unknown';
            hint = msg.split('\n').find((l) => l.includes('hub.artifacts')) ?? '';
          }
          r.ui.cli = { exit, hint: hint.trim() };
          expect.soft(exit, 'scion artifact get fails while off').not.toBe('0');
          expect
            .soft(hint, 'the CLI hint names the hub.artifacts experiment')
            .toContain('hub.artifacts experiment');
        });
      }
      experiment.afterEnable = await setExperiment(true);
      expect((experiment.afterEnable as ExperimentState).enabled, 'hub.artifacts enabled').toBe(
        true
      );
    }
    if (!adminToken()) {
      // Remote without the toggle: the experiment must already be on.
      const probe = await api('owner', 'GET', '/api/v1/artifacts?mine=1');
      experiment.remoteListStatus = probe.status;
      expect(probe.status, 'hub.artifacts must be on for the rest of R1').toBe(200);
    }

    // ---- R1-01: smoke, the owner publishes through the web -------------
    const smoke: Record<string, unknown> = {};
    journey.evidence.smoke = smoke;
    const smokePassed = await row('R1-01', async (r) => {
      const page = await pageOf('owner');
      await page.goto(`/projects/${journey.projects.home}`, { waitUntil: 'domcontentloaded' });
      await revealFiles(page);
      await expect(page.locator(SELECTORS.filesSegmentArtifacts)).toBeVisible();
      await page.locator(SELECTORS.filesSegmentArtifacts).click();
      await expect(page.locator(SELECTORS.artifactList)).toBeAttached();
      const newButton = newArtifactButton(page);
      await expect(newButton, 'New artifact visible to the owner').toBeVisible();
      r.ui.newArtifactVisible = true;
      await newButton.click();
      await expect(page.locator(SELECTORS.publishDialog)).toBeVisible();
      await page.locator(SELECTORS.publishFileInput).setInputFiles({
        name: ENTRY,
        mimeType: 'text/markdown',
        buffer: Buffer.from(ENTRY_TEXT),
      });
      const title = `R1 access ${suffix()}`;
      await page.locator(SELECTORS.publishTitleInput).fill(title);

      const createResponse = page.waitForResponse(
        (res) =>
          res.request().method() === 'POST' && new URL(res.url()).pathname === '/api/v1/artifacts'
      );
      await publishSubmit(page).click();
      const created = await createResponse;
      const createBody = (await created.json().catch(() => null)) as unknown;
      smoke.createStatus = created.status();
      smoke.code = errorCode(createBody);
      r.api.push({
        as: 'owner',
        method: 'POST',
        path: '/api/v1/artifacts',
        status: created.status(),
        code: errorCode(createBody),
      });
      if (created.status() === 401 || created.status() === 403) {
        smoke.stopped = true;
        const text =
          `member session cannot publish (${created.status()} ${String(errorCode(createBody))}); ` +
          'no scope or role workaround';
        finding('R1-01', text);
        journey.stopped = `not run: smoke precondition failed (${text})`;
        throw new Error(`STOP: ${text}`);
      }
      expect(created.status(), 'publish create').toBe(201);
      smoke.stopped = false;

      // The dialog finalizes and the list opens the artifact page.
      await expect(page).toHaveURL(
        new RegExp(`/projects/${journey.projects.home}/artifacts/[0-9a-f-]+$`)
      );
      const id = new URL(page.url()).pathname.split('/').pop()!;
      journey.artifact = { id, title };
      const finalize = who('owner').requests.find(
        (q) => q.method === 'POST' && q.path === `/api/v1/artifacts/${id}/versions/1/finalize`
      );
      smoke.finalizeStatus = finalize?.status ?? null;
      expect.soft(finalize?.status, 'finalize status').toBeGreaterThanOrEqual(200);
      expect.soft(finalize?.status, 'finalize status').toBeLessThan(300);
      await expect(headerTitle(page)).toHaveText(title);
      r.ui.landed = redactPath(new URL(page.url()).pathname);

      const read = await capabilities('owner', r);
      smoke.readStatus = read.status;
      expect.soft(read.status, 'owner reads the artifact').toBe(200);
      const art = (read.body as { artifact?: { ownerKind?: string; ownerRef?: string } }).artifact;
      journey.artifact.ownerRefAtPublish = art?.ownerRef;
      r.ui.ownerAtPublish = {
        ownerKind: art?.ownerKind ?? null,
        ownerRef: art?.ownerRef ?? null,
      };
    });
    // Oracle rule 3: a failed smoke stops every later graded row, whatever
    // failed in it (cleanup still deletes a published artifact).
    if (!smokePassed) {
      const first = journey.rows['R1-01']?.errors[0] ?? 'no error recorded';
      journey.stopped ??= `not run: smoke precondition failed (R1-01: ${first})`;
    }

    // ---- R1-02: the owner's view (control baseline) -------------------
    await row('R1-02', async (r) => {
      const snap = await snapshotArtifactPage('owner', 'R1-02', { history: true });
      r.ui.snapshot = snap;
      // Positive anchors for the hidden checks of later rows: the owner
      // sees History's Upload new version and Edit on the same v1 page.
      expectControls(snap, {
        visible: ['Edit', 'Review', 'Share', 'Download'],
        hidden: ['Grant review access'],
        uploadVersion: true,
        historyEdit: true,
      });
      expect.soft(snap.owner, 'owner label').toBe('Owner: You');
      expect.soft(snap.versionLabel, 'version menu').toContain('v1 (current)');
      const caps = await capabilities('owner', r);
      expect.soft(caps.status).toBe(200);
      expect.soft(caps.flags, 'owner flags').toEqual({
        canManage: true,
        canPublish: true,
        canReview: false,
        canGrantReview: false,
      });
    });

    // ---- R1-03: outsider, non-disclosure ------------------------------
    await row('R1-03a', async (r) => {
      const snap = await snapshotArtifactPage('outsider', 'R1-03a');
      r.ui.snapshot = snap;
      expect.soft(snap.rendered404, 'outsider: 404 page').toBe(true);
      expect.soft(snap.headerRendered, 'outsider: no artifact header').toBe(false);
      expect.soft(snap.visible, 'outsider: no header controls').toEqual([]);
      const page = await pageOf('outsider');
      expect.soft(await page.locator(SELECTORS.header).count(), 'no header element').toBe(0);
      expect.soft(await tab(page, 'preview').count(), 'no tabs').toBe(0);
      for (const sub of ['', '/versions', `/versions/1/files/${ENTRY}`]) {
        const res = await api(
          'outsider',
          'GET',
          `/api/v1/artifacts/${artifactId()}${sub}`,
          undefined,
          r
        );
        expect.soft(res.status, `outsider GET artifact${sub}`).toBe(404);
      }
    });
    await row('R1-03b', async (r) => {
      const access = await artifactsListAccess('outsider');
      r.ui.listRow = access;
      expect.soft(access, 'outsider: no /artifacts row').toBeNull();
      const res = await api('outsider', 'GET', '/api/v1/artifacts?mine=1', undefined, r);
      const ids =
        ((res.body ?? {}) as { artifacts?: Array<{ id: string }> }).artifacts?.map((a) => a.id) ??
        [];
      r.ui.listContainsId = ids.includes(artifactId());
      expect.soft(res.status, 'list status').toBe(200);
      expect.soft(r.ui.listContainsId, 'list has no row for the artifact').toBe(false);
    });
    await row('R1-03c', async (r) => {
      const missing = crypto.randomUUID();
      // UI half: the page of a missing artifact looks like R1-03a's.
      const snap = await snapshotArtifactPage('outsider', 'R1-03c', {
        path: `/projects/${journey.projects.home}/artifacts/${missing}`,
      });
      r.ui.missingSnapshot = snap;
      r.ui.realSnapshot =
        journey.snapshots.find((x) => x.as === 'outsider' && x.step === 'R1-03a') ?? null;
      const page = await pageOf('outsider');
      expect.soft(snap.rendered404, 'missing artifact: 404 page').toBe(true);
      expect.soft(snap.headerRendered, 'missing artifact: no artifact header').toBe(false);
      expect.soft(snap.visible, 'missing artifact: no header controls').toEqual([]);
      expect.soft(await page.locator(SELECTORS.header).count(), 'no header element').toBe(0);
      expect.soft(await tab(page, 'preview').count(), 'no tabs').toBe(0);
      // API half: the same status and error code for each read.
      const pairs: Array<Record<string, unknown>> = [];
      for (const sub of ['', '/versions', `/versions/1/files/${ENTRY}`]) {
        const real = await api(
          'outsider',
          'GET',
          `/api/v1/artifacts/${artifactId()}${sub}`,
          undefined,
          r
        );
        const none = await api(
          'outsider',
          'GET',
          `/api/v1/artifacts/${missing}${sub}`,
          undefined,
          r
        );
        pairs.push({ sub, real: [real.status, real.code], missing: [none.status, none.code] });
        expect
          .soft([real.status, real.code], `same answer for ${sub || 'the artifact'}`)
          .toEqual([none.status, none.code]);
      }
      r.ui.compared = pairs;
    });

    // ---- R1-04: member (home-project reader) --------------------------
    await row('R1-04a', async (r) => {
      const snap = await snapshotArtifactPage('member', 'R1-04', { history: true });
      r.ui.snapshot = snap;
      expectControls(snap, { visible: ['Download'], hidden: [] });
      expect.soft(snap.title, 'title').toBe(journey.artifact!.title);
      expect.soft(snap.previewRendered, 'preview rendered').toBe(true);
      expect.soft(snap.versionLabel, 'version menu').toContain('v1 (current)');
      // Names resolve after the first render (ids until then): poll the label.
      await expect
        .soft(ownerMeta(await pageOf('member')), 'owner label names the owner')
        .toHaveText(new RegExp(`^\\s*Owner: (${ownerLabels().map(escapeRegExp).join('|')})\\s*$`));
      r.ui.ownerLabel = ((await ownerMeta(await pageOf('member')).textContent()) ?? '').trim();
      const caps = await capabilities('member', r);
      expect.soft(caps.status, 'member GET').toBe(200);
    });
    await row('R1-04b', async (r) => {
      const snap = journey.snapshots.find((s) => s.as === 'member' && s.step === 'R1-04');
      if (!snap) throw new Error('no member snapshot (R1-04a did not reach the page)');
      r.ui.snapshot = snap;
      expectControls(snap, {
        visible: [],
        hidden: ['Edit', 'Review', 'Share', 'Grant review access'],
        uploadVersion: false,
        historyEdit: false,
      });
      const caps = await capabilities('member', r);
      expect.soft(caps.flags, 'member flags').toEqual({
        canManage: false,
        canPublish: false,
        canReview: false,
        canGrantReview: false,
      });
      const publish = await api(
        'member',
        'POST',
        `/api/v1/artifacts/${artifactId()}/versions`,
        probePublishBody(),
        r
      );
      expect.soft(publish.status, 'member publish probe').toBe(403);
      const grant = await api(
        'member',
        'POST',
        `/api/v1/artifacts/${artifactId()}/grants`,
        {
          subjectKind: 'principal',
          subjectRef: `user:${who('outsider').id}`,
          permission: 'read',
        },
        r
      );
      expect.soft(grant.status, 'member grant probe').toBe(403);
    });
    await row('R1-04c', async (r) => {
      const access = await artifactsListAccess('member');
      r.ui.listRow = access;
      expect.soft(access, 'member: /artifacts access badge').toBe('Project');
      const res = await api('member', 'GET', '/api/v1/artifacts?mine=1', undefined, r);
      const item = (
        (res.body ?? {}) as { artifacts?: Array<{ id: string; access?: string }> }
      ).artifacts?.find((a) => a.id === artifactId());
      r.ui.listAccess = item?.access ?? null;
      expect.soft(item?.access, 'list access').toBe('project');
    });

    // ---- R1-05: project admin, no grant --------------------------------
    await row('R1-05', async (r) => {
      const snap = await snapshotArtifactPage('padmin', 'R1-05', { history: true });
      r.ui.snapshot = snap;
      expectControls(snap, {
        visible: ['Download'],
        hidden: ['Edit', 'Review', 'Share', 'Grant review access'],
        uploadVersion: false,
        historyEdit: false,
      });
      // The same reads as R1-04a.
      expect.soft(snap.title, 'title').toBe(journey.artifact!.title);
      expect.soft(snap.previewRendered, 'preview rendered').toBe(true);
      expect.soft(snap.versionLabel, 'version menu').toContain('v1 (current)');
      await expect
        .soft(ownerMeta(await pageOf('padmin')), 'owner label names the owner')
        .toHaveText(new RegExp(`^\\s*Owner: (${ownerLabels().map(escapeRegExp).join('|')})\\s*$`));
      r.ui.ownerLabel = ((await ownerMeta(await pageOf('padmin')).textContent()) ?? '').trim();
      const caps = await capabilities('padmin', r);
      expect.soft(caps.status).toBe(200);
      expect.soft(caps.flags, 'padmin flags').toEqual({
        canManage: false,
        canPublish: false,
        canReview: false,
        canGrantReview: false,
      });
    });

    // ---- R1-06: people search, then the write grant through the UI -----
    await row('R1-06a', async (r) => {
      const snap = await snapshotArtifactPage('grantee', 'R1-06a');
      r.ui.snapshot = snap;
      expect.soft(snap.rendered404, 'grantee before the grant: 404 page').toBe(true);
      expect.soft(snap.visible, 'no header controls').toEqual([]);
      const res = await api('grantee', 'GET', `/api/v1/artifacts/${artifactId()}`, undefined, r);
      expect.soft(res.status, 'grantee GET before the grant').toBe(404);
    });

    const ownerPage = await pageOf('owner');
    /** Opens the owner's Share dialog on the artifact page. */
    const openShare = async (): Promise<void> => {
      await snapshotArtifactPage('owner', 'share');
      await headerButton(ownerPage, 'Share').click();
      await expect(shareDialog(ownerPage)).toBeVisible();
      await expect(homeGrantRow(ownerPage), 'grants loaded (home project row)').toBeVisible();
    };
    /** Types text into the people input and returns the search answer. */
    const searchPeople = async (text: string): Promise<{ status: number; ids: string[] }> => {
      const answer = ownerPage.waitForResponse(
        (res) =>
          res.request().method() === 'GET' &&
          new URL(res.url()).pathname === '/api/v1/users' &&
          new URL(res.url()).searchParams.get('search') === text
      );
      await ownerPage.locator(SELECTORS.sharePeopleInput).fill(text);
      const res = await answer;
      const body = (await res.json().catch(() => ({}))) as { users?: Array<{ id: string }> };
      return { status: res.status(), ids: (body.users ?? []).map((u) => u.id) };
    };

    await row(
      'R1-06x',
      async (r) => {
        await openShare();
        const outsider = who('outsider');
        const res = await searchPeople(outsider.email);
        const inAnswer = res.ids.includes(outsider.id);
        // In the answer: wait for the suggestion to render; otherwise read it once.
        const suggested = inAnswer
          ? await shareSuggestion(ownerPage, outsider.email)
              .waitFor({ state: 'visible', timeout: 5_000 })
              .then(() => true)
              .catch(() => false)
          : (await shareSuggestion(ownerPage, outsider.email).count()) > 0;
        const observation = {
          searchStatus: res.status,
          outsiderInAnswer: inAnswer,
          outsiderSuggested: suggested,
          note:
            'observation only (not graded): main searches users hub-wide; whether it should ' +
            'be narrower is a separate product question',
        };
        r.ui.peopleSearch = observation;
        r.api.push({
          as: 'owner',
          method: 'GET',
          path: '/api/v1/users',
          status: res.status,
          code: null,
        });
        journey.evidence.peopleSearch = observation;
        test.info().annotations.push({
          type: 'OBSERVATION',
          description: `R1-06x: outsider (no shared project) ${suggested ? 'IS' : 'is NOT'} offered by the people search (search ${res.status})`,
        });
        await ownerPage.locator(SELECTORS.sharePeopleInput).fill('');
      },
      { requires: ['R1-01'] }
    );

    await row('R1-06b', async (r) => {
      if (!(await shareDialog(ownerPage).isVisible())) await openShare();
      const grantee = who('grantee');
      // The dialog's label: Share “<title>” (artifact-share-dialog.ts:1003).
      r.ui.dialogLabel = await shareDialog(ownerPage).getAttribute('label');
      expect
        .soft(r.ui.dialogLabel, 'Share dialog label')
        .toBe(`Share “${journey.artifact!.title}”`);
      // The home project row: listed, "(home project)", no controls.
      const homeControls = grantRowControls(homeGrantRow(ownerPage));
      r.ui.homeRowControls = {
        selects: await homeControls.selects.count(),
        buttons: await homeControls.buttons.count(),
      };
      expect
        .soft(r.ui.homeRowControls, 'home project row has no controls')
        .toEqual({ selects: 0, buttons: 0 });

      const res = await searchPeople(grantee.email);
      r.api.push({
        as: 'owner',
        method: 'GET',
        path: '/api/v1/users',
        status: res.status,
        code: null,
      });
      r.ui.granteeInSearch = res.ids.includes(grantee.id);
      const suggestion = shareSuggestion(ownerPage, grantee.email);
      // A hidden grantee is a FAIL of this row (and a defect), not a skip.
      await expect(suggestion, 'people search offers the grantee').toBeVisible();
      await suggestion.click();
      const select = ownerPage.locator(SELECTORS.shareAddPermission);
      await select.click();
      await permissionOption(select, 'write').click();
      await expect(select).toHaveJSProperty('value', 'write');
      const granted = ownerPage.waitForResponse(
        (q) =>
          q.request().method() === 'POST' &&
          new URL(q.url()).pathname === `/api/v1/artifacts/${artifactId()}/grants`
      );
      await shareAddButton(ownerPage).click();
      const g = await granted;
      const gBody = (await g.json().catch(() => ({}))) as { grant?: { permission?: string } };
      r.api.push({
        as: 'owner',
        method: 'POST',
        path: `/api/v1/artifacts/${artifactId()}/grants`,
        status: g.status(),
        code: errorCode(gBody),
      });
      r.ui.grantPermission = gBody.grant?.permission ?? null;
      expect.soft([200, 201], 'grant POST status').toContain(g.status());
      expect.soft(gBody.grant?.permission, 'grant permission').toBe('write');
      const grantRowLocator = grantRow(ownerPage, [grantee.displayName, grantee.email]);
      await expect(grantRowLocator, 'grant row for the grantee').toBeVisible();
      await expect
        .soft(grantRowPermission(grantRowLocator), 'grant row at Can edit')
        .toHaveJSProperty('value', 'write');
      await expect.soft(grantRowRemove(grantRowLocator), 'grant row Remove').toBeVisible();
      r.ui.grantRowShown = true;
      await shareCloseButton(ownerPage).click();
      await expect(shareDialog(ownerPage), 'Share dialog closed').toBeHidden();
    });

    await row(
      'R1-06c',
      async (r) => {
        const snap = await snapshotArtifactPage('grantee', 'R1-06c', { history: true });
        r.ui.snapshot = snap;
        expectControls(snap, {
          visible: ['Edit', 'Review', 'Download'],
          hidden: ['Share', 'Grant review access'],
          uploadVersion: true,
        });
        const caps = await capabilities('grantee', r);
        expect.soft(caps.status).toBe(200);
        expect.soft(caps.flags.canPublish, 'grantee canPublish').toBe(true);
        expect.soft(caps.flags.canManage, 'grantee canManage').toBe(false);
        const access = await artifactsListAccess('grantee');
        r.ui.listRow = access;
        expect.soft(access, 'grantee: /artifacts badge').toBe('Shared with you');
      },
      { requires: ['R1-06b'] }
    );

    // ---- R1-07: the grantee publishes v2; ownership unchanged ----------
    await row(
      'R1-07a',
      async (r) => {
        const page = await pageOf('grantee');
        await snapshotArtifactPage('grantee', 'R1-07a');
        await headerButton(page, 'Edit').click();
        const editor = page.locator(SELECTORS.editorContent);
        await expect(editor).toBeVisible();
        await editor.click();
        await page.keyboard.press('Control+End');
        await page.keyboard.type(`${APPENDED_LINE}\n`);
        await page.locator(SELECTORS.editNoteInput).fill('r1 grantee edit');
        const created = page.waitForResponse(
          (q) =>
            q.request().method() === 'POST' &&
            new URL(q.url()).pathname === `/api/v1/artifacts/${artifactId()}/versions`
        );
        await editPublishButton(page).click();
        const c = await created;
        r.api.push({
          as: 'grantee',
          method: 'POST',
          path: `/api/v1/artifacts/${artifactId()}/versions`,
          status: c.status(),
          code: errorCode(await c.json().catch(() => null)),
        });
        expect(c.status(), 'version create').toBe(201);
        await expect(page.locator(SELECTORS.versionMenu)).toContainText('v2 (current)');
        const finalize = who('grantee').requests.find(
          (q) =>
            q.method === 'POST' &&
            q.path === `/api/v1/artifacts/${artifactId()}/versions/2/finalize`
        );
        r.ui.finalizeStatus = finalize?.status ?? null;
        expect.soft(finalize?.status, 'finalize status').toBeGreaterThanOrEqual(200);
        expect.soft(finalize?.status, 'finalize status').toBeLessThan(300);
        await tab(page, 'history').click();
        const v2 = historyRow(page, 2);
        await expect(v2).toBeVisible();
        await expect.soft(historyCurrentBadge(v2), 'v2 current').toBeVisible();
        r.ui.v2PublishedBy = ((await historyPublishedBy(v2).textContent()) ?? '').trim();
        expect.soft(r.ui.v2PublishedBy, 'v2 published by the grantee (shown as You)').toBe('You');
      },
      { requires: ['R1-06c'] }
    );

    await row(
      'R1-07b',
      async (r) => {
        const snap = await snapshotArtifactPage('owner', 'R1-07b');
        r.ui.snapshot = snap;
        expectControls(snap, { visible: ['Share', 'Edit'], hidden: ['Grant review access'] });
        expect.soft(snap.owner, 'owner label unchanged').toBe('Owner: You');
        expect.soft(snap.versionLabel, 'version menu').toContain('v2 (current)');
        await tab(ownerPage, 'history').click();
        const v2 = historyRow(ownerPage, 2);
        await expect(v2).toBeVisible();
        const grantee = who('grantee');
        // Names resolve after the first render (ids until then): poll the cell.
        const granteeLabels = [grantee.displayName, grantee.email].filter((x) => x !== '');
        await expect
          .soft(historyPublishedBy(v2), 'v2 published by the grantee')
          .toHaveText(new RegExp(`^\\s*(${granteeLabels.map(escapeRegExp).join('|')})\\s*$`));
        r.ui.v2PublishedBy = ((await historyPublishedBy(v2).textContent()) ?? '').trim();
        const read = await capabilities('owner', r);
        const body = read.body as {
          artifact?: { ownerKind?: string; ownerRef?: string; currentSeq?: number };
          version?: { seq?: number; createdByKind?: string; createdByRef?: string };
        };
        r.ui.ownership = {
          ownerKind: body.artifact?.ownerKind ?? null,
          ownerRef: body.artifact?.ownerRef ?? null,
          ownerRefAtPublish: journey.artifact?.ownerRefAtPublish ?? null,
          currentSeq: body.artifact?.currentSeq ?? null,
          versionCreatedBy: body.version
            ? `${String(body.version.createdByKind)}:${String(body.version.createdByRef)}`
            : null,
        };
        expect.soft(body.artifact?.ownerKind, 'ownerKind').toBe('user');
        expect
          .soft(body.artifact?.ownerRef, 'ownerRef unchanged')
          .toBe(journey.artifact?.ownerRefAtPublish);
        expect.soft(body.artifact?.ownerRef, 'ownerRef is the owner').toBe(who('owner').id);
        expect.soft(body.artifact?.currentSeq, 'currentSeq').toBe(2);
        expect.soft(body.version?.createdByRef, 'v2 created by the grantee').toBe(grantee.id);
        expect.soft(read.flags.canManage, 'owner still manages').toBe(true);
      },
      { requires: ['R1-07a'] }
    );

    await row(
      'R1-07c',
      async (r) => {
        const snap = await snapshotArtifactPage('member', 'R1-07c', { history: true });
        r.ui.snapshot = snap;
        expectControls(snap, {
          visible: ['Download'],
          hidden: ['Edit', 'Review', 'Share', 'Grant review access'],
          uploadVersion: false,
          historyEdit: false,
        });
        const caps = await capabilities('member', r);
        expect.soft(caps.flags, 'member flags').toEqual({
          canManage: false,
          canPublish: false,
          canReview: false,
          canGrantReview: false,
        });
      },
      { requires: ['R1-06b', 'R1-07a'] }
    );

    // ---- R1-08: Grant review access hidden on a user-owned artifact ----
    await row('R1-08', async (r) => {
      const seen: Record<string, unknown> = {};
      const keys: IdentityKey[] =
        journey.rows['R1-06b']?.status === 'passed'
          ? ['owner', 'member', 'padmin', 'grantee']
          : ['owner', 'member', 'padmin'];
      // Without the grant the grantee leg cannot run: the row records the
      // other legs and ends "not run" (never passed) unless a leg failed.
      if (keys.length < 4) r.legNotRun = 'grantee leg not run: needs R1-06b to pass';
      for (const key of keys) {
        const snap = await snapshotArtifactPage(key, 'R1-08');
        const caps = await capabilities(key, r);
        seen[key] = { visible: snap.visible, canGrantReview: caps.flags.canGrantReview };
        expect.soft(snap.headerRendered, `${key}: header rendered`).toBe(true);
        expect
          .soft(snap.hidden, `${key}: Grant review access hidden`)
          .toContain('Grant review access');
        expect.soft(caps.flags.canGrantReview, `${key}: canGrantReview`).toBe(false);
      }
      expect
        .soft(
          journey.snapshots.filter((s) => s.as === 'owner' && s.step === 'R1-08').at(-1)?.visible,
          'owner has Share instead'
        )
        .toContain('Share');
      r.ui.seen = seen;
      const probe = await api(
        'padmin',
        'POST',
        `/api/v1/artifacts/${artifactId()}/grants`,
        {
          subjectKind: 'principal',
          subjectRef: `user:${who('member').id}`,
          permission: 'review',
        },
        r
      );
      expect.soft(probe.status, 'padmin review-grant probe on a user-owned artifact').toBe(403);
    });

    // Every identity still holds its own test identity session after the rows.
    const identityChecks: Record<string, string> = {};
    for (const key of IDENTITY_KEYS) {
      const identity = journey.ids[key];
      if (!identity) continue;
      try {
        await assertTestIdentity(identity.context.request, {
          email: identity.email,
          role: 'member',
        });
        identityChecks[key] = 'ok';
      } catch (err) {
        identityChecks[key] = firstLine(err);
      }
    }
    journey.evidence.identityChecksAfterRows = identityChecks;
    expect
      .soft(Object.values(identityChecks), 'identities asserted after the rows')
      .toEqual(Object.values(identityChecks).map(() => 'ok'));

    expect(journey.hardFailures, 'rows that could not be carried out').toEqual([]);
  });

  // ---- R1-09/R1-10: cleanup by absence, then the evidence --------------
  // eslint-disable-next-line no-empty-pattern -- Playwright needs the fixtures argument
  test.afterAll(TITLE_CLEANUP, async ({}, testInfo) => {
    const residue: string[] = [];
    const cleanup: Record<string, unknown> = {};
    const t = journey.target;
    const hub = t?.hub;
    const states: Record<string, ReadState | 'absent' | 'present_in_list'> = {};

    for (const identity of Object.values(journey.ids)) {
      for (const page of identity?.context.pages() ?? []) await page.close();
    }

    // The artifact, as its owner (soft delete; grants and links go with it).
    if (journey.artifact && journey.ids.owner) {
      const id = journey.artifact.id;
      try {
        const del = await api('owner', 'DELETE', `/api/v1/artifacts/${id}`);
        cleanup.artifactDelete = del.status;
        if (del.status !== 204) residue.push(`artifact delete answered ${del.status}`);
      } catch (err) {
        residue.push(`cleanup error (artifact delete): ${firstLine(err)}`);
      }
    }

    // Memberships, then both projects, as the owner.
    const removals: Record<string, number | string> = {};
    for (const b of journey.bindings) {
      try {
        const res = await api('owner', 'DELETE', `/api/v1/projects/${b.projectId}/members/${b.id}`);
        removals[`${b.key}@${b.project}`] = res.status;
        if (res.status >= 300)
          residue.push(`membership ${b.key}@${b.project}: DELETE ${res.status}`);
      } catch (err) {
        removals[`${b.key}@${b.project}`] = `error: ${firstLine(err)}`;
        residue.push(`cleanup error (membership ${b.key}@${b.project}): ${firstLine(err)}`);
      }
    }
    cleanup.memberRemovals = removals;
    // Each removed binding re-read by absence, before the projects go.
    for (const name of ['home', 'side'] as const) {
      const projectId = journey.projects[name];
      const mine = journey.bindings.filter((b) => b.project === name);
      if (!projectId || mine.length === 0) continue;
      try {
        const res = await api('owner', 'GET', `/api/v1/projects/${projectId}/members`);
        const items =
          ((res.body ?? {}) as { items?: Array<{ id: string; principalId?: string }> }).items ?? [];
        for (const b of mine) {
          const userId = journey.ids[b.key]?.id;
          const present = items.some(
            (i) => i.id === b.id || (!!userId && i.principalId === userId)
          );
          states[`binding ${b.key}@${name}`] =
            res.status === 200 ? (present ? 'present' : 'absent') : 'unknown';
        }
      } catch (err) {
        residue.push(`cleanup error (members of ${name}): ${firstLine(err)}`);
      }
    }
    const projectDeletes: Record<string, number | string> = {};
    for (const name of ['home', 'side'] as const) {
      const projectId = journey.projects[name];
      if (!projectId) continue;
      try {
        const res = await api('owner', 'DELETE', `/api/v1/projects/${projectId}`);
        projectDeletes[name] = res.status;
        if (res.status >= 300) residue.push(`project ${name}: DELETE ${res.status}`);
      } catch (err) {
        projectDeletes[name] = `error: ${firstLine(err)}`;
        residue.push(`cleanup error (project ${name}): ${firstLine(err)}`);
      }
    }
    cleanup.projectDeletes = projectDeletes;

    // Final reads while the sessions still exist.
    try {
      if (journey.artifact && journey.ids.owner) {
        const id = journey.artifact.id;
        const own = await api('owner', 'GET', `/api/v1/artifacts/${id}`);
        states['artifact (owner read)'] = classifyRead(own.status, own.body, 'artifact');
        const list = await api('owner', 'GET', '/api/v1/artifacts?mine=1');
        const listed = ((list.body ?? {}) as { artifacts?: Array<{ id: string }> }).artifacts?.some(
          (a) => a.id === id
        );
        states['artifact (owner list)'] =
          list.status === 200 && !listed ? 'absent' : 'present_in_list';
        if (journey.ids.grantee) {
          const g = await api('grantee', 'GET', `/api/v1/artifacts/${id}`);
          states['artifact (grantee read)'] = classifyRead(g.status, g.body, 'artifact');
        }
      }
      for (const name of ['home', 'side'] as const) {
        const projectId = journey.projects[name];
        if (!projectId || !journey.ids.owner) continue;
        const res = await api('owner', 'GET', `/api/v1/projects/${projectId}`);
        states[`project ${name}`] = classifyRead(res.status, res.body);
      }
    } catch (err) {
      residue.push(`cleanup error (final reads): ${firstLine(err)}`);
    }

    // The experiment override back to its stored default.
    if (t && adminToken()) {
      try {
        const after = await setExperiment(null);
        cleanup.experimentReset = after;
        const experiment = journey.evidence.experiment as Record<string, unknown> | undefined;
        if (experiment) experiment.afterReset = after;
        if (after.override !== null) residue.push('hub.artifacts override still set');
      } catch (err) {
        residue.push(`cleanup error (experiment reset): ${firstLine(err)}`);
      }
    }

    // The identities (local hub). Remote: the runner deletes them later.
    if (hub) {
      const authMe: Record<string, number> = {};
      const deleted: Record<string, string> = {};
      for (const identity of Object.values(journey.ids)) {
        if (!identity) continue;
        try {
          runScionCLI(hub, ['hub', 'test-identity', 'delete', identity.id]);
          deleted[identity.key] = 'deleted';
        } catch (err) {
          deleted[identity.key] = `delete failed: ${firstLine(err)}`;
          residue.push(`identity ${identity.key}: ${firstLine(err)}`);
        }
        try {
          const me = await identity.context.request.get('/auth/me', { maxRedirects: 0 });
          authMe[identity.key] = me.status();
          if (me.status() !== 401)
            residue.push(`identity ${identity.key}: session still answers /auth/me`);
          await me.dispose();
        } catch (err) {
          residue.push(`cleanup error (identity ${identity.key} /auth/me): ${firstLine(err)}`);
        }
      }
      cleanup.identity = deleted;
      cleanup.authMeAfterDelete = authMe;
      try {
        const listed = runScionCLI(hub, [
          'hub',
          'test-identity',
          'list',
          '--include-expired',
          '--json',
        ]);
        for (const identity of Object.values(journey.ids)) {
          if (!identity) continue;
          const present = listed.includes(identity.id);
          states[`identity ${identity.key}`] = present ? 'present' : 'absent';
        }
      } catch (err) {
        residue.push(`cleanup error (identity list): ${firstLine(err)}`);
      }
    } else if (t?.mode === 'remote') {
      cleanup.identity =
        'remote target: NOT verified here; the runner deletes the identities out of band, ' +
        'and this evidence is written before that';
    }

    for (const [what, state] of Object.entries(states)) {
      if (state !== 'absent' && !(state !== 'present_in_list' && isGone(state))) {
        residue.push(`final: ${what} is ${state}`);
      }
    }
    cleanup.final = { states };

    // Request logs and snapshots, then close the contexts.
    journey.evidence.requests = Object.fromEntries(
      Object.values(journey.ids)
        .filter((i): i is Identity => !!i)
        .map((i) => [i.key, i.requests])
    );
    journey.evidence.stateSequence = journey.snapshots;
    for (const identity of Object.values(journey.ids)) await identity?.context.close();
    if (journey.tmpDir) fs.rmSync(journey.tmpDir, { recursive: true, force: true });

    // Rows nothing reached keep "not run", with the first hard failure as the cause.
    const cause =
      journey.setupError ?? journey.stopped ?? journey.hardFailures[0] ?? journey.testError;
    for (const r of Object.values(journey.rows)) {
      if (r.status === 'not run' && r.reason === NOT_REACHED && cause) {
        r.reason = `${NOT_REACHED}: ${cause}`;
      }
    }
    notRun(
      R1B_IDS.filter((id) => journey.rows[id]?.reason?.startsWith(NOT_REACHED)),
      R1B_REASON
    );

    // R1-09: cleanup verified by absence.
    const r09 = rowRecord('R1-09');
    delete r09.reason;
    r09.status = residue.length === 0 ? 'passed' : 'failed';
    r09.errors = [...residue];
    r09.ui = { final: cleanup.final };

    // Evidence only now, after the cleanup.
    journey.evidence.artifact = journey.artifact
      ? { id: journey.artifact.id, ownerRefAtPublish: journey.artifact.ownerRefAtPublish ?? null }
      : null;
    journey.evidence.rows = journey.rows;
    journey.evidence.findings = journey.findings;
    journey.evidence.hardFailures = journey.hardFailures;
    journey.evidence.setupError = journey.setupError ?? null;
    journey.evidence.testError = journey.testError ?? null;
    journey.evidence.cleanup = { ...cleanup, residue };
    const results = (journey.evidence.results ??= {}) as Record<string, unknown>;
    results[TITLE_A] ??= { status: 'not run', skipReason: cause ?? null };
    for (const id of R1B_IDS) {
      results[r1bTitle(id)] ??= { status: 'not run', skipReason: R1B_REASON };
    }
    results[TITLE_CLEANUP] = {
      status: residue.length === 0 ? 'passed' : 'failed',
      residue: residue.length,
      annotations: annotationsOf(testInfo.annotations),
    };

    // R1-10: the evidence holds nothing secret-shaped. Checked on the
    // serialised evidence before it is written; the row goes in with it.
    const r10 = rowRecord('R1-10');
    delete r10.reason;
    const leaks = secretLikeStrings(JSON.stringify(journey.evidence));
    r10.status = leaks.length === 0 ? 'passed' : 'failed';
    r10.ui = {
      secretLikeMatches: leaks.length,
      traces: 'off',
      video: 'off',
      screenshots: 'off',
      har: 'not recorded',
    };
    r10.errors = leaks.map((l) => `secret-like string at offset ${l}`);
    const body = JSON.stringify(journey.evidence, null, 2);
    // The spec's path: one file in the suite's output dir, not a test's subdir.
    const evidenceFile = path.join(testInfo.project.outputDir, 'artifacts-access-evidence.json');
    fs.mkdirSync(path.dirname(evidenceFile), { recursive: true });
    fs.writeFileSync(evidenceFile, body);
    console.info(`[journey] evidence: ${evidenceFile}`);
    console.info(`[journey] cleanup: ${JSON.stringify(journey.evidence.cleanup)}`);

    expect(leaks, 'R1-10: no secret-like strings in the evidence').toEqual([]);
    expect(residue, 'R1-09: nothing left behind').toEqual([]);
  });
});
