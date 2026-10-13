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
 * Artifact page view and render (R2), graded at the UI, end to end on a
 * throwaway hub with no broker.
 *
 * Three member test identities, each in its own browser context: owner
 * (publishes everything; owns the fixture project), member (the reader: a
 * project member with no grant) and outsider (no project). Rows, in order:
 *
 * - R2-01: the owner publishes an HTML bundle (index, a linked page, CSS,
 *   an image) as a folder through New artifact. A refused publish STOPS
 *   the run with a FINDING; every later row is then "not run".
 * - R2-02..06: the bundle's sandboxed frame: its attributes and the view
 *   response's policy; the bundle renders with its own files; a relative
 *   link stays on the version; the frame cannot reach the app page, the
 *   hub session or the top page; Open in new tab stays sandboxed.
 * - R2-07/07b/08: a markdown bundle renders in its frame, a clicked link
 *   opens a new tab, and raw HTML and script in it are inert.
 * - R2-09..12: v2 and v3 through Upload new version; the history in order
 *   with the publisher; older versions show their own content and links;
 *   the Files tab and Download.
 * - R2-13..16: the reader sees the same pages.
 * - R2-17a: a view URL works without a session (by design, until it
 *   expires). R2-17b (expiry) is NOT RUN: the view lifetime is a fixed
 *   constant with no supported setting to lower it.
 * - R2-20: markdown in the chat preview. R2-21 (an HTML bundle in the chat
 *   preview) is NOT RUN: that preview is not on this build.
 * - R2-18/19 (afterAll): cleanup verified by absence, then the evidence.
 *
 * The UI observation decides each row; the API statuses are recorded and
 * must agree. Row assertions are soft. The isolation rows (R2-05, R2-06,
 * R2-13) report failures with one generic message only; their detail is
 * kept in the structured evidence fields.
 */

import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import {
  expect,
  test,
  type Browser,
  type BrowserContext,
  type Frame,
  type Page,
  type Request,
} from '@playwright/test';

import {
  EXPERIMENT,
  NOT_REACHED,
  createJourney,
  isShown,
  type Identity,
  type RowRecord,
  type RowSpec,
} from './journey-core.js';
import {
  annotationsOf,
  classifyRead,
  firstLine,
  isGone,
  redactPath,
  secretLikeStrings,
  type ReadState,
} from './journey-lib.js';
import {
  API_PROBE_PATH,
  CHART_SIZE,
  CSS_PROBE,
  DOT_SIZE,
  PROBE_PATHS,
  REMOTE_IMAGE_HOST,
  bundleFiles,
  markdownFiles,
  writeFolder,
} from './render-fixtures.js';
import {
  RENDER_SELECTORS,
  fileIconButton,
  fileRow,
  historyViewButton,
  htmlFrame,
  markdownFrame,
  previewBarButton,
  publishFolderMode,
  remoteImagesNotice,
  versionMenuItem,
} from './render-selectors.js';
import {
  SELECTORS,
  anyPageHeading,
  headerButton,
  headerTitle,
  newArtifactButton,
  notFoundHeading,
  ownerMeta,
  publishSubmit,
  tab,
  uploadVersionButton,
} from './selectors.js';

const KEYS = ['owner', 'member', 'outsider'] as const;
type Key = (typeof KEYS)[number];

/** The only failure text of the isolation rows in any Playwright output. */
const ISOLATION_FAIL = 'R2 isolation row failed; see restricted evidence';

const R2_17B_REASON =
  'NOT RUN: the view TTL is a fixed 30-minute constant (pkg/artifacts/view.go:52) with no ' +
  'supported setting to lower it; the run does not wait 30 minutes';
const R2_21_REASON =
  'NOT RUN: chat HTML preview not merged (upstream ptone/scion#4580)';

const VIEW_PREFIX = '/api/v1/artifacts/view/';

const ROWS: Record<string, RowSpec> = {
  'R2-01': { role: 'owner', title: 'HTML bundle v1 published as a folder through New artifact' },
  'R2-02': { role: 'owner', title: 'HTML frame attributes, bar and view response policy' },
  'R2-03': { role: 'owner', title: 'the bundle renders with its own CSS and image' },
  'R2-04': { role: 'owner', title: 'a relative link inside the frame stays on the version' },
  'R2-05': {
    role: 'owner',
    title: 'isolation negatives of the bundle frame',
    genericError: ISOLATION_FAIL,
  },
  'R2-06': {
    role: 'owner',
    title: 'Open in new tab stays sandboxed',
    genericError: ISOLATION_FAIL,
  },
  'R2-07': { role: 'owner', title: 'markdown bundle renders in its sandboxed frame' },
  'R2-07b': { role: 'owner', title: 'a clicked markdown link opens a new tab; no script runs' },
  'R2-08': { role: 'owner', title: 'raw HTML and script in markdown are inert' },
  'R2-09': { role: 'owner', title: 'Upload new version publishes v2 and v3' },
  'R2-10': { role: 'owner', title: 'history lists v3, v2, v1 with the publisher' },
  'R2-11': { role: 'owner', title: 'older versions show their own content and links' },
  'R2-12': { role: 'owner', title: 'Files tab lists the files; Download works' },
  'R2-13': {
    role: 'member',
    title: 'reader: bundle page and sandboxed frame',
    genericError: ISOLATION_FAIL,
  },
  'R2-14': { role: 'member', title: 'reader sees the same history and older versions' },
  'R2-15': { role: 'member', title: 'reader: Files tab and Download' },
  'R2-16': { role: 'member', title: 'reader: markdown renders and stays inert' },
  'R2-17': { role: 'outsider', title: 'outsider: 404 page and no view' },
  'R2-17a': { role: 'anonymous', title: 'a held view URL works without a session' },
  'R2-17b': {
    role: 'anonymous',
    title: 'a held view URL is refused after it expires',
    where: 'skip',
  },
  'R2-20': { role: 'member', title: 'markdown in the chat preview' },
  'R2-20x': {
    role: 'owner -> member',
    title: 'recorded only: chips, references the hub recorded, artifactWarning',
    graded: false,
  },
  'R2-21': { role: 'member', title: 'HTML bundle in the chat preview', where: 'skip' },
  'R2-18': {
    role: '-',
    title: 'cleanup by absence: artifacts, membership, project, identities, temp files',
    where: 'cleanup',
  },
  'R2-19': {
    role: '-',
    title: 'evidence after the cleanup; no traces, cookies, tokens or view capabilities',
    where: 'cleanup',
  },
};

const j = createJourney<Key>({
  keys: KEYS,
  rows: ROWS,
  remotePrefix: 'E2E_R2',
  purpose: 'e2e artifact render',
});

/** One request made by an HTML bundle document (frame or top-level view). */
interface FrameRequest {
  who: Key | 'anonymous';
  step: string;
  method: string;
  path: string;
  resourceType: string;
  status?: number;
  failed?: boolean;
  /** The page's own top-level document (Open in new tab), not a frame request. */
  topLevelDocument: boolean;
  /** The request's frame was not available (a navigation issued before its frame existed). */
  frameUnavailable?: boolean;
  /**
   * The Cookie header as sent: read is false until the browser's full
   * request headers were read. An unread header is unknown, never "no
   * session".
   */
  cookieHeader: { read: boolean; present: boolean | null; names: string[] };
}

/** A view mint seen by the browser: status, seq and the URL (in memory only). */
interface Mint {
  who: Key;
  seq: number;
  status: number;
  remoteImages: boolean | null;
  expiresAt: string | null;
  /** The view URL: a bearer capability. Never written to the evidence. */
  url: string | null;
}

const r2 = {
  step: 'setup',
  experimentSetByRun: false,
  bundle: undefined as { id: string; title: string } | undefined,
  markdown: undefined as { id: string; title: string } | undefined,
  dirs: {} as Record<string, string>,
  fileSizes: {} as Record<string, number>,
  frameRequests: [] as FrameRequest[],
  /** Header reads still in flight (awaited before any session check). */
  pendingHeaderReads: [] as Array<Promise<void>>,
  /** Requests from any page to the bundle's remote image host (host only kept). */
  remoteHostHits: 0,
  mints: [] as Mint[],
  /** Set when the markdown publish was refused: its rows are not run with this cause. */
  markdownRefused: undefined as string | undefined,
  /** The markdown artifact's id from its create (for the cleanup). */
  markdownCreatedId: undefined as string | undefined,
  /** Any other artifact id seen on landing (cleaned up as well). */
  extraArtifactIds: [] as string[],
  /** v3 exists: its versions POST answered 201 and the page showed v3 (current). */
  v3Published: false,
  /** Saved download files (in memory), checked absent after delete and in the cleanup. */
  downloads: [] as Array<{ who: Key; file: string }>,
  /** A view URL minted for the owner (in memory only), for R2-17a and R2-18. */
  heldViewUrl: undefined as string | undefined,
  frames: [] as Array<Record<string, unknown>>,
  probes: [] as Array<Record<string, unknown>>,
  publishes: [] as Array<Record<string, unknown>>,
  history: {} as Record<string, unknown>,
  files: {} as Record<string, unknown>,
  markdownInert: [] as Array<Record<string, unknown>>,
  consoleBlocked: [] as Array<{ who: string; count: number }>,
  consoleCounts: {} as Record<string, number>,
  /**
   * Restricted: set when a frame request carried the viewer's session, or
   * could not be shown not to carry it (an unread header, or no sweep).
   */
  frameSessionAlert: false,
  /** Restricted: the whole-run frame session sweep; swept false means unknown. */
  frameSessionAll: { swept: false } as Record<string, number | boolean>,
  chat: {} as Record<string, unknown>,
};

function suffix(): string {
  return crypto.randomBytes(3).toString('hex');
}

function bundleId(): string {
  if (!r2.bundle) throw new Error('no bundle published');
  return r2.bundle.id;
}

function markdownId(): string {
  if (!r2.markdown) throw new Error('no markdown published');
  return r2.markdown.id;
}

function artifactPage(id: string, seq = 0): string {
  const base = `/projects/${j.state.projectId}/artifacts/${id}`;
  return seq > 0 ? `${base}/v/${seq}` : base;
}

