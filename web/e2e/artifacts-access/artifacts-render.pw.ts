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
  'NOT RUN: the chat HTML preview is not on this build (ptone/scion#4580); rows frozen';

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
  'R2-17b': { role: 'anonymous', title: 'a held view URL is refused after it expires', where: 'skip' },
  'R2-20': { role: 'member', title: 'markdown in the chat preview' },
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
  cookieHeader: { present: boolean; names: string[] };
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
  mints: [] as Mint[],
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
  /** Restricted: set when a frame request carried the viewer's session. */
  sessionBearingFrameRequest: false,
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
  if (!pathname.startsWith(VIEW_PREFIX)) return { viewSeq: null, viewIdMatches: false, entry: null };
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
  let frameUrl = '';
  try {
    frameUrl = req.frame().url();
  } catch {
    // A service-worker or detached request: not a bundle request.
    return false;
  }
  const reqPath = new URL(req.url()).pathname;
  return frameUrl.includes(VIEW_PREFIX) || reqPath.startsWith(VIEW_PREFIX);
}

/**
 * Records the bundle requests of a context (path redacted, status, and
 * whether a Cookie header was sent, with cookie names only), and every view
 * mint answer (status, seq and the URL in memory).
 */
function watchContext(context: BrowserContext, who: Key | 'anonymous'): void {
  const records = new Map<Request, FrameRequest>();
  context.on('request', (req) => {
    if (!/^https?:/.test(req.url()) || !isBundleRequest(req)) return;
    let topLevelDocument = false;
    try {
      topLevelDocument = req.isNavigationRequest() && req.frame().parentFrame() === null;
    } catch {
      topLevelDocument = false;
    }
    const rec: FrameRequest = {
      who,
      step: r2.step,
      method: req.method(),
      path: redactPath(new URL(req.url()).pathname),
      resourceType: req.resourceType(),
      topLevelDocument,
      cookieHeader: { present: false, names: [] },
    };
    records.set(req, rec);
    r2.frameRequests.push(rec);
    void req
      .allHeaders()
      .then((h) => {
        rec.cookieHeader = { present: !!h.cookie, names: cookieNames(h.cookie) };
      })
      .catch(() => undefined);
  });
  context.on('response', (res) => {
    const rec = records.get(res.request());
    if (rec) {
      rec.status = res.status();
      void res
        .request()
        .allHeaders()
        .then((h) => {
          rec.cookieHeader = { present: !!h.cookie, names: cookieNames(h.cookie) };
        })
        .catch(() => undefined);
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

/** Cookie-bearing bundle requests other than a top-level document (restricted detail). */
function sessionBearing(fromIndex: number): FrameRequest[] {
  return r2.frameRequests
    .slice(fromIndex)
    .filter((q) => !q.topLevelDocument && q.cookieHeader.present);
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

/** The probe result of a bundle document (waits for done:true). */
async function readProbe(frame: Frame | Page): Promise<Record<string, unknown>> {
  const probe = frame.locator('#probe');
  await expect(probe).toContainText('"done":true', { timeout: 10_000 });
  return JSON.parse((await probe.textContent()) ?? '{}') as Record<string, unknown>;
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

/** The response of the most recent document request for a view path's entry. */
function lastViewDocument(who: Key | 'anonymous', entry: string): FrameRequest | undefined {
  return [...r2.frameRequests]
    .reverse()
    .find((q) => q.who === who && q.resourceType === 'document' && q.path.endsWith(`/${entry}`));
}

/** Waits for the latest mint of seq by key and returns it (URL in memory). */
async function latestMint(key: Key, seq: number): Promise<Mint> {
  await expect
    .poll(
      () => [...r2.mints].reverse().find((m) => m.who === key && m.seq === seq && m.url !== null),
      { message: `view minted for v${seq}` }
    )
    .toBeTruthy();
  return [...r2.mints].reverse().find((m) => m.who === key && m.seq === seq && m.url !== null)!;
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
  key: Key,
  dir: string,
  opts: { title?: string; note: string; createPath: RegExp }
): Promise<{ createStatus: number; createCode: string | null; entry: string }> {
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
  } | null;
  const code = body?.error?.code ?? body?.code ?? null;
  void key;
  return { createStatus: created.status(), createCode: code, entry };
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
  filePath: string
): Promise<{ filename: string; bytes: number; requestStatus: number | null }> {
  const row = fileRow(page, filePath);
  const button = fileIconButton(row, `Download ${filePath}`);
  await expect(button).toBeVisible();
  const [download] = await Promise.all([page.waitForEvent('download'), button.click()]);
  const saved = await download.path();
  const bytes = saved ? fs.statSync(saved).size : -1;
  const filename = download.suggestedFilename();
  await download.delete();
  return { filename, bytes, requestStatus: null };
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
      notesTarget: notes?.getAttribute('target') ?? doc?.querySelector('base')?.getAttribute('target') ?? null,
      scripts: doc?.querySelectorAll('script').length ?? -1,
      onAttrs,
      jsHrefs: Array.from(doc?.querySelectorAll('a[href]') ?? []).filter((a) =>
        /^\s*javascript:/i.test(a.getAttribute('href') ?? '')
      ).length,
      embeds: doc?.querySelectorAll('iframe, object, embed').length ?? -1,
      forms: doc?.querySelectorAll('form').length ?? -1,
      styleKept: (doc?.querySelectorAll('body style').length ?? 0) > 0,
      parentFlag: (window as unknown as { __r2md?: unknown }).__r2md ?? null,
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
  expect.soft(facts.parentFlag, 'no fixture script ran (frame view)').toBeNull();
  expect.soft(flags.r2md, 'no fixture script ran (app page)').toBeNull();
}

/** One generic soft check of the isolation rows. */
function iso(ok: boolean): void {
  expect.soft(ok, ISOLATION_FAIL).toBe(true);
}

/**
 * Runs an isolation row body: any throw becomes the generic message, so no
 * detail reaches Playwright output; the detail stays in r2's fields.
 */
async function isolated(fn: () => Promise<void>): Promise<void> {
  try {
    await fn();
  } catch {
    throw new Error(ISOLATION_FAIL);
  }
}

/** Notes a session-bearing frame request: restricted detail stays in the evidence only. */
function noteSessionBearing(found: FrameRequest[], rec: RowRecord): void {
  if (found.length === 0) return;
  r2.sessionBearingFrameRequest = true;
  rec.ui.sessionBearingFrameRequests = found.length;
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

// ---- NOT RUN by design: declared outside the serial group so each body runs.
test.describe('render rows not run at this build', () => {
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
  // eslint-disable-next-line no-empty-pattern -- Playwright needs the fixtures argument
  test.afterEach(({}, testInfo) => {
    if (testInfo.title === TITLE_MAIN && testInfo.error?.message) {
      j.state.testError ??= firstLine(testInfo.error.message);
    }
    j.recordResult(testInfo);
  });

  test.beforeAll(async ({ browser }) => {
    j.seedRows();
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
    const pub = await publishThroughDialog(page, 'owner', r2.dirs['bundle-v1']!, {
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
    expect.soft(pub.entry, 'entry defaults to index.html').toBe('index.html');
    await expect(page).toHaveURL(
      new RegExp(`/projects/${j.state.projectId}/artifacts/[0-9a-f-]+$`)
    );
    const id = new URL(page.url()).pathname.split('/').pop()!;
    r2.bundle = { id, title };
    await expect(headerTitle(page)).toHaveText(title);
    await expect(page.locator(SELECTORS.versionMenu)).toContainText('v1 (current)');
    r.ui.landed = redactPath(new URL(page.url()).pathname);
    r.ui.versionLabel = await textOf(page.locator(SELECTORS.versionMenu));
    const calls = publishCalls('owner', since, 1);
    r.api.push(...calls.map((c) => ({ as: 'owner', ...c, status: c.status ?? 0, code: null })));
    r2.publishes.push({ artifact: 'bundle', seq: 1, calls });
    const finalize = calls.find((c) => c.path.endsWith('/finalize'));
    smoke.finalizeStatus = finalize?.status ?? null;
    if (finalize && (finalize.status === 401 || finalize.status === 403)) {
      j.finding('R2-01', `member session cannot finalize a publish (${finalize.status})`);
    }
    expect.soft(finalize?.status ?? 0, 'finalize 2xx').toBeGreaterThanOrEqual(200);
    expect.soft(finalize?.status ?? 0, 'finalize 2xx').toBeLessThan(300);
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
    { requires: ['R2-01'] }
  );

  // ---- R2-03: renders with its own files ---------------------------------
  r2.step = 'R2-03';
  await j.row(
    'R2-03',
    async (r) => {
      const page = await j.pageOf('owner');
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
      const own = r2.frameRequests.filter((q) => q.who === 'owner' && q.step <= 'R2-03');
      const css = own.find((q) => q.path.endsWith('/css/site.css'));
      const png = own.find((q) => q.path.endsWith('/img/chart.png'));
      r.ui.subresources = { css: css?.status ?? null, png: png?.status ?? null };
      expect.soft(css?.status, 'css 200 under the view').toBe(200);
      expect.soft(png?.status, 'png 200 under the view').toBe(200);
      expect.soft(css?.path.startsWith(`${VIEW_PREFIX}<redacted>/`), 'css under the view').toBe(true);
      const remoteRequests = j
        .who('owner')
        .requests.filter((q) => q.path.includes(REMOTE_IMAGE_HOST)).length;
      const remoteAny = r2.frameRequests.filter((q) => q.path.includes(REMOTE_IMAGE_HOST)).length;
      r.ui.remoteRequests = remoteRequests + remoteAny;
      expect.soft(r.ui.remoteRequests, 'no request to the remote image host').toBe(0);
    },
    { requires: ['R2-02'] }
  );

  // ---- R2-04: relative link stays on the version -------------------------
  r2.step = 'R2-04';
  await j.row(
    'R2-04',
    async (r) => {
      const page = await j.pageOf('owner');
      const appUrl = page.url();
      const frame = await bundleFrame(page);
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
      expect.soft((r.ui.page2 as { sameCapability: boolean }).sameCapability, 'same capability').toBe(
        true
      );
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
    { requires: ['R2-02'] }
  );

  // ---- R2-05: isolation negatives (generic failure text only) ------------
  r2.step = 'R2-05';
  await j.row(
    'R2-05',
    (r) =>
      isolated(async () => {
        const page = await openArtifact('owner', artifactPage(bundleId()));
        const startIndex = r2.frameRequests.length;
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
        const bearing = sessionBearing(startIndex).concat(
          sessionBearing(0).filter((q) => q.who === 'owner' && q.step <= 'R2-04')
        );
        noteSessionBearing(bearing, r);
        const probeRec = { who: 'owner', step: 'R2-05', ...result, parent, nav, apiNavStatus };
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
        iso(result.frame === 'framed');
        iso(result.origin === 'null');
        iso(String(result.parentDom).startsWith('blocked:'));
        iso(String(result.parentWrite).startsWith('blocked:'));
        iso(parent.contentDocumentNull);
        iso(parent.parentFlag === null);
        // (b)
        iso(String(result.cookie).startsWith('blocked:'));
        iso(String(result.storage).startsWith('blocked:'));
        iso(String(result.fetch).startsWith('blocked:'));
        iso(bearing.length === 0);
        if (apiNavStatus === 200) r2.sessionBearingFrameRequest = true;
        iso(apiNavStatus !== 200);
        if (apiNavStatus !== 200 && apiNavStatus !== 401) {
          j.finding('R2-05', `frame API navigation answered ${apiNavStatus}`, ISOLATION_FAIL);
        }
        iso(apiNavStatus === 401);
        // (c)
        iso(String(result.popup).startsWith('blocked:'));
        iso(nav.pageUrlUnchanged);
        iso(nav.probePathRequests === 0);
        iso(nav.newPages === 0);
      }),
    { requires: ['R2-02'] }
  );

  // ---- R2-06: Open in new tab stays sandboxed ----------------------------
  r2.step = 'R2-06';
  await j.row(
    'R2-06',
    (r) =>
      isolated(async () => {
        const page = await openArtifact('owner', artifactPage(bundleId()));
        await page.locator(RENDER_SELECTORS.htmlFrame).waitFor({ state: 'attached' });
        const frameSrc = (await htmlFrameAttributes(page)).src ?? '';
        const startIndex = r2.frameRequests.length;
        const [popup] = await Promise.all([
          j.who('owner').context.waitForEvent('page'),
          previewBarButton(page, 'Open in new tab').click(),
        ]);
        await popup.waitForLoadState('load');
        const samePath = new URL(popup.url()).pathname === new URL(frameSrc, popup.url()).pathname;
        await expect(popup.locator('#ver')).toHaveText('R2 bundle v1');
        const result = await readProbe(popup);
        await settle(popup);
        const mine = r2.frameRequests.slice(startIndex).filter((q) => q.who === 'owner');
        const doc = mine.find((q) => q.topLevelDocument);
        const sub = mine.filter((q) => !q.topLevelDocument);
        const bearing = sub.filter((q) => q.cookieHeader.present);
        noteSessionBearing(bearing, r);
        const headers = await viewHeaders('owner', frameSrc, r);
        await popup.close();
        const rec = {
          who: 'owner',
          step: 'R2-06',
          ...result,
          samePath,
          topLevelDocument: {
            status: doc?.status ?? null,
            cookieHeaderPresent: doc?.cookieHeader.present ?? null,
          },
          subresources: sub.map((q) => ({ path: q.path, status: q.status ?? null })),
          csp: headers.csp,
        };
        r2.probes.push(rec);
        r.ui.popup = rec;
        iso(samePath);
        iso(result.frame === 'top-level');
        iso(result.origin === 'null');
        iso(String(result.cookie).startsWith('blocked:'));
        iso(String(result.storage).startsWith('blocked:'));
        iso(String(result.fetch).startsWith('blocked:'));
        iso(bearing.length === 0);
        iso(doc?.status === 200);
        iso(String(headers.csp).includes('sandbox allow-scripts'));
      }),
    { requires: ['R2-02'] }
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
      const pub = await publishThroughDialog(page, 'owner', r2.dirs.markdown!, {
        title,
        note: 'r2 md',
        createPath: /^\/api\/v1\/artifacts$/,
      });
      r.ui.entryShown = pub.entry;
      expect(pub.createStatus, 'markdown publish create').toBe(201);
      expect.soft(pub.entry, 'entry defaults to index.md').toBe('index.md');
      await expect(page).toHaveURL(
        new RegExp(`/projects/${j.state.projectId}/artifacts/[0-9a-f-]+$`)
      );
      r2.markdown = { id: new URL(page.url()).pathname.split('/').pop()!, title };
      const calls = publishCalls('owner', since, 1);
      r.api.push(...calls.map((c) => ({ as: 'owner', ...c, status: c.status ?? 0, code: null })));
      r2.publishes.push({ artifact: 'markdown', seq: 1, calls });
      const finalize = calls.find((c) => c.path.endsWith('/finalize'));
      expect.soft(finalize?.status ?? 0, 'finalize 2xx').toBeGreaterThanOrEqual(200);
      expect.soft(finalize?.status ?? 0, 'finalize 2xx').toBeLessThan(300);

      const md = await openMarkdown('owner');
      r.ui.entryBar = await textOf(md.locator(RENDER_SELECTORS.htmlBar));
      expect.soft(String(r.ui.entryBar), 'entry bar names index.md').toContain('index.md');
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
    },
    { requires: ['R2-01'] }
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
      const [popup] = await Promise.all([context.waitForEvent('page'), link.click()]);
      const response = await popup.waitForEvent('response').catch(() => null);
      await popup.waitForLoadState('load');
      const want = `/api/v1/artifacts/${markdownId()}/versions/1/files/notes.md`;
      r.ui.popupPath = new URL(popup.url()).pathname;
      const status =
        response?.status() ??
        j.who('owner').requests.find((q) => q.path === want)?.status ??
        null;
      r.ui.popupStatus = status;
      expect.soft(r.ui.popupPath, 'new tab opens the v1 notes.md').toBe(want);
      expect.soft(status, 'new tab answered 200').toBe(200);
      await popup.close();
      const facts = await markdownFacts(page);
      const flags = await parentFlags(page);
      r.ui.afterClick = { scripts: facts.scripts, parentFlag: facts.parentFlag, appFlag: flags.r2md };
      expect.soft(facts.scripts, 'no script elements').toBe(0);
      expect.soft(facts.parentFlag, 'no script ran').toBeNull();
      expect.soft(flags.r2md, 'no script ran (app page)').toBeNull();
      r.api.push({ as: 'owner', method: 'GET', path: want, status: status ?? 0, code: null });
    },
    { requires: ['R2-07'] }
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
    { requires: ['R2-07'] }
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
        const pub = await publishThroughDialog(page, 'owner', r2.dirs[`bundle-v${n}`]!, {
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
        await settle(page);
        const calls = publishCalls('owner', since, n);
        r.api.push(...calls.map((c) => ({ as: 'owner', ...c, status: c.status ?? 0, code: null })));
        const create = j
          .who('owner')
          .requests.slice(since)
          .find((q) => q.method === 'POST' && q.path === `/api/v1/artifacts/${bundleId()}/versions`);
        const finalize = calls.find((c) => c.path.endsWith('/finalize'));
        r2.publishes.push({
          artifact: 'bundle',
          seq: n,
          create: create?.status ?? null,
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
    { requires: ['R2-02'] }
  );

  // ---- R2-10: history in order with the publisher ------------------------
  r2.step = 'R2-10';
  const expectedHistory = (publisher: string) => [
    { seq: 'v3', current: true, kind: 'publish', publishedBy: publisher, note: 'r2 v3', files: '5', view: false },
    { seq: 'v2', current: false, kind: 'publish', publishedBy: publisher, note: 'r2 v2', files: '4', view: true },
    { seq: 'v1', current: false, kind: 'publish', publishedBy: publisher, note: 'r2 v1', files: '4', view: true },
  ];
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
      const versions = await j.api('owner', 'GET', `/api/v1/artifacts/${bundleId()}/versions`, undefined, r);
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
    { requires: ['R2-09'] }
  );

  // ---- R2-11: older versions show their own content ----------------------
  r2.step = 'R2-11';
  await j.row(
    'R2-11',
    async (r) => {
      const page = await openArtifact('owner', artifactPage(bundleId()));
      // v2 through the Version menu.
      await page.locator(SELECTORS.versionMenu).click();
      await versionMenuItem(page, 2).click();
      await expect(page).toHaveURL(new RegExp(`/artifacts/${bundleId()}/v/2$`));
      await expect(page.locator(SELECTORS.versionMenu)).toContainText('Version v2');
      await expect(htmlFrame(page).locator('#ver')).toHaveText('R2 bundle v2');
      const v2Label = await textOf(page.locator(SELECTORS.versionMenu));
      const v2Mint = await latestMint('owner', 2);
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
      const v1Mint = await latestMint('owner', 1);
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
      r.ui.v3 = { heading: 'R2 bundle v3', page2: 'R2 page two v3', files: v3Files.rows.map((x) => x.path) };
      expect.soft(v3Files.rows.map((x) => x.path), 'v3 lists notes/v3.txt').toContain('notes/v3.txt');
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
    { requires: ['R2-09'] }
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
    const dl = await downloadFile(page, 'img/chart.png');
    await settle(page);
    const want = `/api/v1/artifacts/${bundleId()}/versions/3/files/img/chart.png`;
    dl.requestStatus = [...j.who(key).requests].reverse().find((q) => q.path === want)?.status ?? null;
    r.ui.download = dl;
    expect.soft(dl.filename, 'download filename').toBe('chart.png');
    expect.soft(dl.bytes, 'download size').toBe(r2.fileSizes['img/chart.png']);
    const res = await j.api(key, 'GET', want, undefined, r);
    r.ui.fileGet = { status: res.status, contentLength: Number(res.headers['content-length'] ?? -1) };
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
      r.ui.indexHtml = { status: html.status, contentDisposition: html.headers['content-disposition'] ?? '' };
      expect.soft(html.status, 'index.html GET').toBe(200);
      expect
        .soft(html.headers['content-disposition'] ?? '', 'index.html served as attachment')
        .toMatch(/^attachment/);
    },
    { requires: ['R2-09'] }
  );

  // ---- R2-13: reader, bundle page and frame (generic failure text) -------
  r2.step = 'R2-13';
  await j.row(
    'R2-13',
    (r) =>
      isolated(async () => {
        const startIndex = r2.frameRequests.length;
        const page = await openArtifact('member', artifactPage(bundleId()));
        await expect(headerTitle(page)).toHaveText(r2.bundle!.title);
        await page.locator(RENDER_SELECTORS.htmlFrame).waitFor({ state: 'attached' });
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
        await expect(htmlFrame(page).locator('#page2')).toHaveText('R2 page two v3');
        await settle(page);
        const bearing = sessionBearing(startIndex).filter((q) => q.who === 'member');
        noteSessionBearing(bearing, r);
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
          owner,
          bar,
          frame: { sandboxTokens: tokens(attrs.sandbox), referrerpolicy: attrs.referrerpolicy, ...info },
          heading,
          chartNaturalWidth: chart,
          probe: result,
          canPublish: (caps.body as { canPublish?: boolean } | null)?.canPublish ?? null,
        };
        r2.probes.push({ ...result, who: 'member', step: 'R2-13' });
        r2.frames.push({ who: 'member', step: 'R2-13', kind: 'html', ...rec.frame });
        r.ui.reader = rec;
        const labels = ownerLabels();
        iso(owner !== 'Owner: You' && labels.some((l) => owner === `Owner: ${l}`));
        iso(labels.some((l) => bar === `Content published by ${l} · runs sandboxed, isolated from Scion`));
        iso(JSON.stringify(rec.frame.sandboxTokens) === JSON.stringify(['allow-scripts']));
        iso(attrs.referrerpolicy === 'no-referrer');
        iso(info.viewSeq === 3 && info.viewIdMatches && info.entry === 'index.html');
        iso(heading === 'R2 bundle v3');
        iso(chart === CHART_SIZE.width);
        iso(result.origin === 'null');
        iso(String(result.parentDom).startsWith('blocked:'));
        iso(String(result.cookie).startsWith('blocked:'));
        iso(String(result.fetch).startsWith('blocked:'));
        iso(bearing.length === 0);
        iso(caps.status === 200 && rec.canPublish === false);
        iso(mint.status === 200);
      }),
    { requires: ['R2-09'] }
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
      const versions = await j.api('member', 'GET', `/api/v1/artifacts/${bundleId()}/versions`, undefined, r);
      const body = (versions.body ?? {}) as { versions?: Array<{ seq: number; createdByRef?: string }> };
      const apiSeqs = (body.versions ?? []).map((v) => v.seq);
      const apiCreatedBy = (body.versions ?? []).map((v) => v.createdByRef ?? null);
      r2.history.member = { rows, apiSeqs, apiCreatedBy };
      expect.soft(versions.status, 'versions 200').toBe(200);
      expect.soft(apiSeqs, 'same seqs as the owner').toEqual([3, 2, 1]);
      expect.soft(apiCreatedBy, 'same publisher as the owner saw').toEqual(Array(3).fill(j.who('owner').id));
    },
    { requires: ['R2-09'] }
  );

  // ---- R2-15: reader Files and Download ----------------------------------
  r2.step = 'R2-15';
  await j.row('R2-15', (r) => filesCheck('member', r), { requires: ['R2-09'] });

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
    { requires: ['R2-07'] }
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
      const real = await j.api('outsider', 'POST', `/api/v1/artifacts/${bundleId()}/versions/1/view`, undefined, r);
      const random = await j.api(
        'outsider',
        'POST',
        `/api/v1/artifacts/${crypto.randomUUID()}/versions/1/view`,
        undefined,
        r
      );
      expect.soft(real.status, 'mint view for a non-reader').toBe(404);
      expect.soft([real.status, real.code], 'same as for a random id').toEqual([random.status, random.code]);
    },
    { requires: ['R2-01'] }
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
    { requires: ['R2-02'] }
  );

  // ---- R2-20: markdown in the chat preview -------------------------------
  r2.step = 'R2-20';
  await runChatRow();

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
  await j.row('R2-20', (r) => chatRow(r), { requires: ['R2-07'] });
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
  const link = page.locator(`${RENDER_SELECTORS.chatArtifactLink}[data-artifact-id="${markdownId()}"]`);
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
  expect.soft((r.ui.openInViewer as { visible: boolean }).visible, 'Open in artifact viewer').toBe(true);
  expect
    .soft((r.ui.openInViewer as { href: string | null }).href, 'viewer link')
    .toBe(`/projects/${projectId}/artifacts/${markdownId()}`);
  const art = await j.api('member', 'GET', `/api/v1/artifacts/${markdownId()}`, undefined, r);
  expect.soft(art.status, 'member reads the referenced artifact').toBe(200);
}

async function runCleanup(testInfo: {
  annotations: ReadonlyArray<{ type: string; description?: string }>;
  project: { outputDir: string };
}): Promise<void> {
  const residue: string[] = [];
  const cleanup: Record<string, unknown> = {};
  const states: Record<string, string> = {};
  r2.step = 'cleanup';

  for (const identity of Object.values(j.state.ids) as Array<Identity<Key> | undefined>) {
    for (const page of identity?.context.pages() ?? []) await page.close();
  }

  // Both artifacts, as the owner (soft delete).
  const artifactDeletes: Record<string, number | string> = {};
  for (const [name, a] of [
    ['bundle', r2.bundle],
    ['markdown', r2.markdown],
  ] as const) {
    if (!a || !j.state.ids.owner) continue;
    try {
      const del = await j.api('owner', 'DELETE', `/api/v1/artifacts/${a.id}`);
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
  const chatCleanup = await cleanupChat(residue, states);
  if (chatCleanup) cleanup.chat = chatCleanup;

  // Final reads while the sessions still exist.
  try {
    for (const [name, a] of [
      ['bundle', r2.bundle],
      ['markdown', r2.markdown],
    ] as const) {
      if (!a || !j.state.ids.owner) continue;
      const own = await j.api('owner', 'GET', `/api/v1/artifacts/${a.id}`);
      states[`${name} (owner read)`] = classifyRead(own.status, own.body, 'artifact');
      const list = await j.api('owner', 'GET', '/api/v1/artifacts?mine=1');
      const listed = ((list.body ?? {}) as { artifacts?: Array<{ id: string }> }).artifacts?.some(
        (x) => x.id === a.id
      );
      states[`${name} (owner list)`] = list.status === 200 && !listed ? 'absent' : 'present_in_list';
      if (j.state.ids.member) {
        const m = await j.api('member', 'GET', `/api/v1/artifacts/${a.id}`);
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
  ev.sessionBearingFrameRequest = r2.sessionBearingFrameRequest;
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
  const body = JSON.stringify(ev, null, 2);
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

/** Deletes the chat thread, if one was created, and checks it is gone. */
async function cleanupChat(
  residue: string[],
  states: Record<string, string>
): Promise<Record<string, unknown> | null> {
  const topicId = r2.chat.topicId as string | undefined;
  if (!topicId || !j.state.ids.owner) return null;
  const out: Record<string, unknown> = {};
  try {
    const del = await j.api('owner', 'DELETE', `/api/v1/chat/topics/${encodeURIComponent(topicId)}`);
    out.threadDelete = del.status;
    if (del.status >= 300 && del.status !== 404) residue.push(`chat thread: DELETE ${del.status}`);
    const get = await j.api('owner', 'GET', `/api/v1/chat/topics/${encodeURIComponent(topicId)}`);
    out.threadReadAfter = get.status;
    const deletedAt = (get.body as { deletedAt?: string } | null)?.deletedAt;
    states['chat thread'] = get.status === 404 || !!deletedAt ? 'absent' : `status ${get.status}`;
  } catch (err) {
    residue.push(`cleanup error (chat thread): ${firstLine(err)}`);
  }
  return out;
}