/** Replaces every view capability in text with <redacted>. */
function redactViews(text: string): string {
  return text.replace(/\/api\/v1\/artifacts\/view\/[^/\s;'"]+/g, `${VIEW_PREFIX}<redacted>`);
}

/** What a view URL's capability names, without keeping the capability. */
function viewInfo(
  url: string | null | undefined,
  id: string
): { viewSeq: number | null; viewIdMatches: boolean; entry: string | null } {
  if (!url) return { viewSeq: null, viewIdMatches: false, entry: null };
  const pathname = new URL(url, 'http://x').pathname;
  if (!pathname.startsWith(VIEW_PREFIX)) {
    return { viewSeq: null, viewIdMatches: false, entry: null };
  }
  const [cap, ...rest] = pathname.slice(VIEW_PREFIX.length).split('/');
  const parts = (cap ?? '').split('.');
  return {
    viewSeq: parts.length >= 4 ? Number(parts[1]) : null,
    viewIdMatches: parts[0] === id,
    entry: rest.join('/') || null,
  };
}

/** The capability segment of a view URL (in memory only). */
function capabilityOf(url: string): string {
  return new URL(url, 'http://x').pathname.slice(VIEW_PREFIX.length).split('/')[0] ?? '';
}

function cookieNames(header: string | undefined): string[] {
  if (!header) return [];
  return header
    .split(';')
    .map((c) => c.split('=')[0]?.trim() ?? '')
    .filter((n) => n !== '');
}

/** True for a request made by an HTML bundle document (sandboxed frame or a top-level view). */
function isBundleRequest(req: Request): boolean {
  const reqPath = new URL(req.url()).pathname;
  if (reqPath.startsWith(VIEW_PREFIX)) return true;
  try {
    return req.frame().url().includes(VIEW_PREFIX);
  } catch {
    // A navigation issued before its frame exists: only its path can tell.
    return false;
  }
}

/** Reads the full request headers of a record once (cookie names only). */
function readCookieHeader(req: Request, rec: FrameRequest): void {
  const p = req
    .allHeaders()
    .then((h) => {
      rec.cookieHeader = { read: true, present: !!h.cookie, names: cookieNames(h.cookie) };
    })
    .catch(() => undefined);
  r2.pendingHeaderReads.push(p);
}

/**
 * Records the bundle requests of a context (path redacted, status, and the
 * Cookie header as sent, by cookie name only), any request to the remote
 * image host, and every view mint answer (status, seq and the URL in
 * memory).
 */
function watchContext(context: BrowserContext, who: Key | 'anonymous'): void {
  const records = new Map<Request, FrameRequest>();
  context.on('request', (req) => {
    if (!/^https?:/.test(req.url())) return;
    if (new URL(req.url()).hostname === REMOTE_IMAGE_HOST) r2.remoteHostHits++;
    if (!isBundleRequest(req)) return;
    let topLevelDocument = false;
    let frameUnavailable = false;
    try {
      topLevelDocument = req.isNavigationRequest() && req.frame().parentFrame() === null;
    } catch {
      // Only a navigation issued before its frame exists has no frame: a new
      // page's top-level document (an iframe's element always exists first).
      frameUnavailable = true;
      topLevelDocument = req.isNavigationRequest();
    }
    const rec: FrameRequest = {
      who,
      step: r2.step,
      method: req.method(),
      path: redactPath(new URL(req.url()).pathname),
      resourceType: req.resourceType(),
      topLevelDocument,
      ...(frameUnavailable ? { frameUnavailable } : {}),
      cookieHeader: { read: false, present: null, names: [] },
    };
    records.set(req, rec);
    r2.frameRequests.push(rec);
    readCookieHeader(req, rec);
  });
  context.on('response', (res) => {
    const rec = records.get(res.request());
    if (rec) {
      rec.status = res.status();
      if (!rec.cookieHeader.read) readCookieHeader(res.request(), rec);
      if (rec.frameUnavailable) {
        // The frame exists by now: classify from it rather than the fallback.
        try {
          rec.topLevelDocument =
            res.request().isNavigationRequest() && res.frame().parentFrame() === null;
          delete rec.frameUnavailable;
        } catch {
          // Still unavailable: the fallback stands (counted in the sweep).
        }
      }
    }
    if (who === 'anonymous') return;
    const req = res.request();
    const m = /^\/api\/v1\/artifacts\/[^/]+\/versions\/(\d+)\/view$/.exec(
      new URL(res.url()).pathname
    );
    if (req.method() !== 'POST' || !m) return;
    const mint: Mint = {
      who,
      seq: Number(m[1]),
      status: res.status(),
      remoteImages: null,
      expiresAt: null,
      url: null,
    };
    r2.mints.push(mint);
    void res
      .json()
      .then((b: { url?: string; remoteImages?: boolean; expiresAt?: string }) => {
        mint.url = b.url ?? null;
        mint.remoteImages = !!b.remoteImages;
        mint.expiresAt = b.expiresAt ?? null;
      })
      .catch(() => undefined);
  });
  context.on('requestfailed', (req) => {
    const rec = records.get(req);
    if (rec) rec.failed = true;
  });
}

/**
 * The session facts of frame requests (not a page's own top-level
 * document), after every pending header read settled (bounded wait). An
 * unread header counts as unknown, which fails a session check.
 */
async function frameSessionFacts(records: FrameRequest[]): Promise<{
  total: number;
  unread: number;
  bearing: number;
  unverified: number;
}> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  await Promise.race([
    Promise.allSettled([...r2.pendingHeaderReads]),
    new Promise((resolve) => {
      timer = setTimeout(resolve, 5_000);
    }),
  ]);
  if (timer) clearTimeout(timer);
  const frameOnly = records.filter((q) => !q.topLevelDocument);
  return {
    total: frameOnly.length,
    unread: frameOnly.filter((q) => !q.cookieHeader.read).length,
    bearing: frameOnly.filter((q) => q.cookieHeader.present === true).length,
    // Recorded only: a failed or unanswered request's headers may be the
    // provisional ones, which never show a Cookie header.
    unverified: frameOnly.filter((q) => q.failed || q.status === undefined).length,
  };
}

/** Bundle requests of one identity in the given steps. */
function requestsIn(who: Key | 'anonymous', steps: string[]): FrameRequest[] {
  const set = new Set(steps);
  return r2.frameRequests.filter((q) => q.who === who && set.has(q.step));
}

/** Lets the async header and body reads of the request listeners finish. */
async function settle(page: Page): Promise<void> {
  await page.waitForTimeout(500);
}

/**
 * Reveals the project page's Files area (it renders as a placeholder and is
 * replaced once scrolled into view, project-detail.ts:1407-1445).
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

/** Text of a locator with whitespace collapsed. */
async function textOf(locator: ReturnType<Page['locator']>): Promise<string> {
  return ((await locator.first().textContent()) ?? '').replace(/\s+/g, ' ').trim();
}

/** The owner's label as other identities see it. */
function ownerLabels(): string[] {
  const o = j.who('owner');
  return [o.displayName, o.email].filter((x) => x !== '');
}

/** Opens an artifact page as key and waits for the header (or 404) to render. */
async function openArtifact(key: Key, url: string): Promise<Page> {
  const page = await j.pageOf(key);
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await anyPageHeading(page).waitFor({ state: 'visible' });
  return page;
}

/** The HTML frame element's Frame, once its document has loaded. */
async function bundleFrame(page: Page): Promise<Frame> {
  const el = await page.locator(RENDER_SELECTORS.htmlFrame).elementHandle();
  if (!el) throw new Error('no HTML frame');
  const frame = await el.contentFrame();
  if (!frame) throw new Error('HTML frame has no content frame');
  await frame.waitForLoadState('load');
  return frame;
}

/** Attributes of the HTML frame element. */
async function htmlFrameAttributes(page: Page): Promise<{
  sandbox: string | null;
  referrerpolicy: string | null;
  title: string | null;
  src: string | null;
}> {
  return page.locator(RENDER_SELECTORS.htmlFrame).evaluate((f) => ({
    sandbox: f.getAttribute('sandbox'),
    referrerpolicy: f.getAttribute('referrerpolicy'),
    title: f.getAttribute('title'),
    src: f.getAttribute('src'),
  }));
}

/**
 * The probe result of a bundle document, polled until done:true. Its
 * failure message carries no page content (it is used by isolation rows).
 */
async function readProbe(target: Frame | Page): Promise<Record<string, unknown>> {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    const text = await target
      .evaluate(() => document.getElementById('probe')?.textContent ?? '')
      .catch(() => '');
    if (text.includes('"done":true')) return JSON.parse(text) as Record<string, unknown>;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error('probe not done');
}

/** The current heading of the bundle frame. */
async function frameHeading(page: Page): Promise<string> {
  return textOf(htmlFrame(page).locator('#ver'));
}

const SANDBOX_FORBIDDEN_HTML = [
  'allow-same-origin',
  'allow-top-navigation',
  'allow-top-navigation-by-user-activation',
  'allow-top-navigation-to-custom-protocols',
  'allow-popups',
  'allow-popups-to-escape-sandbox',
  'allow-forms',
  'allow-modals',
];

function tokens(sandbox: string | null): string[] {
  return (sandbox ?? '')
    .split(/\s+/)
    .filter((t) => t !== '')
    .sort();
}

/** Waits for the latest mint of seq by key and returns it (URL in memory). */
async function latestMint(key: Key, seq: number, since = 0): Promise<Mint> {
  const find = (): Mint | undefined =>
    r2.mints
      .slice(since)
      .reverse()
      .find((m) => m.who === key && m.seq === seq && m.url !== null);
  await expect.poll(find, { message: `view minted for v${seq}` }).toBeTruthy();
  return find()!;
}

/** Headers of a view response, fetched in key's session, with the capability redacted. */
async function viewHeaders(
  key: Key,
  viewUrl: string,
  rec: RowRecord
): Promise<Record<string, string | number>> {
  const res = await j.who(key).context.request.get(viewUrl, { maxRedirects: 0 });
  const h = res.headers();
  await res.dispose();
  rec.api.push({
    as: key,
    method: 'GET',
    path: redactPath(new URL(viewUrl, j.target().baseURL).pathname),
    status: res.status(),
    code: null,
  });
  return {
    status: res.status(),
    csp: redactViews(h['content-security-policy'] ?? ''),
    xContentTypeOptions: h['x-content-type-options'] ?? '',
    referrerPolicy: h['referrer-policy'] ?? '',
    contentDisposition: h['content-disposition'] ?? '',
  };
}

/** Publish requests of an identity since index: create, uploads, finalize (h2). */
function publishCalls(
  key: Key,
  since: number,
  seq: number
): Array<{ method: string; path: string; status: number | null }> {
  return j
    .who(key)
    .requests.slice(since)
    .filter(
      (q) =>
        (q.method === 'POST' && q.path === '/api/v1/artifacts') ||
        (q.method === 'POST' && /^\/api\/v1\/artifacts\/[^/]+\/versions$/.test(q.path)) ||
        (q.method === 'PUT' && new RegExp(`/versions/${seq}/files/`).test(q.path)) ||
        (q.method === 'POST' && q.path.endsWith(`/versions/${seq}/finalize`))
    )
    .map((q) => ({ method: q.method, path: q.path, status: q.status ?? null }));
}

/**
 * Publishes a folder through the publish dialog that is already open, then
 * waits for the page to land on the artifact. Returns the create status.
 */
async function publishThroughDialog(
  page: Page,
  dir: string,
  opts: { title?: string; note: string; createPath: RegExp }
): Promise<{
  createStatus: number;
  createCode: string | null;
  entry: string;
  asked: string[];
  createdId: string | null;
}> {
  await expect(page.locator(SELECTORS.publishDialog)).toBeVisible();
  await publishFolderMode(page).click();
  await page.locator(RENDER_SELECTORS.publishFolderInput).setInputFiles(dir);
  const entrySelect = page.locator(RENDER_SELECTORS.publishEntrySelect);
  await expect(entrySelect).toBeVisible();
  const entry = await entrySelect.evaluate((el) => {
    const v = (el as HTMLElement & { value: string }).value;
    return el.querySelector(`sl-option[value="${v}"]`)?.textContent?.trim() ?? '';
  });
  if (opts.title !== undefined) {
    await page.locator(SELECTORS.publishTitleInput).fill(opts.title);
  }
  await page.locator(RENDER_SELECTORS.publishNoteInput).fill(opts.note);
  const createResponse = page.waitForResponse(
    (res) => res.request().method() === 'POST' && opts.createPath.test(new URL(res.url()).pathname)
  );
  await publishSubmit(page).click();
  const created = await createResponse;
  const body = (await created.json().catch(() => null)) as {
    error?: { code?: string };
    code?: string;
    upload?: { required?: string[] };
    artifact?: { id?: string };
  } | null;
  const code = body?.error?.code ?? body?.code ?? null;
  // The files the hub asked to upload (the rest it already holds).
  const asked = body?.upload?.required ?? [];
  const createdId = created.status() === 201 ? (body?.artifact?.id ?? null) : null;
  return { createStatus: created.status(), createCode: code, entry, asked, createdId };
}

/** Rows of the History tab in order. */
async function historyRows(page: Page): Promise<
  Array<{
    seq: string;
    current: boolean;
    kind: string;
    publishedBy: string;
    note: string;
    files: string;
    view: boolean;
  }>
> {
  await tab(page, 'history').click();
  await page.locator(RENDER_SELECTORS.historyVersionRow).first().waitFor({ state: 'visible' });
  return page.locator(RENDER_SELECTORS.historyVersionRow).evaluateAll((rows) =>
    rows.map((tr) => {
      const cells = Array.from(tr.querySelectorAll(':scope > td'));
      const t = (i: number): string => (cells[i]?.textContent ?? '').replace(/\s+/g, ' ').trim();
      return {
        seq: (tr.querySelector('strong')?.textContent ?? '').trim(),
        current: Array.from(cells[0]?.querySelectorAll('sl-badge') ?? []).some(
          (b) => (b.textContent ?? '').trim() === 'current'
        ),
        kind: t(1),
        publishedBy: t(2),
        note: t(4),
        files: t(5),
        view: !!tr.querySelector('sl-icon-button[label^="View v"]'),
      };
    })
  );
}

/** Rows of the Files tab in order. */
async function filesRows(page: Page): Promise<{
  summary: string;
  rows: Array<{
    path: string;
    size: string;
    type: string;
    entry: boolean;
    open: string;
    openLabel: string;
  }>;
}> {
  await tab(page, 'files').click();
  await page.locator(RENDER_SELECTORS.filesSummary).waitFor({ state: 'visible' });
  const summary = await textOf(page.locator(RENDER_SELECTORS.filesSummary));
  const rows = await page.locator(RENDER_SELECTORS.filesRow).evaluateAll((trs) =>
    trs.map((tr) => {
      const cells = Array.from(tr.querySelectorAll(':scope > td'));
      const t = (i: number): string => (cells[i]?.textContent ?? '').replace(/\s+/g, ' ').trim();
      const fp = tr.querySelector('.file-path');
      const badge = fp?.querySelector('sl-badge');
      const pathText = ((fp?.textContent ?? '').replace(badge?.textContent ?? '', '') ?? '')
        .replace(/\s+/g, ' ')
        .trim();
      const eye = tr.querySelector('sl-icon-button[name="eye"]');
      return {
        path: pathText,
        size: t(1),
        type: t(2),
        entry: (badge?.textContent ?? '').trim() === 'entry',
        open: eye?.hasAttribute('disabled') ? 'disabled' : 'enabled',
        openLabel: eye?.getAttribute('label') ?? '',
      };
    })
  );
  return { summary, rows };
}

/** Downloads a file from the Files tab and returns filename and byte size (file deleted). */
async function downloadFile(
  page: Page,
  who: Key,
  filePath: string
): Promise<{
  filename: string;
  bytes: number;
  requestStatus: number | null;
  savedAbsentAfterDelete: boolean;
}> {
  const row = fileRow(page, filePath);
  const button = fileIconButton(row, `Download ${filePath}`);
  await expect(button).toBeVisible();
  const [download] = await Promise.all([page.waitForEvent('download'), button.click()]);
  const saved = await download.path();
  const bytes = saved ? fs.statSync(saved).size : -1;
  const filename = download.suggestedFilename();
  await download.delete();
  // The saved file is kept in memory by path only, and checked again in the cleanup.
  if (saved) r2.downloads.push({ who, file: saved });
  const savedAbsentAfterDelete = !!saved && !fs.existsSync(saved);
  return { filename, bytes, requestStatus: null, savedAbsentAfterDelete };
}

/** Markdown frame facts, read through the parent page (the frame is same-origin by design). */
async function markdownFacts(
  page: Page,
  selector: string = RENDER_SELECTORS.markdownIframe
): Promise<Record<string, unknown>> {
  return page.locator(selector).evaluate((f) => {
    const iframe = f as HTMLIFrameElement;
    const doc = iframe.contentDocument;
    const all = doc ? Array.from(doc.querySelectorAll('*')) : [];
    const onAttrs = all.filter((el) =>
      Array.from(el.attributes).some((a) => a.name.toLowerCase().startsWith('on'))
    ).length;
    const img = doc?.querySelector('img[alt="dot"]') as HTMLImageElement | null;
    const notes = Array.from(doc?.querySelectorAll('a') ?? []).find(
      (a) => (a.textContent ?? '').trim() === 'notes'
    );
    const jslink = Array.from(doc?.querySelectorAll('a') ?? []).find(
      (a) => (a.textContent ?? '').trim() === 'js link'
    );
    const meta = doc?.querySelector('meta[http-equiv="Content-Security-Policy"]');
    return {
      sandbox: iframe.getAttribute('sandbox'),
      referrerpolicy: iframe.getAttribute('referrerpolicy'),
      hasSrcAttr: iframe.hasAttribute('src'),
      srcdocSet: (iframe.srcdoc ?? '').length > 0,
      metaCsp: meta?.getAttribute('content') ?? null,
      h1: (doc?.querySelector('h1')?.textContent ?? '').trim(),
      markerVisible: (doc?.body?.textContent ?? '').includes('Plain text marker r2-md-ok.'),
      hoverMarker: (doc?.body?.textContent ?? '').includes('hover marker'),
      jsLinkText: !!jslink,
      jsLinkHref: jslink?.getAttribute('href') ?? null,
      img: img
        ? {
            complete: img.complete,
            naturalWidth: img.naturalWidth,
            src: img.getAttribute('src'),
          }
        : null,
      notesHref: notes?.getAttribute('href') ?? null,
      notesTarget:
        notes?.getAttribute('target') ??
        doc?.querySelector('base')?.getAttribute('target') ??
        null,
      scripts: doc?.querySelectorAll('script').length ?? -1,
      onAttrs,
      jsHrefs: Array.from(doc?.querySelectorAll('a[href]') ?? []).filter((a) =>
        /^\s*javascript:/i.test(a.getAttribute('href') ?? '')
      ).length,
      embeds: doc?.querySelectorAll('iframe, object, embed').length ?? -1,
      forms: doc?.querySelectorAll('form').length ?? -1,
      styleKept: (doc?.querySelectorAll('body style').length ?? 0) > 0,
      frameFlag:
        (iframe.contentWindow as unknown as { __r2md?: unknown } | null)?.__r2md ?? null,
    };
  });
}

/** The app page's window.__r2md and __r2probe flags (set only if a fixture script ran). */
async function parentFlags(page: Page): Promise<{ r2md: unknown; r2probe: unknown }> {
  return page.evaluate(() => {
    const w = window as unknown as { __r2md?: unknown; __r2probe?: unknown };
    return { r2md: w.__r2md ?? null, r2probe: w.__r2probe ?? null };
  });
}

/** Opens the markdown page as key and waits for the rendered preview (h1 inside the frame). */
async function openMarkdown(key: Key): Promise<Page> {
  const page = await openArtifact(key, artifactPage(markdownId()));
  await expect(headerTitle(page)).toHaveText(r2.markdown!.title);
  await markdownFrame(page).locator('h1').waitFor({ state: 'visible' });
  await expect
    .poll(async () => {
      const f = await markdownFacts(page);
      const img = f.img as { complete: boolean; naturalWidth: number } | null;
      return !!img && img.complete && img.naturalWidth > 0;
    })
    .toBe(true);
  return page;
}

/** Soft checks of a markdown render (R2-07 / R2-16). */
function expectMarkdownRendered(facts: Record<string, unknown>, seq: number): void {
  const img = facts.img as { complete: boolean; naturalWidth: number; src: string | null } | null;
  expect.soft(facts.h1, 'markdown heading').toBe('R2 markdown');
  expect.soft(facts.markerVisible, 'markdown text').toBe(true);
  expect.soft(img?.naturalWidth, 'relative image loaded').toBe(DOT_SIZE.width);
  expect
    .soft(img?.src, 'image from the version files')
    .toBe(`/api/v1/artifacts/${markdownId()}/versions/${seq}/files/img/dot.png?stream=1`);
  expect
    .soft(facts.notesHref, 'link pinned to the version file')
    .toBe(`/api/v1/artifacts/${markdownId()}/versions/${seq}/files/notes.md`);
  const t = tokens(facts.sandbox as string | null);
  expect.soft(t, 'markdown frame: no allow-scripts').not.toContain('allow-scripts');
  // The intended set (ruling D-1): same origin, and user-clicked links open a new tab.
  expect
    .soft(t, 'markdown sandbox tokens')
    .toEqual(['allow-popups', 'allow-popups-to-escape-sandbox', 'allow-same-origin']);
  expect
    .soft(t.includes('allow-scripts') && t.includes('allow-same-origin'), 'never both')
    .toBe(false);
  expect.soft(facts.referrerpolicy, 'markdown referrerpolicy').toBe('no-referrer');
  expect.soft(facts.hasSrcAttr, 'markdown frame has no src').toBe(false);
  expect.soft(facts.srcdocSet, 'markdown frame uses srcdoc').toBe(true);
  const csp = String(facts.metaCsp ?? '');
  expect.soft(csp, "meta CSP default-src 'none'").toContain("default-src 'none'");
  expect.soft(csp, "meta CSP img-src 'self' data:").toContain("img-src 'self' data:");
  expect.soft(csp, 'meta CSP has no script-src').not.toContain('script-src');
}

/** Soft checks that markdown active content is inert (R2-08 / R2-16). */
function expectMarkdownInert(facts: Record<string, unknown>, flags: { r2md: unknown }): void {
  expect.soft(facts.scripts, 'no script elements').toBe(0);
  expect.soft(facts.onAttrs, 'no on* attributes').toBe(0);
  expect.soft(facts.jsHrefs, 'no javascript: hrefs').toBe(0);
  expect.soft(facts.embeds, 'no iframe, object or embed').toBe(0);
  expect.soft(facts.forms, 'no form').toBe(0);
  expect.soft(facts.hoverMarker, 'hover marker text kept').toBe(true);
  expect.soft(facts.jsLinkText, 'js link text kept').toBe(true);
  expect.soft(facts.jsLinkHref, 'js link has no href').toBeNull();
  expect.soft(facts.frameFlag, 'no fixture script ran (frame window)').toBeNull();
  expect.soft(flags.r2md, 'no fixture script ran (app page)').toBeNull();
}

/** Named checks of one isolation row (restricted: evidence only). */
type IsolationChecks = Record<string, boolean>;

/**
 * Runs an isolation row. The body records named checks and asserts
 * nothing; anything it throws only marks the row incomplete. Then one
 * generic assertion, from this single call site, decides the row: no
 * Playwright output tells which check failed, or how many. The checks and
 * their inputs go in the restricted evidence only.
 */
async function isolationRow(
  r: RowRecord,
  body: (checks: IsolationChecks) => Promise<void>
): Promise<void> {
  const checks: IsolationChecks = {};
  let completed = true;
  try {
    await body(checks);
  } catch {
    completed = false;
  }
  checks.completed = completed;
  r.ui.checks = checks;
  const held = Object.keys(checks).length > 1 && Object.values(checks).every(Boolean);
  expect.soft(held, ISOLATION_FAIL).toBe(true);
}

/** Polls a locator's text until it equals want; false on timeout (never throws). */
async function textIs(
  locator: ReturnType<Page['locator']>,
  want: string,
  timeoutMs = 15_000
): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const got = await locator
      .first()
      .textContent({ timeout: 1_000 })
      .catch(() => null);
    if ((got ?? '').replace(/\s+/g, ' ').trim() === want) return true;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  return false;
}

/** Preconditions: facts, not row results (a soft failure never hides later rows). */
function preBundle(): string | undefined {
  return r2.bundle ? undefined : 'no bundle was published (R2-01)';
}
function preMarkdown(): string | undefined {
  if (r2.markdown) return undefined;
  return r2.markdownRefused ?? 'the markdown publish did not complete (R2-07)';
}
function preV3(): string | undefined {
  return r2.v3Published ? undefined : 'v3 was not published (R2-09)';
}
function preHeld(): string | undefined {
  return r2.heldViewUrl ? undefined : 'no view URL was minted for the owner (R2-02)';
}

const TITLE_MAIN = 'R2: artifact page view and render, graded at the UI';
const TITLE_CLEANUP = 'R2-18/R2-19: cleanup by absence, then the evidence';
const skipTitle = (id: string): string => `${id}: ${ROWS[id]!.title}`;
const SKIPPED: Record<string, string> = { 'R2-17b': R2_17B_REASON, 'R2-21': R2_21_REASON };

const ORACLE_ROW_MAP: Record<string, { test: string; note: string }> = Object.fromEntries(
  Object.entries(ROWS).map(([id, spec]) => {
    if (spec.where === 'skip') return [id, { test: skipTitle(id), note: SKIPPED[id] ?? '' }];
    if (spec.where === 'cleanup') {
      return [id, { test: TITLE_CLEANUP, note: `the afterAll hook: evidence.rows["${id}"]` }];
    }
    return [id, { test: TITLE_MAIN, note: `evidence.rows["${id}"] (a test step)` }];
  })
);

/**
 * Why this run may not carry the isolation rows, or undefined when it may:
 * the render config, reporters list, github or json only, no trace, video
 * or screenshot, no debugger, and the failure page snapshot off. Checked
 * from the resolved run settings, so CLI overrides (--reporter, --trace,
 * --ui, --debug) cannot bypass it.
 */
function runSettingsRefusal(): string | undefined {
  const info = test.info();
  const configFile = path.basename(info.config.configFile ?? '');
  if (configFile !== 'playwright.render.config.ts') return WRONG_CONFIG;
  // JSON is allowed: with trace, video and screenshot off it has no
  // attachments, and the isolation rows' step errors are generic.
  const allowed = new Set(['list', 'github', 'json']);
  if (!info.config.reporter.every(([name]) => allowed.has(name))) return WRONG_CONFIG;
  const use = info.project.use as { trace?: unknown; video?: unknown; screenshot?: unknown };
  const off = (v: unknown): boolean => {
    if (v === undefined || v === 'off') return true;
    return typeof v === 'object' && v !== null && (v as { mode?: unknown }).mode === 'off';
  };
  if (!off(use.trace) || !off(use.video) || !off(use.screenshot)) return WRONG_CONFIG;
  if (process.env.PWDEBUG) return WRONG_CONFIG;
  // Playwright's failure page snapshot (error-context.md) can hold frame content.
  if (!process.env.PLAYWRIGHT_NO_COPY_PROMPT) return WRONG_CONFIG;
  return undefined;
}

/** Set by playwright.render.config.ts, the only config without an HTML reporter. */
const UNDER_RENDER_CONFIG = process.env.E2E_R2_RENDER_CONFIG === '1';
const WRONG_CONFIG =
  'run with e2e/artifacts-access/playwright.render.config.ts, reporters list, github or ' +
  'json only, and no --trace, --ui or --debug (isolation rows)';

// ---- NOT RUN by design: declared outside the serial group so each body runs.
test.describe('render rows not run at this build', () => {
  test.skip(!UNDER_RENDER_CONFIG, WRONG_CONFIG);
  for (const id of Object.keys(SKIPPED)) {
    test(skipTitle(id), () => {
      const reason = SKIPPED[id]!;
      j.notRun([id], reason);
      test.info().annotations.push({
        type: 'NOT RUN',
        description: `${id} (${ROWS[id]!.role}): ${ROWS[id]!.title}`,
      });
      const results = (j.state.evidence.results ??= {}) as Record<string, unknown>;
      results[skipTitle(id)] = {
        status: 'not run',
        skipReason: reason,
        annotations: annotationsOf(test.info().annotations),
      };
      test.skip(true, reason);
    });
  }
});

test.describe.serial('artifact page view and render (R2)', () => {
  test.skip(!UNDER_RENDER_CONFIG, WRONG_CONFIG);
  // eslint-disable-next-line no-empty-pattern -- Playwright needs the fixtures argument
  test.afterEach(({}, testInfo) => {
    if (testInfo.title === TITLE_MAIN && testInfo.error?.message) {
      j.state.testError ??= firstLine(testInfo.error.message);
    }
    j.recordResult(testInfo);
  });

  test.beforeAll(async ({ browser }) => {
    j.seedRows();
    // Refuse before any identity or page exists.
    const refusal = runSettingsRefusal();
    if (refusal) {
      j.state.setupError = `beforeAll: ${refusal}`;
      throw new Error(refusal);
    }
    try {
      const t = j.resolveTarget();
      j.state.target = t;
      j.state.evidence.oracleRowMap = ORACLE_ROW_MAP;
      j.state.evidence.target = {
        mode: t.mode,
        baseURL: t.baseURL,
        hubId: t.hub?.hubId ?? null,
        experimentToggle: !!j.adminToken(),
      };
      j.state.evidence.fixture = {
        broker: 'none (R2 needs no broker, runtime, credentials or external network)',
        publishSurface: 'web New artifact and Upload new version dialogs (Folder mode)',
        project: 'home (owner, member); outsider in none',
      };
      j.state.tmpDir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-r2-')));
      fs.chmodSync(j.state.tmpDir, 0o700);
      for (const n of [1, 2, 3]) {
        r2.dirs[`bundle-v${n}`] = writeFolder(j.state.tmpDir, `r2-bundle-v${n}`, bundleFiles(n));
      }
      r2.dirs.markdown = writeFolder(j.state.tmpDir, 'r2-md', markdownFiles());
      r2.fileSizes = {
        'img/chart.png': bundleFiles(3)['img/chart.png']!.length,
      };

      await j.setUpIdentities(async () => {
        const context = await browser.newContext({ baseURL: t.baseURL });
        return context;
      });
      for (const key of KEYS) {
        const identity = j.who(key);
        watchContext(identity.context, key);
      }
      const fixtures: Record<string, unknown> = {};
      await j.createProject('owner', `e2e-r2-home-${suffix()}`);
      await j.addMember('owner', 'member', fixtures);
      j.state.evidence.projects = { home: { id: j.state.projectId } };
      j.state.evidence.identities = Object.fromEntries(
        KEYS.map((key) => [
          key,
          {
            id: j.who(key).id,
            role: {
              owner: 'publishes both artifacts and all versions; owner of home',
              member: 'reader: home project-member, no grant',
              outsider: 'no project, no grant',
            }[key],
          },
        ])
      );
      j.state.evidence.membershipFixtures = fixtures;
    } catch (err) {
      j.state.setupError = `beforeAll: ${firstLine(err)}`;
      throw err;
    }
  });

  test(TITLE_MAIN, async ({ browser }) => {
    test.setTimeout(15 * 60_000);
    await runMain(browser);
  });

  // eslint-disable-next-line no-empty-pattern -- Playwright needs the fixtures argument
  test.afterAll(TITLE_CLEANUP, async ({}, testInfo) => {
    await runCleanup(testInfo);
  });
});

async function runMain(browser: Browser): Promise<void> {
  // ---- experiment: on for the run (enabled here only if it was off) ------
  const experiment: Record<string, unknown> = {};
  j.state.evidence.experiment = experiment;
  if (j.adminToken()) {
    const before = await j.readExperiment();
    experiment.before = before;
    if (before.enabled !== true) {
      experiment.afterEnable = await j.setExperiment(true);
      r2.experimentSetByRun = true;
    }
  }
  const probe = await j.api('owner', 'GET', '/api/v1/artifacts?mine=1');
  experiment.listStatus = probe.status;
  experiment.setByRun = r2.experimentSetByRun;
  expect(probe.status, `${EXPERIMENT} must be on for R2`).toBe(200);

  // ---- R2-01: the owner publishes the HTML bundle v1 (smoke) -------------
  r2.step = 'R2-01';
  const smoke: Record<string, unknown> = {};
  j.state.evidence.smoke = smoke;
  const smokePassed = await j.row('R2-01', async (r) => {
    const page = await j.pageOf('owner');
    const since = j.who('owner').requests.length;
    await page.goto(`/projects/${j.state.projectId}`, { waitUntil: 'domcontentloaded' });
    await revealFiles(page);
    await expect(page.locator(SELECTORS.filesSegmentArtifacts)).toBeVisible();
    await page.locator(SELECTORS.filesSegmentArtifacts).click();
    await expect(page.locator(SELECTORS.artifactList)).toBeAttached();
    const newButton = newArtifactButton(page);
    await expect(newButton, 'New artifact visible').toBeVisible();
    await newButton.click();
    const title = `R2 bundle ${suffix()}`;
    const pub = await publishThroughDialog(page, r2.dirs['bundle-v1']!, {
      title,
      note: 'r2 v1',
      createPath: /^\/api\/v1\/artifacts$/,
    });
    r.ui.entryShown = pub.entry;
    smoke.createStatus = pub.createStatus;
    smoke.code = pub.createCode;
    if (pub.createStatus === 401 || pub.createStatus === 403) {
      smoke.stopped = true;
      const text =
        `member session cannot publish (${pub.createStatus} ${String(pub.createCode)}); ` +
        'no scope or role workaround';
      j.finding('R2-01', text);
      j.state.stopped = `not run: smoke precondition failed (${text})`;
      throw new Error(`STOP: ${text}`);
    }
    expect(pub.createStatus, 'publish create').toBe(201);
    smoke.stopped = false;
    // Known from the create on, so the cleanup deletes it even if the publish stops.
    if (pub.createdId) r2.bundle = { id: pub.createdId, title };
    expect.soft(pub.entry, 'entry defaults to index.html').toBe('index.html');
    const landed = await expect(page)
      .toHaveURL(new RegExp(`/projects/${j.state.projectId}/artifacts/[0-9a-f-]+$`))
      .then(() => true)
      .catch(() => false);
    await settle(page);
    const calls = publishCalls('owner', since, 1);
    r.api.push(...calls.map((c) => ({ as: 'owner', ...c, status: c.status ?? 0, code: null })));
    r2.publishes.push({ artifact: 'bundle', seq: 1, calls, asked: pub.asked });
    const finalize = calls.find((c) => c.path.endsWith('/finalize'));
    smoke.finalizeStatus = finalize?.status ?? null;
    const finalizeOk = !!finalize?.status && finalize.status >= 200 && finalize.status < 300;
    if (!finalizeOk) {
      // A non-2xx or missing finalize is a STOP with a FINDING (no workaround).
      const text = `publish finalize ${String(finalize?.status ?? 'missing')}; no workaround`;
      smoke.stopped = true;
      j.finding('R2-01', text);
      j.state.stopped = `not run: smoke precondition failed (${text})`;
      throw new Error(`STOP: ${text}`);
    }
    expect(landed, 'lands on the artifact').toBe(true);
    const id = new URL(page.url()).pathname.split('/').pop()!;
    expect.soft(id, 'landed on the created artifact').toBe(pub.createdId);
    // The created id stays the bundle; a different landed id is cleaned up too.
    if (pub.createdId && id !== pub.createdId) r2.extraArtifactIds.push(id);
    r2.bundle = { id: pub.createdId ?? id, title };
    await expect(headerTitle(page)).toHaveText(title);
    await expect(page.locator(SELECTORS.versionMenu)).toContainText('v1 (current)');
    r.ui.landed = redactPath(new URL(page.url()).pathname);
    r.ui.versionLabel = await textOf(page.locator(SELECTORS.versionMenu));
    for (const c of calls.filter((x) => x.method === 'PUT')) {
      expect.soft(c.status ?? 0, `upload ${c.path}`).toBeGreaterThanOrEqual(200);
      expect.soft(c.status ?? 0, `upload ${c.path}`).toBeLessThan(300);
    }
  });
  if (!smokePassed) {
    const first = j.state.rows['R2-01']?.errors[0] ?? 'no error recorded';
    j.state.stopped ??= `not run: smoke precondition failed (R2-01: ${first})`;
  }

  // ---- R2-02: HTML frame attributes, bar, view response policy -----------
  r2.step = 'R2-02';
  await j.row(
    'R2-02',
    async (r) => {
      const page = await j.pageOf('owner');
      await page.locator(RENDER_SELECTORS.htmlFrame).waitFor({ state: 'attached' });
      await bundleFrame(page);
      const attrs = await htmlFrameAttributes(page);
      const info = viewInfo(attrs.src, bundleId());
      const frameRec = {
        who: 'owner',
        step: 'R2-02',
        kind: 'html',
        sandboxTokens: tokens(attrs.sandbox),
        referrerpolicy: attrs.referrerpolicy,
        title: attrs.title,
        srcKind: attrs.src?.startsWith(VIEW_PREFIX) ? 'view' : 'other',
        ...info,
      };
      r2.frames.push(frameRec);
      r.ui.frame = frameRec;
      expect.soft(frameRec.sandboxTokens, 'sandbox is exactly allow-scripts').toEqual([
        'allow-scripts',
      ]);
      for (const f of SANDBOX_FORBIDDEN_HTML) {
        expect.soft(frameRec.sandboxTokens, `sandbox has no ${f}`).not.toContain(f);
      }
      expect.soft(attrs.referrerpolicy, 'referrerpolicy').toBe('no-referrer');
      expect.soft(attrs.title, 'frame title').toBe(`${r2.bundle!.title} (sandboxed)`);
      expect.soft(frameRec.srcKind, 'src is a view URL').toBe('view');
      expect.soft(info.viewSeq, 'view names v1').toBe(1);
      expect.soft(info.viewIdMatches, 'view names this artifact').toBe(true);
      expect.soft(info.entry, 'view opens the entry').toBe('index.html');
      r.ui.untrustedBar = await textOf(page.locator(RENDER_SELECTORS.untrustedBar));
      expect
        .soft(r.ui.untrustedBar, 'untrusted bar')
        .toBe('Content published by You · runs sandboxed, isolated from Scion');
      r.ui.htmlBar = await textOf(page.locator(RENDER_SELECTORS.htmlBar));
      expect.soft(String(r.ui.htmlBar), 'entry bar').toContain('index.html · bundle of 4 files');
      r.ui.fullScreen = await isShown(previewBarButton(page, 'Full screen'));
      r.ui.openInNewTab = await isShown(previewBarButton(page, 'Open in new tab'));
      r.ui.remoteImagesNotice = await isShown(remoteImagesNotice(page));
      expect.soft(r.ui.fullScreen, 'Full screen visible').toBe(true);
      expect.soft(r.ui.openInNewTab, 'Open in new tab visible').toBe(true);
      expect.soft(r.ui.remoteImagesNotice, 'remote-images notice visible').toBe(true);

      const mint = await latestMint('owner', 1);
      r2.heldViewUrl = mint.url ?? undefined;
      r.api.push({
        as: 'owner',
        method: 'POST',
        path: `/api/v1/artifacts/${bundleId()}/versions/1/view`,
        status: mint.status,
        code: null,
      });
      r.ui.mint = { status: mint.status, remoteImages: mint.remoteImages };
      expect.soft(mint.status, 'mint view').toBe(200);
      expect.soft(mint.remoteImages, 'mint remoteImages').toBe(true);
      const headers = await viewHeaders('owner', mint.url!, r);
      r.ui.viewResponse = headers;
      const csp = String(headers.csp);
      expect.soft(headers.status, 'view document').toBe(200);
      for (const d of [
        'sandbox allow-scripts',
        "connect-src 'none'",
        "form-action 'none'",
        "frame-ancestors 'self'",
      ]) {
        expect.soft(csp, `view CSP has ${d}`).toContain(d);
      }
      expect.soft(headers.xContentTypeOptions, 'nosniff').toBe('nosniff');
      expect.soft(headers.referrerPolicy, 'Referrer-Policy').toBe('no-referrer');
      expect.soft(headers.contentDisposition, 'Content-Disposition').toBe('inline');
    },
    { pre: preBundle }
  );

  // ---- R2-03: renders with its own files ---------------------------------
  r2.step = 'R2-03';
  await j.row(
    'R2-03',
    async (r) => {
      const page = await openArtifact('owner', artifactPage(bundleId()));
      await page.locator(RENDER_SELECTORS.htmlFrame).waitFor({ state: 'attached' });
      const frame = await bundleFrame(page);
      await expect(htmlFrame(page).locator('#ver')).toHaveText('R2 bundle v1');
      await expect
        .poll(() =>
          frame.evaluate(() => {
            const img = document.getElementById('chart') as HTMLImageElement | null;
            return !!img && img.complete && img.naturalWidth > 0;
          })
        )
        .toBe(true);
      const facts = await frame.evaluate(() => {
        const probe = document.getElementById('css-probe');
        const cs = probe ? getComputedStyle(probe) : null;
        const chart = document.getElementById('chart') as HTMLImageElement | null;
        const remote = document.getElementById('remote') as HTMLImageElement | null;
        return {
          heading: (document.getElementById('ver')?.textContent ?? '').trim(),
          cssWidth: cs?.width ?? null,
          cssColor: cs?.color ?? null,
          chartNaturalWidth: chart?.naturalWidth ?? -1,
          remoteNaturalWidth: remote?.naturalWidth ?? -1,
        };
      });
      r.ui.render = facts;
      expect.soft(facts.heading, 'v1 heading').toBe('R2 bundle v1');
      expect.soft(facts.cssWidth, 'CSS applied (width)').toBe(CSS_PROBE.width);
      expect.soft(facts.cssColor, 'CSS applied (colour)').toBe(CSS_PROBE.color);
      expect.soft(facts.chartNaturalWidth, 'relative image loaded').toBe(CHART_SIZE.width);
      expect.soft(facts.remoteNaturalWidth, 'remote image not loaded').toBe(0);
      await settle(page);
      const own = requestsIn('owner', ['R2-03']);
      const css = own.find((q) => q.path.endsWith('/css/site.css'));
      const png = own.find((q) => q.path.endsWith('/img/chart.png'));
      const underView = (q: FrameRequest | undefined): boolean =>
        !!q && q.path.startsWith(`${VIEW_PREFIX}<redacted>/`);
      r.ui.subresources = {
        css: { status: css?.status ?? null, underView: underView(css) },
        png: { status: png?.status ?? null, underView: underView(png) },
      };
      expect.soft(css?.status, 'css 200').toBe(200);
      expect.soft(png?.status, 'png 200').toBe(200);
      expect.soft(underView(css), 'css under the view path').toBe(true);
      expect.soft(underView(png), 'png under the view path').toBe(true);
      // Any request to the remote host, from any page of any identity (host only).
      r.ui.remoteHostHits = r2.remoteHostHits;
      expect.soft(r2.remoteHostHits, 'no request to the remote image host').toBe(0);
    },
    { pre: preBundle }
  );

  // ---- R2-04: relative link stays on the version -------------------------
  r2.step = 'R2-04';
  await j.row(
    'R2-04',
    async (r) => {
      const page = await openArtifact('owner', artifactPage(bundleId()));
      await page.locator(RENDER_SELECTORS.htmlFrame).waitFor({ state: 'attached' });
      const appUrl = page.url();
      const frame = await bundleFrame(page);
      await expect(htmlFrame(page).locator('#ver')).toHaveText('R2 bundle v1');
      const capBefore = capabilityOf(frame.url());
      await htmlFrame(page).locator('#to-page2').click();
      await expect(htmlFrame(page).locator('#page2')).toHaveText('R2 page two v1');
      const frame2 = await bundleFrame(page);
      await expect
        .poll(() =>
          frame2.evaluate(() => {
            const img = document.getElementById('chart2') as HTMLImageElement | null;
            return img ? img.naturalWidth : -1;
          })
        )
        .toBe(CHART_SIZE.width);
      const info = viewInfo(frame2.url(), bundleId());
      r.ui.page2 = {
        text: await textOf(htmlFrame(page).locator('#page2')),
        sameCapability: capabilityOf(frame2.url()) === capBefore,
        entry: info.entry,
        viewSeq: info.viewSeq,
      };
      expect
        .soft((r.ui.page2 as { sameCapability: boolean }).sameCapability, 'same capability')
        .toBe(true);
      expect.soft(info.entry, 'page2 under the view').toBe('page2.html');
      await htmlFrame(page).locator('#back').click();
      await expect(htmlFrame(page).locator('#ver')).toHaveText('R2 bundle v1');
      r.ui.backHeading = await frameHeading(page);
      r.ui.appUrlUnchanged = page.url() === appUrl;
      expect.soft(r.ui.appUrlUnchanged, 'app page URL unchanged').toBe(true);
      await settle(page);
      const p2 = r2.frameRequests.find(
        (q) => q.who === 'owner' && q.step === 'R2-04' && q.path.endsWith('/page2.html')
      );
      r.api.push({
        as: 'owner (frame)',
        method: 'GET',
        path: p2?.path ?? `${VIEW_PREFIX}<redacted>/page2.html`,
        status: p2?.status ?? 0,
        code: null,
      });
      expect.soft(p2?.status, 'page2 200').toBe(200);
    },
    { pre: preBundle }
  );

  // ---- R2-05: isolation negatives (one generic verdict) ----------------
  r2.step = 'R2-05';
  await j.row(
    'R2-05',
    (r) =>
      isolationRow(r, async (checks) => {
        const page = await openArtifact('owner', artifactPage(bundleId()));
        const appUrl = page.url();
        const pagesBefore = j.who('owner').context.pages().length;
        await page.locator(RENDER_SELECTORS.htmlFrame).waitFor({ state: 'attached' });
        const frame = await bundleFrame(page);
        const result = await readProbe(frame);
        const parent = {
          contentDocumentNull: await page
            .locator(RENDER_SELECTORS.htmlFrame)
            .evaluate((f) => (f as HTMLIFrameElement).contentDocument === null),
          parentFlag: (await parentFlags(page)).r2probe,
        };
        // (c) a user-activated top navigation attempt.
        await htmlFrame(page).locator('#top-nav').click();
        await page.waitForTimeout(3000);
        const probeRequests = j
          .who('owner')
          .requests.filter((q) => Object.values(PROBE_PATHS).some((p) => q.path === p));
        const nav = {
          pageUrlUnchanged: page.url() === appUrl,
          probePathRequests: probeRequests.length,
          newPages: j.who('owner').context.pages().length - pagesBefore,
        };
        // (b) the frame navigating itself to a hub API URL.
        const apiNav = page.waitForResponse(
          (res) =>
            new URL(res.url()).pathname === API_PROBE_PATH &&
            res.request().isNavigationRequest() &&
            res.request().frame() !== page.mainFrame(),
          { timeout: 15_000 }
        );
        await htmlFrame(page).locator('#api-nav').click();
        const apiNavStatus = (await apiNav).status();
        await settle(page);
        // Every frame request of the owner so far (R2-01..R2-05), by step.
        const session = await frameSessionFacts(
          requestsIn('owner', ['R2-01', 'R2-02', 'R2-03', 'R2-04', 'R2-05'])
        );
        if (session.bearing > 0 || apiNavStatus === 200) r2.frameSessionAlert = true;
        if (apiNavStatus !== 200 && apiNavStatus !== 401) {
          // An ordinary finding, kept in the restricted evidence only (no
          // annotation, so no output tells which check it was); the status
          // is in the probe record.
          j.state.findings.push({ id: 'R2-05', text: ISOLATION_FAIL });
        }
        const probeRec = {
          who: 'owner',
          step: 'R2-05',
          ...result,
          parent,
          nav,
          apiNavStatus,
          session,
        };
        r2.probes.push(probeRec);
        r.ui.probe = probeRec;
        r.api.push({
          as: 'owner (frame)',
          method: 'GET',
          path: API_PROBE_PATH,
          status: apiNavStatus,
          code: null,
        });
        // (a)
        checks.framed = result.frame === 'framed';
        checks.opaqueOrigin = result.origin === 'null';
        checks.parentDomBlocked = String(result.parentDom).startsWith('blocked:');
        checks.parentWriteBlocked = String(result.parentWrite).startsWith('blocked:');
        checks.appCannotReadFrame = parent.contentDocumentNull;
        checks.appFlagUnset = parent.parentFlag === null;
        // (b)
        checks.cookieBlocked = String(result.cookie).startsWith('blocked:');
        checks.storageBlocked = String(result.storage).startsWith('blocked:');
        checks.fetchBlocked = String(result.fetch).startsWith('blocked:');
        checks.frameRequestsSeen = session.total > 0;
        checks.headersAllRead = session.unread === 0;
        checks.noSessionBearing = session.bearing === 0;
        checks.apiNavUnauthenticated = apiNavStatus === 401;
        // (c)
        checks.popupBlocked = String(result.popup).startsWith('blocked:');
        checks.pageUrlUnchanged = nav.pageUrlUnchanged;
        checks.noProbePathRequests = nav.probePathRequests === 0;
        checks.noNewPages = nav.newPages === 0;
      }),
    { pre: preBundle }
  );

  // ---- R2-06: Open in new tab stays sandboxed (one generic verdict) -----
  r2.step = 'R2-06';
  await j.row(
    'R2-06',
    (r) =>
      isolationRow(r, async (checks) => {
        const page = await openArtifact('owner', artifactPage(bundleId()));
        await page.locator(RENDER_SELECTORS.htmlFrame).waitFor({ state: 'attached' });
        const frameSrc = (await htmlFrameAttributes(page)).src ?? '';
        r2.step = 'R2-06-popup';
        let popup: Page | undefined;
        let phase: {
          samePath: boolean;
          rendered: boolean;
          result: Record<string, unknown>;
          doc: FrameRequest | undefined;
          sub: FrameRequest[];
          session: Awaited<ReturnType<typeof frameSessionFacts>>;
          headers: Record<string, string | number>;
        };
        try {
          [popup] = await Promise.all([
            j.who('owner').context.waitForEvent('page'),
            previewBarButton(page, 'Open in new tab').click(),
          ]);
          await popup.waitForLoadState('load');
          const samePath =
            new URL(popup.url()).pathname === new URL(frameSrc, popup.url()).pathname;
          const rendered = await textIs(popup.locator('#ver'), 'R2 bundle v1');
          const result = await readProbe(popup);
          await settle(popup);
          const popupPath = redactPath(new URL(popup.url()).pathname);
          const doc = requestsIn('owner', ['R2-06-popup']).find(
            (q) => q.topLevelDocument && q.path === popupPath
          );
          // The popup's own sub-resources (and the app page's frame, if it reloads).
          const sub = requestsIn('owner', ['R2-06-popup']).filter((q) => !q.topLevelDocument);
          const session = await frameSessionFacts(sub);
          if (session.bearing > 0) r2.frameSessionAlert = true;
          const headers = await viewHeaders('owner', frameSrc, r);
          phase = { samePath, rendered, result, doc, sub, session, headers };
        } finally {
          await popup?.close();
          r2.step = 'R2-06';
        }
        const { samePath, rendered, result, doc, sub, session, headers } = phase;
        const rec = {
          who: 'owner',
          step: 'R2-06',
          ...result,
          samePath,
          rendered,
          topLevelDocument: {
            status: doc?.status ?? null,
            cookieHeaderRead: doc?.cookieHeader.read ?? null,
            cookieHeaderPresent: doc?.cookieHeader.present ?? null,
          },
          subresources: sub.map((q) => ({ path: q.path, status: q.status ?? null })),
          session,
          csp: headers.csp,
        };
        r2.probes.push(rec);
        r.ui.popup = rec;
        checks.samePath = samePath;
        checks.rendered = rendered;
        checks.topLevel = result.frame === 'top-level';
        checks.opaqueOrigin = result.origin === 'null';
        checks.cookieBlocked = String(result.cookie).startsWith('blocked:');
        checks.storageBlocked = String(result.storage).startsWith('blocked:');
        checks.fetchBlocked = String(result.fetch).startsWith('blocked:');
        checks.subresourcesSeen = session.total > 0;
        checks.headersAllRead = session.unread === 0;
        checks.noSessionBearing = session.bearing === 0;
        checks.documentServed = doc?.status === 200;
        checks.cspSandbox = String(headers.csp).includes('sandbox allow-scripts');
      }),
    { pre: preBundle }
  );

  // ---- R2-07: markdown bundle renders ------------------------------------
  r2.step = 'R2-07';
  await j.row(
    'R2-07',
    async (r) => {
      const page = await j.pageOf('owner');
      const since = j.who('owner').requests.length;
      await page.goto(`/projects/${j.state.projectId}`, { waitUntil: 'domcontentloaded' });
      await revealFiles(page);
      await page.locator(SELECTORS.filesSegmentArtifacts).click();
      await expect(page.locator(SELECTORS.artifactList)).toBeAttached();
      await newArtifactButton(page).click();
      const title = `R2 markdown ${suffix()}`;
      const pub = await publishThroughDialog(page, r2.dirs.markdown!, {
        title,
        note: 'r2 md',
        createPath: /^\/api\/v1\/artifacts$/,
      });
      r.ui.entryShown = pub.entry;
      // A refused publish makes the markdown rows "not run" with the cause (oracle rule 3).
      const refuse = (cause: string): void => {
        r2.markdownRefused = `markdown publish refused (${cause})`;
        r.legNotRun = r2.markdownRefused;
      };
      if (pub.createStatus !== 201) {
        refuse(`create ${pub.createStatus} ${String(pub.createCode)}`);
        return;
      }
      // Known from the create on, so the cleanup deletes it even if finalize is refused.
      r2.markdownCreatedId = pub.createdId ?? undefined;
      const landed = await expect(page)
        .toHaveURL(new RegExp(`/projects/${j.state.projectId}/artifacts/[0-9a-f-]+$`))
        .then(() => true)
        .catch(() => false);
      await settle(page);
      const calls = publishCalls('owner', since, 1);
      r.api.push(...calls.map((c) => ({ as: 'owner', ...c, status: c.status ?? 0, code: null })));
      r2.publishes.push({ artifact: 'markdown', seq: 1, calls, asked: pub.asked });
      const finalize = calls.find((c) => c.path.endsWith('/finalize'));
      const finalizeOk = !!finalize?.status && finalize.status >= 200 && finalize.status < 300;
      if (!finalizeOk) {
        refuse(`finalize ${String(finalize?.status ?? 'missing')}`);
        return;
      }
      // Published (create 201, finalize 2xx): the markdown rows may run.
      if (pub.createdId) r2.markdown = { id: pub.createdId, title };
      expect(landed, 'lands on the markdown artifact').toBe(true);
      const landedId = new URL(page.url()).pathname.split('/').pop()!;
      expect.soft(landedId, 'landed on the created markdown').toBe(pub.createdId);
      r2.markdown = { id: pub.createdId ?? landedId, title };
      if (pub.createdId && landedId !== pub.createdId) r2.extraArtifactIds.push(landedId);
      expect.soft(pub.entry, 'entry defaults to index.md').toBe('index.md');

      const md = await openMarkdown('owner');
      r.ui.entryBar = await textOf(md.locator(RENDER_SELECTORS.htmlBar));
      expect
        .soft(String(r.ui.entryBar), 'entry bar: index.md · <size>')
        .toMatch(/^index\.md · \S+/);
      r.ui.openRaw = await isShown(previewBarButton(md, 'Open raw'));
      expect.soft(r.ui.openRaw, 'Open raw visible').toBe(true);
      const facts = await markdownFacts(md);
      r2.frames.push({ who: 'owner', step: 'R2-07', kind: 'markdown', ...facts });
      r.ui.facts = facts;
      expectMarkdownRendered(facts, 1);
      for (const p of ['index.md', 'img/dot.png']) {
        const res = await j.api(
          'owner',
          'GET',
          `/api/v1/artifacts/${markdownId()}/versions/1/files/${p}?stream=1`,
          undefined,
          r
        );
        expect.soft(res.status, `GET ${p}`).toBe(200);
      }
    }
  );

  // ---- R2-07b: a clicked link opens a new tab; nothing opens by itself ---
  r2.step = 'R2-07b';
  await j.row(
    'R2-07b',
    async (r) => {
      const page = await openMarkdown('owner');
      const context = j.who('owner').context;
      const pagesBefore = context.pages().length;
      await page.waitForTimeout(1000);
      r.ui.pagesBeforeClick = context.pages().length - pagesBefore;
      expect.soft(r.ui.pagesBeforeClick, 'nothing opened before the click').toBe(0);
      const link = markdownFrame(page).locator('a', { hasText: /^\s*notes\s*$/ });
      const want = `/api/v1/artifacts/${markdownId()}/versions/1/files/notes.md`;
      // Registered before the click, so the new tab's document answer is never missed.
      const docResponse = context.waitForEvent('response', {
        predicate: (res) => new URL(res.url()).pathname === want,
        timeout: 15_000,
      });
      const [popup] = await Promise.all([context.waitForEvent('page'), link.click()]);
      const response = await docResponse.catch(() => null);
      await popup.waitForLoadState('load');
      const popupHeaders = response ? await response.allHeaders() : {};
      r.ui.popup = {
        path: new URL(popup.url()).pathname,
        status: response?.status() ?? null,
        contentType: popupHeaders['content-type'] ?? null,
        contentDisposition: popupHeaders['content-disposition'] ?? null,
      };
      const popupRec = r.ui.popup as {
        path: string;
        status: number | null;
        contentType: string | null;
        contentDisposition: string | null;
      };
      expect.soft(popupRec.path, 'new tab opens the v1 notes.md').toBe(want);
      expect.soft(popupRec.status, 'new tab answered 200').toBe(200);
      expect
        .soft(popupRec.contentType ?? '', 'served as text/markdown')
        .toMatch(/^text\/markdown/);
      expect.soft(popupRec.contentDisposition ?? '', 'served inline').toMatch(/^inline/);
      await popup.close();
      // The same file through the API (recorded; must agree).
      const api = await j.api('owner', 'GET', want, undefined, r);
      r.ui.apiFile = {
        status: api.status,
        contentType: api.headers['content-type'] ?? null,
        contentDisposition: api.headers['content-disposition'] ?? null,
      };
      expect.soft(api.status, 'GET notes.md').toBe(200);
      expect
        .soft(api.headers['content-type'] ?? '', 'API text/markdown')
        .toMatch(/^text\/markdown/);
      expect.soft(api.headers['content-disposition'] ?? '', 'API inline').toMatch(/^inline/);
      // No script ran, and the R2-08 counts still hold after the click.
      const facts = await markdownFacts(page);
      const flags = await parentFlags(page);
      r.ui.afterClick = facts;
      expectMarkdownInert(facts, flags);
    },
    { pre: preMarkdown }
  );

  // ---- R2-08: raw HTML and script inert ----------------------------------
  r2.step = 'R2-08';
  await j.row(
    'R2-08',
    async (r) => {
      const page = await openMarkdown('owner');
      let consoleBlocked = 0;
      const onConsole = (m: { text(): string }): void => {
        if (/Blocked script execution/i.test(m.text())) consoleBlocked++;
      };
      page.on('console', onConsole);
      const afterLoad = await parentFlags(page);
      await markdownFrame(page).getByText('hover marker').hover();
      const jsLink = markdownFrame(page).getByText('js link');
      if ((await jsLink.count()) > 0) await jsLink.click();
      await page.waitForTimeout(1000);
      page.off('console', onConsole);
      const facts = await markdownFacts(page);
      const flags = await parentFlags(page);
      const inert = {
        who: 'owner',
        step: 'R2-08',
        scripts: facts.scripts,
        onAttrs: facts.onAttrs,
        jsHrefs: facts.jsHrefs,
        embeds: facts.embeds,
        forms: facts.forms,
        styleKept: facts.styleKept,
        parentFlagAfterLoad: afterLoad.r2md,
        parentFlagAfterHoverAndClick: flags.r2md,
        consoleBlocked,
      };
      r2.markdownInert.push(inert);
      r.ui.inert = inert;
      expect.soft(afterLoad.r2md, 'no script ran after load').toBeNull();
      expectMarkdownInert(facts, flags);
      expect.soft(page.url(), 'app page stays on the artifact').toContain(markdownId());
    },
    { pre: preMarkdown }
  );

  // ---- R2-09: v2 and v3 through Upload new version -----------------------
  r2.step = 'R2-09';
  await j.row(
    'R2-09',
    async (r) => {
      for (const n of [2, 3]) {
        const page = await openArtifact('owner', artifactPage(bundleId()));
        await expect(page.locator(SELECTORS.versionMenu)).toContainText(`v${n - 1} (current)`);
        const since = j.who('owner').requests.length;
        await tab(page, 'history').click();
        const upload = uploadVersionButton(page);
        await expect(upload, 'Upload new version visible').toBeVisible();
        await upload.click();
        const pub = await publishThroughDialog(page, r2.dirs[`bundle-v${n}`]!, {
          note: `r2 v${n}`,
          createPath: new RegExp(`^/api/v1/artifacts/${bundleId()}/versions$`),
        });
        expect.soft(pub.createStatus, `versions POST v${n}`).toBe(201);
        await expect(page.locator(SELECTORS.versionMenu)).toContainText(`v${n} (current)`, {
          timeout: 30_000,
        });
        await tab(page, 'preview').click();
        await expect(htmlFrame(page).locator('#ver')).toHaveText(`R2 bundle v${n}`);
        r.ui[`v${n}`] = {
          versionLabel: await textOf(page.locator(SELECTORS.versionMenu)),
          heading: await frameHeading(page),
        };
        // v3 exists (the fact R2-10..R2-15 need): created and shown as current.
        if (n === 3 && pub.createStatus === 201) r2.v3Published = true;
        await settle(page);
        const calls = publishCalls('owner', since, n);
        r.api.push(...calls.map((c) => ({ as: 'owner', ...c, status: c.status ?? 0, code: null })));
        const create = j
          .who('owner')
          .requests.slice(since)
          .find(
            (q) => q.method === 'POST' && q.path === `/api/v1/artifacts/${bundleId()}/versions`
          );
        const finalize = calls.find((c) => c.path.endsWith('/finalize'));
        r2.publishes.push({
          artifact: 'bundle',
          seq: n,
          create: create?.status ?? null,
          asked: pub.asked,
          uploads: calls.filter((c) => c.method === 'PUT'),
          finalize: finalize?.status ?? null,
        });
        expect.soft(finalize?.status ?? 0, `finalize v${n}`).toBeGreaterThanOrEqual(200);
        expect.soft(finalize?.status ?? 0, `finalize v${n}`).toBeLessThan(300);
        for (const c of calls.filter((x) => x.method === 'PUT')) {
          expect.soft(c.status ?? 0, `upload ${c.path}`).toBeGreaterThanOrEqual(200);
          expect.soft(c.status ?? 0, `upload ${c.path}`).toBeLessThan(300);
        }
      }
    },
    { pre: preBundle }
  );

  // ---- R2-10: history in order with the publisher ------------------------
  r2.step = 'R2-10';
  const expectedHistory = (publisher: string): Array<Record<string, unknown>> =>
    [
      { seq: 'v3', current: true, note: 'r2 v3', files: '5', view: false },
      { seq: 'v2', current: false, note: 'r2 v2', files: '4', view: true },
      { seq: 'v1', current: false, note: 'r2 v1', files: '4', view: true },
    ].map((x) => ({ ...x, kind: 'publish', publishedBy: publisher }));
  await j.row(
    'R2-10',
    async (r) => {
      const page = await openArtifact('owner', artifactPage(bundleId()));
      await expect(page.locator(SELECTORS.versionMenu)).toContainText('v3 (current)');
      const rows = await historyRows(page);
      const discarded = await page.locator(RENDER_SELECTORS.historyDiscardedRow).count();
      const menu = await page
        .locator(RENDER_SELECTORS.versionMenuItem)
        .evaluateAll((items) =>
          items.map((i) => (i.textContent ?? '').replace(/\s+/g, ' ').trim())
        );
      r.ui.rows = rows;
      r.ui.discardedRows = discarded;
      r.ui.menu = menu;
      expect.soft(rows, 'history rows').toEqual(expectedHistory('You'));
      expect.soft(discarded, 'no discarded rows').toBe(0);
      expect.soft(menu.map((m) => m.split(' · ')[0]), 'menu order').toEqual([
        'v3 (current)',
        'v2',
        'v1',
      ]);
      expect.soft(menu[1] ?? '', 'menu v2 note').toContain('r2 v2');
      const versions = await j.api(
        'owner',
        'GET',
        `/api/v1/artifacts/${bundleId()}/versions`,
        undefined,
        r
      );
      const body = (versions.body ?? {}) as {
        versions?: Array<{ seq: number; createdByRef?: string }>;
        discarded?: unknown[];
      };
      const apiSeqs = (body.versions ?? []).map((v) => v.seq);
      const apiCreatedBy = (body.versions ?? []).map((v) => v.createdByRef ?? null);
      r2.history.owner = { rows, apiSeqs, apiCreatedBy, discarded };
      expect.soft(versions.status, 'versions 200').toBe(200);
      expect.soft(apiSeqs, 'API order').toEqual([3, 2, 1]);
      expect.soft(apiCreatedBy, 'created by the owner').toEqual(Array(3).fill(j.who('owner').id));
      expect.soft((body.discarded ?? []).length, 'no discarded versions').toBe(0);
      const art = await j.api('owner', 'GET', `/api/v1/artifacts/${bundleId()}`, undefined, r);
      const currentSeq = (art.body as { artifact?: { currentSeq?: number } }).artifact?.currentSeq;
      expect.soft(currentSeq, 'currentSeq').toBe(3);
    },
    { pre: preV3 }
  );

  // ---- R2-11: older versions show their own content ----------------------
  r2.step = 'R2-11';
  await j.row(
    'R2-11',
    async (r) => {
      const page = await openArtifact('owner', artifactPage(bundleId()));
      // v2 through the Version menu (mints counted only from here).
      const mintsBeforeV2 = r2.mints.length;
      await page.locator(SELECTORS.versionMenu).click();
      await versionMenuItem(page, 2).click();
      await expect(page).toHaveURL(new RegExp(`/artifacts/${bundleId()}/v/2$`));
      await expect(page.locator(SELECTORS.versionMenu)).toContainText('Version v2');
      await expect(htmlFrame(page).locator('#ver')).toHaveText('R2 bundle v2');
      const v2Label = await textOf(page.locator(SELECTORS.versionMenu));
      const v2Mint = await latestMint('owner', 2, mintsBeforeV2);
      const v2View = viewInfo((await htmlFrameAttributes(page)).src, bundleId());
      r.ui.v2 = { label: v2Label, heading: await frameHeading(page), viewSeq: v2View.viewSeq };
      expect.soft(v2Label, 'v2 label has no (current)').not.toContain('(current)');
      expect.soft(v2View.viewSeq, 'v2 view names seq 2').toBe(2);
      r.api.push({
        as: 'owner',
        method: 'POST',
        path: `/api/v1/artifacts/${bundleId()}/versions/2/view`,
        status: v2Mint.status,
        code: null,
      });
      expect.soft(v2Mint.status, 'mint v2').toBe(200);
      // v1 through History's View v1.
      await tab(page, 'history').click();
      const mintsBeforeV1 = r2.mints.length;
      await historyViewButton(page, 1).click();
      await expect(page).toHaveURL(new RegExp(`/artifacts/${bundleId()}/v/1$`));
      await tab(page, 'preview').click();
      await expect(htmlFrame(page).locator('#ver')).toHaveText('R2 bundle v1');
      const v1Label = await textOf(page.locator(SELECTORS.versionMenu));
      const historyAtV1 = await historyRows(page);
      await tab(page, 'preview').click();
      await htmlFrame(page).locator('#to-page2').click();
      await expect(htmlFrame(page).locator('#page2')).toHaveText('R2 page two v1');
      const v1Frame = await bundleFrame(page);
      const v1View = viewInfo(v1Frame.url(), bundleId());
      const v1Files = await filesRows(page);
      r.ui.v1 = {
        label: v1Label,
        currentBadgeOn: historyAtV1.filter((x) => x.current).map((x) => x.seq),
        page2: 'R2 page two v1',
        viewSeq: v1View.viewSeq,
        files: v1Files,
      };
      expect.soft(v1Label, 'v1 label').toContain('Version v1');
      expect.soft(v1Label, 'v1 label has no (current)').not.toContain('(current)');
      expect.soft(historyAtV1.filter((x) => x.current).map((x) => x.seq), 'current badge').toEqual([
        'v3',
      ]);
      expect.soft(v1View.viewSeq, 'v1 page link stays on seq 1').toBe(1);
      expect.soft(v1Files.summary, 'v1 Files summary').toMatch(/^4 files · .* · version 1$/);
      expect
        .soft(v1Files.rows.map((x) => x.path), 'v1 has no notes/v3.txt')
        .not.toContain('notes/v3.txt');
      const v1Mint = await latestMint('owner', 1, mintsBeforeV1);
      expect.soft(v1Mint.status, 'mint v1').toBe(200);
      // Back to the current version.
      await tab(page, 'preview').click();
      await page.locator(SELECTORS.versionMenu).click();
      await versionMenuItem(page, 3).click();
      await expect(page).toHaveURL(new RegExp(`/artifacts/${bundleId()}$`));
      await expect(htmlFrame(page).locator('#ver')).toHaveText('R2 bundle v3');
      await htmlFrame(page).locator('#to-page2').click();
      await expect(htmlFrame(page).locator('#page2')).toHaveText('R2 page two v3');
      const v3Files = await filesRows(page);
      const v3Paths = v3Files.rows.map((x) => x.path);
      r.ui.v3 = { heading: 'R2 bundle v3', page2: 'R2 page two v3', files: v3Paths };
      expect.soft(v3Paths, 'v3 lists notes/v3.txt').toContain('notes/v3.txt');
      for (const seq of [2, 1]) {
        const res = await j.api(
          'owner',
          'GET',
          `/api/v1/artifacts/${bundleId()}/versions/${seq}`,
          undefined,
          r
        );
        const got = (res.body as { version?: { seq?: number } } | null)?.version?.seq;
        expect.soft(res.status, `GET versions/${seq}`).toBe(200);
        expect.soft(got, `version ${seq} seq`).toBe(seq);
      }
    },
    { pre: preV3 }
  );

  // ---- R2-12: Files tab and Download -------------------------------------
  r2.step = 'R2-12';
  const expectedFiles = [
    { path: 'css/site.css', type: 'text/css', entry: false, open: 'disabled' },
    { path: 'img/chart.png', type: 'image/png', entry: false, open: 'enabled' },
    { path: 'index.html', type: 'text/html', entry: true, open: 'disabled' },
    { path: 'notes/v3.txt', type: 'text/plain', entry: false, open: 'enabled' },
    { path: 'page2.html', type: 'text/html', entry: false, open: 'disabled' },
  ];
  const filesCheck = async (key: Key, r: RowRecord): Promise<void> => {
    const page = await openArtifact(key, artifactPage(bundleId()));
    await expect(page.locator(SELECTORS.versionMenu)).toBeVisible();
    r.ui.headerDownload = await isShown(headerButton(page, 'Download'));
    expect.soft(r.ui.headerDownload, 'header Download hidden on a bundle').toBe(false);
    const files = await filesRows(page);
    r.ui.files = files;
    expect.soft(files.summary, 'Files summary').toMatch(/^5 files · .* · version 3$/);
    expect
      .soft(
        files.rows.map(({ path: p, type, entry, open }) => ({ path: p, type, entry, open })),
        'Files rows'
      )
      .toEqual(expectedFiles);
    expect.soft(files.rows.filter((x) => x.entry).length, 'one entry badge').toBe(1);
    expect
      .soft(files.rows.find((x) => x.path === 'index.html')?.openLabel, 'index.html not shown')
      .toBe('Not shown in the browser');
    const dl = await downloadFile(page, key, 'img/chart.png');
    await settle(page);
    const want = `/api/v1/artifacts/${bundleId()}/versions/3/files/img/chart.png`;
    const dlRequest = [...j.who(key).requests].reverse().find((q) => q.path === want);
    dl.requestStatus = dlRequest?.status ?? null;
    r.ui.download = dl;
    expect.soft(dl.filename, 'download filename').toBe('chart.png');
    expect.soft(dl.bytes, 'download size').toBe(r2.fileSizes['img/chart.png']);
    expect.soft(dl.savedAbsentAfterDelete, 'downloaded file removed').toBe(true);
    const res = await j.api(key, 'GET', want, undefined, r);
    const contentLength = Number(res.headers['content-length'] ?? -1);
    r.ui.fileGet = { status: res.status, contentLength };
    expect.soft(res.status, 'file GET').toBe(200);
    expect
      .soft(Number(res.headers['content-length'] ?? -1), 'Content-Length')
      .toBe(r2.fileSizes['img/chart.png']);
    r2.files[key] = { ...files, download: dl, fileGet: r.ui.fileGet };
  };
  await j.row(
    'R2-12',
    async (r) => {
      await filesCheck('owner', r);
      const html = await j.api(
        'owner',
        'GET',
        `/api/v1/artifacts/${bundleId()}/versions/3/files/index.html`,
        undefined,
        r
      );
      const disposition = html.headers['content-disposition'] ?? '';
      r.ui.indexHtml = { status: html.status, contentDisposition: disposition };
      expect.soft(html.status, 'index.html GET').toBe(200);
      expect
        .soft(html.headers['content-disposition'] ?? '', 'index.html served as attachment')
        .toMatch(/^attachment/);
    },
    { pre: preV3 }
  );

  // ---- R2-13: reader, bundle page and frame (one generic verdict) ------
  r2.step = 'R2-13';
  await j.row(
    'R2-13',
    (r) =>
      isolationRow(r, async (checks) => {
        const page = await openArtifact('member', artifactPage(bundleId()));
        await page.locator(RENDER_SELECTORS.htmlFrame).waitFor({ state: 'attached' });
        const title = await textOf(headerTitle(page));
        const owner = await textOf(ownerMeta(page));
        const bar = await textOf(page.locator(RENDER_SELECTORS.untrustedBar));
        const attrs = await htmlFrameAttributes(page);
        const info = viewInfo(attrs.src, bundleId());
        const frame = await bundleFrame(page);
        const result = await readProbe(frame);
        const heading = await frameHeading(page);
        const chart = await frame.evaluate(
          () => (document.getElementById('chart') as HTMLImageElement | null)?.naturalWidth ?? -1
        );
        await htmlFrame(page).locator('#to-page2').click();
        const page2 = await textIs(htmlFrame(page).locator('#page2'), 'R2 page two v3');
        await settle(page);
        const session = await frameSessionFacts(requestsIn('member', ['R2-13']));
        if (session.bearing > 0) r2.frameSessionAlert = true;
        const caps = await j.api('member', 'GET', `/api/v1/artifacts/${bundleId()}`, undefined, r);
        const mint = await latestMint('member', 3);
        r.api.push({
          as: 'member',
          method: 'POST',
          path: `/api/v1/artifacts/${bundleId()}/versions/3/view`,
          status: mint.status,
          code: null,
        });
        const rec = {
          who: 'member',
          step: 'R2-13',
          title,
          owner,
          bar,
          frame: {
            sandboxTokens: tokens(attrs.sandbox),
            referrerpolicy: attrs.referrerpolicy,
            title: attrs.title,
            ...info,
          },
          heading,
          chartNaturalWidth: chart,
          page2,
          probe: result,
          session,
          canPublish: (caps.body as { canPublish?: boolean } | null)?.canPublish ?? null,
        };
        r2.probes.push({ ...result, who: 'member', step: 'R2-13', session });
        r2.frames.push({ who: 'member', step: 'R2-13', kind: 'html', ...rec.frame });
        r.ui.reader = rec;
        const labels = ownerLabels();
        const barText = (l: string): string =>
          `Content published by ${l} · runs sandboxed, isolated from Scion`;
        checks.title = title === r2.bundle!.title;
        checks.ownerLabel = owner !== 'Owner: You' && labels.some((l) => owner === `Owner: ${l}`);
        checks.untrustedBar = labels.some((l) => bar === barText(l));
        checks.sandboxExact = JSON.stringify(rec.frame.sandboxTokens) === '["allow-scripts"]';
        checks.referrerPolicy = attrs.referrerpolicy === 'no-referrer';
        checks.frameTitle = attrs.title === `${r2.bundle!.title} (sandboxed)`;
        checks.viewSrc = info.viewSeq === 3 && info.viewIdMatches && info.entry === 'index.html';
        checks.heading = heading === 'R2 bundle v3';
        checks.image = chart === CHART_SIZE.width;
        checks.relativeLink = page2;
        checks.opaqueOrigin = result.origin === 'null';
        checks.parentDomBlocked = String(result.parentDom).startsWith('blocked:');
        checks.cookieBlocked = String(result.cookie).startsWith('blocked:');
        checks.fetchBlocked = String(result.fetch).startsWith('blocked:');
        checks.frameRequestsSeen = session.total > 0;
        checks.headersAllRead = session.unread === 0;
        checks.noSessionBearing = session.bearing === 0;
        checks.readOnly = caps.status === 200 && rec.canPublish === false;
        checks.minted = mint.status === 200;
      }),
    { pre: preV3 }
  );

  // ---- R2-14: reader history ---------------------------------------------
  r2.step = 'R2-14';
  await j.row(
    'R2-14',
    async (r) => {
      const page = await openArtifact('member', artifactPage(bundleId()));
      await expect(page.locator(SELECTORS.versionMenu)).toContainText('v3 (current)');
      const ownerLabel = (await textOf(ownerMeta(page))).replace(/^Owner:\s*/, '');
      const rows = await historyRows(page);
      r.ui.rows = rows;
      r.ui.ownerLabel = ownerLabel;
      expect.soft(ownerLabel, 'owner label is not You').not.toBe('You');
      expect.soft(rows, 'reader history').toEqual(expectedHistory(ownerLabel));
      await historyViewButton(page, 1).click();
      await expect(page).toHaveURL(new RegExp(`/artifacts/${bundleId()}/v/1$`));
      await tab(page, 'preview').click();
      await expect(htmlFrame(page).locator('#ver')).toHaveText('R2 bundle v1');
      r.ui.v1Label = await textOf(page.locator(SELECTORS.versionMenu));
      expect.soft(String(r.ui.v1Label), 'v1 label').toContain('Version v1');
      await htmlFrame(page).locator('#to-page2').click();
      await expect(htmlFrame(page).locator('#page2')).toHaveText('R2 page two v1');
      r.ui.v1Page2 = 'R2 page two v1';
      const versions = await j.api(
        'member',
        'GET',
        `/api/v1/artifacts/${bundleId()}/versions`,
        undefined,
        r
      );
      const body = (versions.body ?? {}) as {
        versions?: Array<{ seq: number; createdByRef?: string }>;
      };
      const apiSeqs = (body.versions ?? []).map((v) => v.seq);
      const apiCreatedBy = (body.versions ?? []).map((v) => v.createdByRef ?? null);
      r2.history.member = { rows, apiSeqs, apiCreatedBy };
      expect.soft(versions.status, 'versions 200').toBe(200);
      expect.soft(apiSeqs, 'same seqs as the owner').toEqual([3, 2, 1]);
      expect
        .soft(apiCreatedBy, 'same publisher as the owner saw')
        .toEqual(Array(3).fill(j.who('owner').id));
    },
    { pre: preV3 }
  );

  // ---- R2-15: reader Files and Download ----------------------------------
  r2.step = 'R2-15';
  await j.row('R2-15', (r) => filesCheck('member', r), { pre: preV3 });

  // ---- R2-16: reader markdown --------------------------------------------
  r2.step = 'R2-16';
  await j.row(
    'R2-16',
    async (r) => {
      const page = await openMarkdown('member');
      await markdownFrame(page).getByText('hover marker').hover();
      await page.waitForTimeout(500);
      const facts = await markdownFacts(page);
      const flags = await parentFlags(page);
      r.ui.facts = facts;
      r2.frames.push({ who: 'member', step: 'R2-16', kind: 'markdown', ...facts });
      r2.markdownInert.push({
        who: 'member',
        step: 'R2-16',
        scripts: facts.scripts,
        onAttrs: facts.onAttrs,
        jsHrefs: facts.jsHrefs,
        embeds: facts.embeds,
        forms: facts.forms,
        styleKept: facts.styleKept,
        parentFlag: flags.r2md,
      });
      expectMarkdownRendered(facts, 1);
      expectMarkdownInert(facts, flags);
      const art = await j.api('member', 'GET', `/api/v1/artifacts/${markdownId()}`, undefined, r);
      expect.soft(art.status, 'GET markdown').toBe(200);
      for (const p of ['index.md', 'img/dot.png']) {
        const res = await j.api(
          'member',
          'GET',
          `/api/v1/artifacts/${markdownId()}/versions/1/files/${p}?stream=1`,
          undefined,
          r
        );
        expect.soft(res.status, `GET ${p}`).toBe(200);
      }
    },
    { pre: preMarkdown }
  );

  // ---- R2-17: outsider ---------------------------------------------------
  r2.step = 'R2-17';
  await j.row(
    'R2-17',
    async (r) => {
      const page = await openArtifact('outsider', artifactPage(bundleId()));
      r.ui.rendered404 = await isShown(notFoundHeading(page));
      r.ui.frames = await page.locator(RENDER_SELECTORS.htmlFrame).count();
      expect.soft(r.ui.rendered404, 'outsider: 404 page').toBe(true);
      expect.soft(r.ui.frames, 'outsider: no frame').toBe(0);
      const real = await j.api(
        'outsider',
        'POST',
        `/api/v1/artifacts/${bundleId()}/versions/1/view`,
        undefined,
        r
      );
      const random = await j.api(
        'outsider',
        'POST',
        `/api/v1/artifacts/${crypto.randomUUID()}/versions/1/view`,
        undefined,
        r
      );
      expect.soft(real.status, 'mint view for a non-reader').toBe(404);
      expect
        .soft([real.status, real.code], 'same as for a random id')
        .toEqual([random.status, random.code]);
    },
    { pre: preBundle }
  );

  // ---- R2-17a: a held view URL works without a session -------------------
  r2.step = 'R2-17a';
  await j.row(
    'R2-17a',
    async (r) => {
      const held = r2.heldViewUrl;
      if (!held) throw new Error('no view URL was held from R2-02');
      const context = await browser.newContext({ baseURL: j.target().baseURL });
      try {
        watchContext(context, 'anonymous');
        const page = await context.newPage();
        const res = await page.goto(held, { waitUntil: 'load' });
        r.ui.status = res?.status() ?? null;
        r.ui.heading = await textOf(page.locator('#ver'));
        r.api.push({
          as: 'anonymous',
          method: 'GET',
          path: redactPath(new URL(held, j.target().baseURL).pathname),
          status: res?.status() ?? 0,
          code: null,
        });
        expect.soft(r.ui.status, 'view URL without a session').toBe(200);
        expect.soft(r.ui.heading, 'view renders without a session').toBe('R2 bundle v1');
      } finally {
        await context.close();
      }
    },
    { pre: preHeld }
  );

  // ---- R2-20: markdown in the chat preview -------------------------------
  r2.step = 'R2-20';
  await runChatRow();

  // No page stays open into the end of the test (defence in depth: nothing
  // left for a failure-time page capture).
  for (const key of KEYS) {
    for (const p of j.state.ids[key]?.context.pages() ?? []) await p.close();
  }

  // Every identity still holds its own session after the rows.
  const checks = await j.identityChecks();
  j.state.evidence.identityChecksAfterRows = checks;
  expect
    .soft(Object.values(checks), 'identities asserted after the rows')
    .toEqual(Object.values(checks).map(() => 'ok'));
  expect(j.state.hardFailures, 'rows that could not be carried out').toEqual([]);
}

/** R2-20 is filled in by the chat fixture (see chatRow below). */
async function runChatRow(): Promise<void> {
  await j.row('R2-20', (r) => chatRow(r), { pre: preMarkdown });
  const send = r2.chat.send as Record<string, unknown> | undefined;
  if (!send) {
    j.notRun(['R2-20x'], 'no chat message was sent (see R2-20)');
    return;
  }
  await j.row('R2-20x', (r) => {
    r.ui = {
      chips: (r2.chat.preview as { chips?: number } | undefined)?.chips ?? null,
      referencesRecorded: send.referencesRecorded ?? null,
      artifactWarning: send.artifactWarning ?? null,
      sendStatus: send.status ?? null,
      note: 'chip behaviour not graded; a chip count of 0 refers to ptone/scion#4905',
    };
  });
}

/**
 * R2-20: markdown in the chat preview. Fixture (API, owner): a project
 * thread and one message whose text carries the markdown's reference. The
 * reader opens the thread; the reference is a link that opens the chat
 * preview (chat-message.ts:286-317, 2121-2140), which renders the markdown
 * in the same frame as the artifact page (chat-file-preview.ts:917-926).
 *
 * The message also sends the reference as metadata.artifacts (what the
 * composer sends). Whether the hub kept it (the chip) is recorded only: on
 * a hub with no agent, a user-to-user send does not record references.
 */
async function chatRow(r: RowRecord): Promise<void> {
  const projectId = j.state.projectId!;
  const thread = await j.api(
    'owner',
    'POST',
    `/api/v1/chat/spaces/${projectId}/threads`,
    { name: `R2 chat ${suffix()}` },
    r
  );
  const topicId = (thread.body as { id?: string } | null)?.id;
  r2.chat.threadCreate = thread.status;
  if (thread.status !== 201 || !topicId) {
    r.legNotRun = `chat thread could not be created (${thread.status} ${String(thread.code)})`;
    return;
  }
  r2.chat.topicId = topicId;
  const ref = `scion://artifact/${markdownId()}`;
  const sent = await j.api(
    'owner',
    'POST',
    `/api/v1/chat/conversations/${encodeURIComponent(topicId)}/messages`,
    {
      content: `R2 markdown for review: ${ref}`,
      idempotency_key: crypto.randomUUID(),
      metadata: { artifacts: JSON.stringify([ref]) },
    },
    r
  );
  const sentBody = (sent.body ?? {}) as { artifacts?: unknown[]; artifactWarning?: string };
  r2.chat.send = {
    status: sent.status,
    referencesRecorded: Array.isArray(sentBody.artifacts) ? sentBody.artifacts.length : 0,
    artifactWarning: sentBody.artifactWarning ?? null,
  };
  if (sent.status < 200 || sent.status >= 300) {
    r.legNotRun = `chat message could not be sent (${sent.status} ${String(sent.code)})`;
    return;
  }

  const page = await j.pageOf('member');
  await page.goto(`/chat/space/${projectId}/thread/${topicId}`, { waitUntil: 'domcontentloaded' });
  const link = page.locator(
    `${RENDER_SELECTORS.chatArtifactLink}[data-artifact-id="${markdownId()}"]`
  );
  await expect(link, 'reference shown as a link').toBeVisible({ timeout: 30_000 });
  r.ui.chips = await page.locator(RENDER_SELECTORS.chatArtifactChip).count();
  await link.click();
  const dialog = page.locator(RENDER_SELECTORS.chatPreviewDialog);
  await expect(dialog, 'chat preview opened').toBeVisible();
  const frameSel = RENDER_SELECTORS.chatMarkdownIframe;
  await page.frameLocator(frameSel).locator('h1').waitFor({ state: 'visible' });
  await expect
    .poll(async () => {
      const f = await markdownFacts(page, frameSel);
      const img = f.img as { complete: boolean; naturalWidth: number } | null;
      return !!img && img.complete && img.naturalWidth > 0;
    })
    .toBe(true);
  await page.frameLocator(frameSel).getByText('hover marker').hover();
  await page.waitForTimeout(500);
  const facts = await markdownFacts(page, frameSel);
  const flags = await parentFlags(page);
  const viewer = dialog.locator('sl-button').filter({ hasText: /Open in artifact viewer/ });
  r.ui.facts = facts;
  r.ui.label = await textOf(page.locator(RENDER_SELECTORS.chatPreviewLabel));
  r.ui.openInViewer = {
    visible: await isShown(viewer),
    href: (await viewer.first().getAttribute('href')) ?? null,
  };
  r2.chat.preview = { label: r.ui.label, openInViewer: r.ui.openInViewer, chips: r.ui.chips };
  r2.frames.push({ who: 'member', step: 'R2-20', kind: 'markdown (chat)', ...facts });
  r2.markdownInert.push({
    who: 'member',
    step: 'R2-20',
    scripts: facts.scripts,
    onAttrs: facts.onAttrs,
    jsHrefs: facts.jsHrefs,
    embeds: facts.embeds,
    forms: facts.forms,
    styleKept: facts.styleKept,
    parentFlag: flags.r2md,
  });
  expect.soft(String(r.ui.label), 'preview names the artifact').toContain(r2.markdown!.title);
  expectMarkdownRendered(facts, 1);
  expectMarkdownInert(facts, flags);
  expect
    .soft((r.ui.openInViewer as { visible: boolean }).visible, 'Open in artifact viewer')
    .toBe(true);
  expect
    .soft((r.ui.openInViewer as { href: string | null }).href, 'viewer link')
    .toBe(`/projects/${projectId}/artifacts/${markdownId()}`);
  const art = await j.api('member', 'GET', `/api/v1/artifacts/${markdownId()}`, undefined, r);
  expect.soft(art.status, 'member reads the referenced artifact').toBe(200);
}

/** Every artifact this run created, as [name, id] (known from its create on). */
function createdArtifacts(): Array<[string, string]> {
  const out: Array<[string, string]> = [];
  if (r2.bundle) out.push(['bundle', r2.bundle.id]);
  const md = r2.markdown?.id ?? r2.markdownCreatedId;
  if (md) out.push(['markdown', md]);
  r2.extraArtifactIds.forEach((id, i) => out.push([`extra ${i + 1}`, id]));
  return out;
}

async function runCleanup(testInfo: {
  annotations: ReadonlyArray<{ type: string; description?: string }>;
  project: { outputDir: string };
}): Promise<void> {
  const residue: string[] = [];
  const cleanup: Record<string, unknown> = {};
  const states: Record<string, string> = {};
  r2.step = 'cleanup';

  // Whole-run sweep, here so it runs after a timeout too: every frame
  // request of every identity (restricted; decides no row). A session-
  // bearing or unread one, or a sweep that did not complete, raises the
  // restricted alert that the grader escalates by direct message.
  try {
    const all = await frameSessionFacts(r2.frameRequests.filter((q) => q.who !== 'anonymous'));
    const unavailable = r2.frameRequests.filter((q) => q.frameUnavailable).length;
    r2.frameSessionAll = { swept: true, ...all, frameUnavailable: unavailable };
    if (all.bearing > 0 || all.unread > 0) {
      r2.frameSessionAlert = true;
      j.state.findings.push({ id: 'frame-session-sweep', text: ISOLATION_FAIL });
    }
  } catch {
    r2.frameSessionAll = { swept: false };
  }
  if (r2.frameSessionAll.swept !== true) r2.frameSessionAlert = true;

  for (const identity of Object.values(j.state.ids) as Array<Identity<Key> | undefined>) {
    for (const page of identity?.context.pages() ?? []) await page.close();
  }

  // Both artifacts, as the owner (soft delete).
  const artifactDeletes: Record<string, number | string> = {};
  for (const [name, id] of createdArtifacts()) {
    if (!j.state.ids.owner) continue;
    try {
      const del = await j.api('owner', 'DELETE', `/api/v1/artifacts/${id}`);
      artifactDeletes[name] = del.status;
      if (del.status !== 204) residue.push(`${name} delete answered ${del.status}`);
    } catch (err) {
      residue.push(`cleanup error (${name} delete): ${firstLine(err)}`);
    }
  }
  cleanup.artifactDeletes = artifactDeletes;

  // The held view URL no longer serves the deleted artifact.
  if (r2.heldViewUrl && j.state.target) {
    try {
      const res = await fetch(new URL(r2.heldViewUrl, j.target().baseURL));
      await res.arrayBuffer();
      cleanup.heldViewAfterDelete = res.status;
      states['held view URL after delete'] = res.status === 404 ? 'absent' : `status ${res.status}`;
    } catch (err) {
      residue.push(`cleanup error (held view URL): ${firstLine(err)}`);
    }
  }

  // The chat thread, while its project still exists (so a read proves the delete).
  const chatCleanup = await cleanupChat(residue, states);
  if (chatCleanup) cleanup.chat = chatCleanup;

  // The membership, then the project.
  const removals: Record<string, number | string> = {};
  for (const b of j.state.bindings) {
    try {
      const res = await j.api('owner', 'DELETE', `/api/v1/projects/${b.projectId}/members/${b.id}`);
      removals[b.key] = res.status;
      if (res.status >= 300) residue.push(`membership ${b.key}: DELETE ${res.status}`);
    } catch (err) {
      removals[b.key] = `error: ${firstLine(err)}`;
      residue.push(`cleanup error (membership ${b.key}): ${firstLine(err)}`);
    }
  }
  cleanup.memberRemovals = removals;
  if (j.state.projectId && j.state.bindings.length > 0) {
    try {
      const res = await j.api('owner', 'GET', `/api/v1/projects/${j.state.projectId}/members`);
      const items =
        ((res.body ?? {}) as { items?: Array<{ id: string; principalId?: string }> }).items ?? [];
      const anchored = res.status === 200 && items.some((i) => i.principalId === j.who('owner').id);
      cleanup.membersAnchor = anchored;
      for (const b of j.state.bindings) {
        const userId = j.state.ids[b.key]?.id;
        const present = items.some((i) => i.id === b.id || (!!userId && i.principalId === userId));
        states[`binding ${b.key}`] = anchored ? (present ? 'present' : 'absent') : 'unknown';
      }
    } catch (err) {
      residue.push(`cleanup error (members): ${firstLine(err)}`);
    }
  }
  if (j.state.projectId && j.state.ids.owner) {
    try {
      const res = await j.api('owner', 'DELETE', `/api/v1/projects/${j.state.projectId}`);
      cleanup.projectDelete = res.status;
      if (res.status >= 300) residue.push(`project: DELETE ${res.status}`);
    } catch (err) {
      residue.push(`cleanup error (project): ${firstLine(err)}`);
    }
  }
  // Final reads while the sessions still exist.
  try {
    for (const [name, id] of createdArtifacts()) {
      if (!j.state.ids.owner) continue;
      const own = await j.api('owner', 'GET', `/api/v1/artifacts/${id}`);
      states[`${name} (owner read)`] = classifyRead(own.status, own.body, 'artifact');
      const list = await j.api('owner', 'GET', '/api/v1/artifacts?mine=1');
      const listed = ((list.body ?? {}) as { artifacts?: Array<{ id: string }> }).artifacts?.some(
        (x) => x.id === id
      );
      states[`${name} (owner list)`] =
        list.status === 200 && !listed ? 'absent' : 'present_in_list';
      if (j.state.ids.member) {
        const m = await j.api('member', 'GET', `/api/v1/artifacts/${id}`);
        states[`${name} (member read)`] = classifyRead(m.status, m.body, 'artifact');
      }
    }
    if (j.state.projectId && j.state.ids.owner) {
      const res = await j.api('owner', 'GET', `/api/v1/projects/${j.state.projectId}`);
      states.project = classifyRead(res.status, res.body);
    }
  } catch (err) {
    residue.push(`cleanup error (final reads): ${firstLine(err)}`);
  }

  // The experiment override, only if this run set it.
  if (j.state.target && j.adminToken() && r2.experimentSetByRun) {
    try {
      const after = await j.setExperiment(null);
      cleanup.experimentReset = after;
      const experiment = j.state.evidence.experiment as Record<string, unknown> | undefined;
      if (experiment) experiment.afterReset = after;
      if (after.override !== null) residue.push(`${EXPERIMENT} override still set`);
    } catch (err) {
      residue.push(`cleanup error (experiment reset): ${firstLine(err)}`);
    }
  } else {
    cleanup.experimentReset = 'not set by this run';
  }

  Object.assign(cleanup, await j.deleteIdentities(residue, states));

  // Request logs, then close the contexts and remove the temp files.
  j.state.evidence.requests = Object.fromEntries(
    (Object.values(j.state.ids) as Array<Identity<Key> | undefined>)
      .filter((i): i is Identity<Key> => !!i)
      .map((i) => [i.key, i.requests])
  );
  for (const identity of Object.values(j.state.ids) as Array<Identity<Key> | undefined>) {
    await identity?.context.close();
  }
  r2.downloads.forEach((d, i) => {
    states[`download ${d.who} #${i + 1}`] = fs.existsSync(d.file) ? 'present' : 'absent';
  });
  if (j.state.tmpDir) {
    fs.rmSync(j.state.tmpDir, { recursive: true, force: true });
    states['fixture temp dir'] = fs.existsSync(j.state.tmpDir) ? 'present' : 'absent';
  }

  for (const [what, state] of Object.entries(states)) {
    if (state !== 'absent' && !(state !== 'present_in_list' && isGone(state as ReadState))) {
      residue.push(`final: ${what} is ${state}`);
    }
  }
  cleanup.final = { states };

  // Rows nothing reached keep "not run", with the first cause.
  const cause =
    j.state.setupError ?? j.state.testError ?? j.state.stopped ?? j.state.hardFailures[0];
  for (const row of Object.values(j.state.rows)) {
    if (row.status === 'not run' && row.reason === NOT_REACHED && cause) {
      row.reason = `${NOT_REACHED}: ${cause}`;
    }
  }
  for (const [id, reason] of Object.entries(SKIPPED)) {
    if (j.state.rows[id]?.reason?.startsWith(NOT_REACHED)) j.notRun([id], reason);
  }

  // R2-18: cleanup verified by absence.
  const r18 = j.rowRecord('R2-18');
  delete r18.reason;
  r18.status = residue.length === 0 ? 'passed' : 'failed';
  r18.errors = [...residue];
  r18.ui = { final: cleanup.final, heldViewAfterDelete: cleanup.heldViewAfterDelete ?? null };

  // Evidence only now, after the cleanup.
  const ev = j.state.evidence;
  ev.artifacts = {
    bundle: r2.bundle ? { id: r2.bundle.id, title: r2.bundle.title, seqs: [1, 2, 3] } : null,
    markdown: r2.markdown ? { id: r2.markdown.id, title: r2.markdown.title } : null,
  };
  ev.frames = r2.frames;
  ev.probes = r2.probes;
  ev.frameRequests = r2.frameRequests;
  ev.mints = r2.mints.map((m) => ({
    who: m.who,
    seq: m.seq,
    status: m.status,
    remoteImages: m.remoteImages,
    expiresAt: m.expiresAt,
  }));
  ev.markdownInert = r2.markdownInert;
  ev.history = r2.history;
  ev.files = r2.files;
  ev.publishes = r2.publishes;
  ev.chat = r2.chat;
  ev.frameSessionAlert = r2.frameSessionAlert;
  ev.frameSessionAll = r2.frameSessionAll;
  ev.rows = j.state.rows;
  ev.findings = j.state.findings;
  ev.hardFailures = j.state.hardFailures;
  ev.setupError = j.state.setupError ?? null;
  ev.testError = j.state.testError ?? null;
  ev.cleanup = { ...cleanup, residue };
  const results = (ev.results ??= {}) as Record<string, unknown>;
  results[TITLE_MAIN] ??= { status: 'not run', skipReason: cause ?? null };
  for (const [id, reason] of Object.entries(SKIPPED)) {
    results[skipTitle(id)] ??= { status: 'not run', skipReason: reason };
  }
  results[TITLE_CLEANUP] = {
    status: residue.length === 0 ? 'passed' : 'failed',
    residue: residue.length,
    annotations: annotationsOf(testInfo.annotations),
  };

  // R2-19: nothing secret-shaped, no view capability, no cookie values.
  const r19 = j.rowRecord('R2-19');
  delete r19.reason;
  const serialised = JSON.stringify(ev);
  const leaks = secretLikeStrings(serialised);
  const capabilityLeaks = capabilityLike(serialised);
  r19.status = leaks.length === 0 && capabilityLeaks === 0 ? 'passed' : 'failed';
  r19.ui = {
    secretLikeMatches: leaks.length,
    viewCapabilityMatches: capabilityLeaks,
    cookieValues: 'never recorded (names only)',
    traces: 'off',
    video: 'off',
    screenshots: 'off',
    har: 'not recorded',
  };
  r19.errors = [
    ...leaks.map((l) => `secret-like string at offset ${l}`),
    ...(capabilityLeaks > 0 ? [`${capabilityLeaks} view capability strings`] : []),
  ];
  // The full body is written only if the check passed; otherwise a stub with the counts.
  const breach = leaks.length > 0 || capabilityLeaks > 0;
  const body = breach
    ? JSON.stringify(
        {
          rows: { 'R2-19': r19 },
          withheld: 'evidence not written: secret-like or capability strings found',
        },
        null,
        2
      )
    : JSON.stringify(ev, null, 2);
  const evidenceFile = path.join(testInfo.project.outputDir, 'artifacts-render-evidence.json');
  fs.mkdirSync(path.dirname(evidenceFile), { recursive: true });
  fs.writeFileSync(evidenceFile, body, { mode: 0o600 });
  console.info(`[journey] evidence: ${evidenceFile}`);
  console.info(`[journey] cleanup residue: ${residue.length}`);

  expect(leaks, 'R2-19: no secret-like strings in the evidence').toEqual([]);
  expect(capabilityLeaks, 'R2-19: no view capabilities in the evidence').toBe(0);
  expect(residue, 'R2-18: nothing left behind').toEqual([]);
}

/**
 * Counts view capabilities in text: a view path segment other than
 * <redacted>, or the capability shape <uuid>.<seq>.<exp>.<signature>.
 */
function capabilityLike(text: string): number {
  const inPath = [...text.matchAll(/\/api\/v1\/artifacts\/view\/([^/"\s]+)/g)].filter(
    (m) => m[1] !== '<redacted>' && m[1] !== '%3Credacted%3E'
  ).length;
  const shape = [
    ...text.matchAll(
      /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.\d+\.\d{9,}\.[A-Za-z0-9_-]{20,}/gi
    ),
  ].length;
  return inPath + shape;
}

/**
 * Deletes the chat thread, if one was created, as the owner, and proves it
 * is gone while its project still exists: the DELETE must answer 2xx and a
 * read must then answer 404 or show deletedAt.
 */
async function cleanupChat(
  residue: string[],
  states: Record<string, string>
): Promise<Record<string, unknown> | null> {
  const topicId = r2.chat.topicId as string | undefined;
  if (!topicId || !j.state.ids.owner) return null;
  const out: Record<string, unknown> = {};
  try {
    const topicPath = `/api/v1/chat/topics/${encodeURIComponent(topicId)}`;
    const del = await j.api('owner', 'DELETE', topicPath);
    out.threadDelete = del.status;
    const deleted = del.status >= 200 && del.status < 300;
    if (!deleted) residue.push(`chat thread: DELETE ${del.status}`);
    const get = await j.api('owner', 'GET', topicPath);
    out.threadReadAfter = get.status;
    const deletedAt = (get.body as { deletedAt?: string } | null)?.deletedAt;
    out.deletedAtSet = !!deletedAt;
    const gone = deleted && (get.status === 404 || !!deletedAt);
    states['chat thread'] = gone ? 'absent' : `delete ${del.status}, read ${get.status}`;
  } catch (err) {
    residue.push(`cleanup error (chat thread): ${firstLine(err)}`);
  }
  return out;
}
