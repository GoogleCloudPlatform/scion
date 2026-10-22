/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Artifact API types and helpers (experiment hub.artifacts).
 *
 * Mirrors the hub's /api/v1/artifacts responses (pkg/artifacts/api.go).
 */

import { apiFetch, extractApiError } from './api.js';

/** Feature flag that gates every artifact surface in the web UI. */
export const ARTIFACTS_FLAG = 'hub.artifacts';

export interface ArtifactFile {
  path: string;
  size: number;
  sha256: string;
  mediaType: string;
  /** "remote" for an image the hub fetched when the version was published. */
  origin?: string;
  /** For a remote image: the URL the entry referenced. */
  sourceUrl?: string;
  /** For a remote image: "ok" or "failed". */
  fetchStatus?: string;
}

export interface ArtifactVersion {
  seq: number;
  ref: string;
  kind: string;
  entryPath: string;
  note?: string;
  totalBytes: number;
  fileCount: number;
  createdByKind?: string;
  createdByRef?: string;
  createdAt: string;
  state: string;
  files: ArtifactFile[];
}

export interface Artifact {
  id: string;
  ref: string;
  scopeKind: string;
  scopeRef: string;
  ownerKind: string;
  ownerRef: string;
  key?: string;
  title: string;
  currentSeq: number;
  /** When the artifact expires and is deleted; absent when kept until deleted. */
  expiresAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface ArtifactResponse {
  artifact: Artifact;
  version?: ArtifactVersion;
  warnings?: string[];
  /** On a GET: the caller may share and change the artifact. */
  canManage?: boolean;
  /** On a GET: the caller may publish new versions of any kind (edit, upload, review). */
  canPublish?: boolean;
  /**
   * On a GET: the caller may publish reviews only, through a review grant
   * (ptone/scion#4538). Never set with canPublish.
   */
  canReview?: boolean;
  /**
   * On a GET: the caller may not manage the artifact but may give a user
   * review access (a review grant): the delegating user of the agent that
   * owns it, the project owners and admins of its home project, and
   * super-admins. Never set with canManage.
   */
  canGrantReview?: boolean;
  /**
   * On a GET: the artifact's home project was deleted. Advisory: absent when
   * the hub cannot tell.
   */
  scopeDeleted?: boolean;
}

/** GET /api/v1/artifacts/{id}/versions: ready versions, newest first, without files. */
export interface VersionListResponse {
  versions: ArtifactVersion[];
  /**
   * Versions of the same page that stopped being pending without becoming
   * ready (ptone/scion#4577): only their number, state, reason code and the
   * time their number was taken.
   */
  discarded?: DiscardedVersion[];
  nextBefore?: number;
}

/** A discarded version as GET .../versions lists it. */
export interface DiscardedVersion {
  seq: number;
  state: string;
  /** unmarked_changes, stale_review, newer_publish_pending, expired or discarded. */
  reason: string;
  /** When the version number was taken. */
  createdAt: string;
}

/** Describes a discarded version's reason code for people. */
export function discardReasonText(reason: string): string {
  switch (reason) {
    case 'unmarked_changes':
      return 'the review changed text outside its marks';
    case 'stale_review':
      return 'the review was of an older version';
    case 'newer_publish_pending':
      return 'a newer version was being published';
    case 'expired':
      return 'the upload was not finished in time';
    default:
      return 'no reason recorded';
  }
}

/** POST /api/v1/artifacts/{id}/versions/{seq}/view. */
export interface ViewResponse {
  url: string;
  expiresAt: string;
  remoteImages?: boolean;
}

/** One file of a publish manifest. */
export interface ManifestFile {
  path: string;
  size: number;
  sha256: string;
}

/** Body of POST /api/v1/artifacts and POST /api/v1/artifacts/{id}/versions. */
export interface CreateVersionRequest {
  title?: string;
  key?: string;
  scope?: string;
  kind?: 'publish' | 'review';
  entry: string;
  note?: string;
  files: ManifestFile[];
}

/** Answer to a request that created a pending version. */
export interface PendingVersionResponse {
  artifact: Artifact;
  version: ArtifactVersion;
  upload: { required: string[] };
}

/** Remote image rows the hub adds to a version; not part of what was published. */
export function isRemoteFile(f: ArtifactFile): boolean {
  return f.origin === 'remote';
}

/**
 * Parses an artifact page path: /projects/{p}/artifacts/{id}[/v/{seq}].
 * Returns null for anything else.
 */
export function parseArtifactPagePath(
  path: string
): { projectId: string; id: string; seq: number } | null {
  const m = path.match(
    /^\/projects\/([^/]+)\/artifacts\/([^/?#]+)(?:\/v\/([1-9][0-9]{0,8}))?\/?(?:[?#].*)?$/
  );
  if (!m) return null;
  try {
    return {
      projectId: decodeURIComponent(m[1]),
      id: decodeURIComponent(m[2]),
      seq: m[3] ? Number(m[3]) : 0,
    };
  } catch {
    return null;
  }
}

/** Hex SHA-256 of a blob, as the hub's manifest expects. */
export async function sha256Hex(data: Blob): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', await data.arrayBuffer());
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('');
}

/** How the artifact page renders an entry file. */
export type ArtifactRenderer = 'markdown' | 'text' | 'image' | 'html' | 'download';

/** Text media types that are not text/*. */
const TEXT_APPLICATION_TYPES = new Set([
  'application/json',
  'application/yaml',
  'application/toml',
  'application/xml',
]);

/**
 * Raster image types rendered with <img>. SVG is excluded: the hub serves
 * it as an attachment.
 */
const IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);

/** Picks the renderer for a file's media type. */
export function rendererFor(mediaType: string): ArtifactRenderer {
  const mt = mediaType.toLowerCase();
  if (mt === 'text/markdown') return 'markdown';
  if (IMAGE_TYPES.has(mt)) return 'image';
  if (mt === 'text/html') return 'html';
  if (mt.startsWith('text/') || TEXT_APPLICATION_TYPES.has(mt)) return 'text';
  return 'download';
}

/**
 * URL of a file of an artifact. seq 0 means the current version. stream
 * asks the hub to serve the bytes itself instead of redirecting to object
 * storage, which fetch() needs because the redirect is cross-origin.
 */
/** Percent-encodes each segment of a slash-separated file path. */
function encodePath(path: string): string {
  return path
    .split('/')
    .map((seg) => encodeURIComponent(seg))
    .join('/');
}

export function artifactFileUrl(id: string, seq: number, path: string, stream = false): string {
  const encodedPath = encodePath(path);
  const version = seq > 0 ? `/versions/${seq}` : '';
  const query = stream ? '?stream=1' : '';
  return `/api/v1/artifacts/${encodeURIComponent(id)}${version}/files/${encodedPath}${query}`;
}

/**
 * Media types the hub serves with Content-Disposition: inline (mirrors
 * inlineSafe in pkg/artifacts/media.go). The browser displays these rather
 * than downloading them, so the page offers "Open raw" for them and
 * "Download" for everything else.
 */
const INLINE_TYPES = new Set([
  'text/plain',
  'text/markdown',
  'text/csv',
  'text/tab-separated-values',
  'application/json',
  'application/yaml',
  'application/toml',
  'image/png',
  'image/jpeg',
  'image/gif',
  'image/webp',
]);

/** Reports whether the hub serves a media type inline. */
export function isInlineType(mediaType: string): boolean {
  return INLINE_TYPES.has(mediaType.toLowerCase());
}

/** The last segment of a file path, for download file names. */
export function baseName(path: string): string {
  const i = path.lastIndexOf('/');
  return i >= 0 ? path.slice(i + 1) : path;
}

/** Formats a byte count for display. */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}

/** Largest text file the page renders inline. */
export const MAX_INLINE_TEXT_BYTES = 4 * 1024 * 1024;

// ────────────────────────────────────────────────────────────
// API calls
// ────────────────────────────────────────────────────────────

async function okJSON<T>(res: Response): Promise<T> {
  if (!res.ok) {
    throw new Error(await extractApiError(res, `HTTP ${res.status}`));
  }
  return (await res.json()) as T;
}

function artifactPath(id: string): string {
  return `/api/v1/artifacts/${encodeURIComponent(id)}`;
}

/** One page of the artifacts homed in a project that the caller can read. */
export async function listProjectArtifacts(
  projectId: string,
  opts: {
    q?: string;
    cursor?: string;
    limit?: number;
    signal?: AbortSignal;
    /** Only artifacts homed elsewhere and shared with the project. */
    sharedOnly?: boolean;
  } = {}
): Promise<ArtifactListResponse> {
  const params = new URLSearchParams({ mine: '1', scope: projectId });
  if (opts.q) params.set('q', opts.q);
  if (opts.sharedOnly) params.set('shared', '1');
  if (opts.cursor) params.set('cursor', opts.cursor);
  if (opts.limit) params.set('limit', String(opts.limit));
  return okJSON(
    await apiFetch(`/api/v1/artifacts?${params.toString()}`, { signal: opts.signal ?? null })
  );
}

// ---------------------------------------------------------------------------
// Where the caller may publish (the hub Artifacts page's New artifact,
// ptone/scion#5477)
// ---------------------------------------------------------------------------

/** The hub's global project's slug (pkg/hub globalProjectSlug). */
export const GLOBAL_PROJECT_SLUG = 'global';

/** A project offered as the home of a new artifact. */
export interface ProjectChoice {
  id: string;
  name: string;
  slug: string;
}

/** Projects requested per page when listing the projects the caller can see. */
const READABLE_PROJECTS_PAGE = 200;
/** Pages followed at most: the list stops at this many projects. */
const READABLE_PROJECTS_MAX_PAGES = 10;
/** The most projects listReadableProjects returns. */
export const READABLE_PROJECTS_CAP = READABLE_PROJECTS_PAGE * READABLE_PROJECTS_MAX_PAGES;
/** canCreate checks in flight at once. */
export const PUBLISH_CHECK_CONCURRENCY = 6;

/** The projects the caller can see, and whether the list stopped at the cap. */
export interface ReadableProjects {
  projects: ProjectChoice[];
  truncated: boolean;
}

interface ProjectsPage {
  projects?: Array<{ id: string; name?: string; slug?: string }>;
  nextCursor?: string;
}

function toChoices(page: ProjectsPage): ProjectChoice[] {
  return (page.projects ?? [])
    .filter((p) => !!p.id)
    .map((p) => ({ id: p.id, name: p.name || p.slug || p.id, slug: p.slug ?? '' }));
}

/**
 * The hub's global project, when it exists and the caller can see it: one
 * exact lookup by slug (GET /api/v1/projects?slug=global), so it never
 * depends on paging. The project list is newest first, and global, made
 * when the hub starts, is among the oldest.
 */
export async function findGlobalProject(signal?: AbortSignal): Promise<ProjectChoice | null> {
  const params = new URLSearchParams({ slug: GLOBAL_PROJECT_SLUG, limit: '1' });
  const page = await okJSON<ProjectsPage>(
    await apiFetch(`/api/v1/projects?${params.toString()}`, { signal: signal ?? null })
  );
  return toChoices(page).find((p) => p.slug === GLOBAL_PROJECT_SLUG) ?? null;
}

/**
 * The projects the caller can see (GET /api/v1/projects, paged), by name,
 * leaving out the project with id skip. Not mine=true: on the hub that
 * means projects the caller owns, and a member may publish too
 * (agent-create.ts has the same note). canCreate decides; over-listing only
 * costs checks. The list stops at READABLE_PROJECTS_CAP projects and says
 * so (truncated).
 */
export async function listReadableProjects(
  opts: { skip?: string | undefined; signal?: AbortSignal | undefined } = {}
): Promise<ReadableProjects> {
  const out: ProjectChoice[] = [];
  let cursor = '';
  for (let page = 0; page < READABLE_PROJECTS_MAX_PAGES; page++) {
    const params = new URLSearchParams({ limit: String(READABLE_PROJECTS_PAGE) });
    if (cursor) params.set('cursor', cursor);
    const body = await okJSON<ProjectsPage>(
      await apiFetch(`/api/v1/projects?${params.toString()}`, { signal: opts.signal ?? null })
    );
    out.push(...toChoices(body).filter((p) => p.id !== opts.skip));
    cursor = body.nextCursor ?? '';
    if (!cursor) break;
  }
  return { projects: sortProjectChoices(out), truncated: cursor !== '' };
}

/**
 * The projects to offer as a new artifact's home, before the canCreate
 * filter: the global project first (looked up on its own, so the cap never
 * drops it), then the other projects the caller can see, by name.
 */
export async function listPublishCandidates(signal?: AbortSignal): Promise<ReadableProjects> {
  const global = await findGlobalProject(signal);
  const rest = await listReadableProjects({ skip: global?.id, signal });
  return {
    projects: global ? [global, ...rest.projects] : rest.projects,
    truncated: rest.truncated,
  };
}

/** Orders choices global first, then by name. */
export function sortProjectChoices(projects: ProjectChoice[]): ProjectChoice[] {
  return [...projects].sort((a, b) => {
    const ga = a.slug === GLOBAL_PROJECT_SLUG;
    const gb = b.slug === GLOBAL_PROJECT_SLUG;
    if (ga !== gb) return ga ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
}

/**
 * Whether the caller may publish artifacts in project: the list's
 * canCreate, which is the decision publish makes (pkg/artifacts canCreateIn).
 * A list the caller may not read (403, 404) answers false; other failures
 * throw. (A hub with no artifact routes at all also answers 404, but the
 * callers only run with hub.artifacts on.)
 */
export async function canPublishIn(projectId: string, signal?: AbortSignal): Promise<boolean> {
  const params = new URLSearchParams({ mine: '1', scope: projectId, limit: '1' });
  const res = await apiFetch(`/api/v1/artifacts?${params.toString()}`, {
    suppressAccessDeniedToast: true,
    signal: signal ?? null,
  });
  if (res.status === 403 || res.status === 404) return false;
  const body = await okJSON<ArtifactListResponse>(res);
  return body.canCreate === true;
}

/**
 * Checks projects in order, PUBLISH_CHECK_CONCURRENCY at a time, and
 * returns the publishable ones in the order given. onUpdate gets that list
 * so far (in order, only checked projects) each time a check passes, so a
 * caller can render it progressively. With firstOnly no new check starts
 * after the first pass, and the result holds that one project. Any failed
 * check fails the whole call, so a hub error never looks like "nowhere to
 * publish"; after a failure, or once signal aborts, no check starts and no
 * update is reported. signal also cancels the checks in flight.
 */
export async function filterPublishable(
  projects: ProjectChoice[],
  opts: {
    firstOnly?: boolean;
    check?: (id: string, signal?: AbortSignal) => Promise<boolean>;
    onUpdate?: (publishable: ProjectChoice[]) => void;
    signal?: AbortSignal;
  } = {}
): Promise<ProjectChoice[]> {
  const check = opts.check ?? canPublishIn;
  const signal = opts.signal;
  const allowed: boolean[] = projects.map(() => false);
  const passed = (): ProjectChoice[] => projects.filter((_, i) => allowed[i]);
  let next = 0;
  let stop = false;
  const halted = (): boolean => stop || signal?.aborted === true;
  const worker = async (): Promise<void> => {
    while (!halted() && next < projects.length) {
      const i = next++;
      let ok: boolean;
      try {
        ok = await check(projects[i].id, signal);
      } catch (err) {
        stop = true;
        throw err;
      }
      if (!ok || halted()) continue;
      allowed[i] = true;
      if (opts.firstOnly) stop = true;
      opts.onUpdate?.(passed());
    }
  };
  const workers = Math.min(PUBLISH_CHECK_CONCURRENCY, projects.length);
  await Promise.all(Array.from({ length: workers }, () => worker()));
  if (signal?.aborted) throw new DOMException('Aborted', 'AbortError');
  const out = passed();
  return opts.firstOnly ? out.slice(0, 1) : out;
}

/**
 * The project preselected for a new artifact: the global project when it
 * is publishable (canCreate holds only for a member or owner), or the only
 * choice when there is exactly one; otherwise none.
 */
export function defaultProjectChoice(publishable: ProjectChoice[]): string {
  const global = publishable.find((p) => p.slug === GLOBAL_PROJECT_SLUG);
  if (global) return global.id;
  return publishable.length === 1 ? publishable[0].id : '';
}

/** One page of an artifact's ready versions, newest first. */
export async function listVersions(id: string, before = 0): Promise<VersionListResponse> {
  const query = before > 0 ? `?before=${before}` : '';
  return okJSON(await apiFetch(`${artifactPath(id)}/versions${query}`));
}

/** Mints a short-lived view of an HTML bundle version. */
export async function mintView(id: string, seq: number): Promise<ViewResponse> {
  return okJSON(await apiFetch(`${artifactPath(id)}/versions/${seq}/view`, { method: 'POST' }));
}

/**
 * A file to publish: its path in the bundle and either its bytes, or (for
 * a file kept unchanged from the current version) its size and digest.
 */
export type PublishFile =
  | { path: string; data: Blob }
  | { path: string; size: number; sha256: string };

export interface PublishRequest {
  /** Set to add a version to this artifact; unset to create an artifact. */
  artifactId?: string | undefined;
  /** Home project of a new artifact. */
  scope?: string | undefined;
  title?: string | undefined;
  key?: string | undefined;
  note?: string | undefined;
  /** Version kind; unset publishes. A review needs artifactId and base. */
  kind?: 'publish' | 'review' | undefined;
  /**
   * For a review, the version it was made against. The hub checks the
   * review against that version only and refuses it (409 stale_review)
   * unless it is still the current version.
   */
  base?: number | undefined;
  entry: string;
  files: PublishFile[];
  /** Called after each upload with the number of files uploaded so far. */
  onProgress?: ((done: number, total: number, path: string) => void) | undefined;
  /**
   * A version a failed attempt left pending. It is resumed when this
   * request publishes exactly the same thing; otherwise a new version is
   * created.
   */
  resume?: PendingPublish | null | undefined;
}

/** A pending version that a failed publish left behind. */
export interface PendingPublish {
  artifactId: string;
  seq: number;
  /** Files still to upload. */
  required: string[];
  /** Every file the hub asked for when the version was created. */
  all: string[];
  /** Identifies what was being published (manifest and metadata). */
  fingerprint: string;
}

/**
 * A publish that failed after its version was created. pending names that
 * version, so the next attempt with the same content resumes it instead
 * of leaving another pending version behind.
 */
export class PublishError extends Error {
  constructor(
    message: string,
    readonly pending: PendingPublish | null,
    /** The hub's error code, when it answered with one. */
    readonly code = ''
  ) {
    super(message);
    this.name = 'PublishError';
  }
}

/** How long the hub keeps a version that was never finalized. */
export const PENDING_VERSION_LIFETIME = '24 hours';

/**
 * Publishes files as a new artifact or a new version of one, through the
 * two-step API: create the pending version with the manifest, upload the
 * files the hub asks for, then finalize.
 */
export async function publishFiles(req: PublishRequest): Promise<ArtifactResponse> {
  const manifest: ManifestFile[] = [];
  const byPath = new Map<string, Blob>();
  for (const f of req.files) {
    if ('data' in f) {
      manifest.push({ path: f.path, size: f.data.size, sha256: await sha256Hex(f.data) });
      byPath.set(f.path, f.data);
    } else {
      manifest.push({ path: f.path, size: f.size, sha256: f.sha256 });
    }
  }
  const body: CreateVersionRequest = { entry: req.entry, files: manifest };
  if (req.title) body.title = req.title;
  if (req.key) body.key = req.key;
  if (req.note) body.note = req.note;
  if (req.kind === 'review') body.kind = 'review';
  if (!req.artifactId && req.scope) body.scope = req.scope;
  const fingerprint = JSON.stringify([req.artifactId ?? '', body]);

  let pending: PendingPublish;
  if (req.resume && req.resume.fingerprint === fingerprint) {
    pending = req.resume;
  } else {
    const createPath = req.artifactId
      ? `${artifactPath(req.artifactId)}/versions`
      : '/api/v1/artifacts';
    const created = await okJSON<PendingVersionResponse>(
      await apiFetch(createPath, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
    );
    const asked = created.upload.required ?? [];
    pending = {
      artifactId: created.artifact.id,
      seq: created.version.seq,
      required: asked,
      all: asked,
      fingerprint,
    };
  }
  const { artifactId: id, seq, required } = pending;
  // Files still to upload. Each successful upload is taken off, so a
  // failure reports, and the next attempt uploads, only what is left.
  let remaining = [...required];
  const left = (): PendingPublish => ({ ...pending, required: remaining });
  const fail = async (res: Response, prefix: string): Promise<never> => {
    let body: {
      error?: { code?: string; details?: { missing?: unknown; missingCount?: unknown } };
    } = {};
    try {
      body = (await res.clone().json()) as typeof body;
    } catch {
      // Not JSON; the status decides.
    }
    const code = body.error?.code ?? '';
    // A review the hub refused is discarded: say why in this page's terms.
    if (code === 'unmarked_changes') {
      throw new PublishError(reviewRejectedMessage(body), null, code);
    }
    if (code === 'stale_review') {
      throw new PublishError(
        'Your review was not saved: a newer version was published, or is being published. The review now starts from the current version; your discarded text is kept read-only to copy from.',
        null,
        code
      );
    }
    const message = prefix + (await extractApiError(res, `HTTP ${res.status}`));
    // The version is gone (404), no longer pending (409 conflict), or not
    // the caller's (403): another attempt must start a new version.
    const gone =
      res.status === 404 || res.status === 403 || (res.status === 409 && code === 'conflict');
    if (gone) throw new PublishError(message, null, code);
    // Finalize found files missing: upload those next time. The hub lists
    // at most a few; when it lists fewer than are missing, upload them all.
    const missing = body.error?.details?.missing;
    if (code === 'incomplete') {
      const all =
        Array.isArray(missing) && missing.length === body.error?.details?.missingCount
          ? (missing as unknown[]).filter((p): p is string => typeof p === 'string')
          : pending.all;
      remaining = all.filter((p) => byPath.has(p));
    }
    throw new PublishError(message, left(), code);
  };
  let done = 0;
  for (const path of required) {
    const data = byPath.get(path);
    if (!data) {
      throw new PublishError(`The hub asked for ${path}, which is not being uploaded.`, null);
    }
    const m = manifest.find((x) => x.path === path)!;
    let res: Response;
    try {
      res = await apiFetch(`${artifactPath(id)}/versions/${seq}/files/${encodePath(path)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/octet-stream', 'X-Content-SHA256': m.sha256 },
        body: data,
      });
    } catch (err) {
      throw new PublishError(
        `${path}: ${err instanceof Error ? err.message : 'upload failed'}`,
        left()
      );
    }
    if (!res.ok) await fail(res, `${path}: `);
    remaining = remaining.filter((p) => p !== path);
    done++;
    req.onProgress?.(done, required.length, path);
  }
  let res: Response;
  try {
    res = await apiFetch(
      `${artifactPath(id)}/versions/${seq}/finalize`,
      req.kind === 'review'
        ? {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ base: req.base }),
          }
        : { method: 'POST' }
    );
  } catch (err) {
    throw new PublishError(err instanceof Error ? err.message : 'finalize failed', left());
  }
  if (!res.ok) await fail(res, '');
  return (await res.json()) as ArtifactResponse;
}

/** A file of a review the hub refused (code unmarked_changes). */
interface UnmarkedFile {
  path?: string;
  change?: string;
  hunks?: { line?: number }[];
}

/** Explains an unmarked_changes refusal: where the review changed text. */
export function reviewRejectedMessage(body: { error?: { details?: unknown } }): string {
  const details = (body.error?.details ?? {}) as { files?: unknown };
  const files = Array.isArray(details.files) ? (details.files as UnmarkedFile[]) : [];
  const where = files
    .slice(0, 3)
    .map((f) => {
      const lines = (f.hunks ?? [])
        .map((h) => h.line)
        .filter((n): n is number => typeof n === 'number');
      if (f.change === 'modified' && lines.length > 0) {
        return `${f.path ?? ''} (line ${lines.slice(0, 5).join(', ')})`;
      }
      return `${f.path ?? ''} (${f.change ?? 'changed'})`;
    })
    .join('; ');
  return (
    'Your review changes text outside marks' +
    (where ? `: ${where}` : '') +
    '. A review may only add marks: mark every change with the toolbar. The review was not saved.'
  );
}

/** The message to show for a failed publish. */
export function publishErrorMessage(err: unknown): string {
  const message = (err instanceof Error ? err.message : 'Publishing failed').replace(/\.+$/, '');
  if (err instanceof PublishError && err.pending) {
    return `${message}. Publish again to retry; the upload continues where it stopped. If you do not, the unfinished version is removed after ${PENDING_VERSION_LIFETIME}.`;
  }
  return message;
}

/** One row of GET /api/v1/artifacts?mine=1 (pkg/artifacts/list.go). */
export interface ArtifactListItem extends Artifact {
  /** The current version is a review awaiting the owner. */
  reviewPending: boolean;
  /** Why the caller sees the artifact. */
  access?: ArtifactAccess;
  /** In a list narrowed to a project: homed elsewhere and shared with it. */
  sharedWithScope?: boolean;
  /** The artifact's home project was deleted. */
  scopeDeleted?: boolean;
  /** Set on rows with scopeDeleted: the caller may move the artifact. */
  canManage?: boolean;
}

/** Why the caller sees an artifact in the list. */
export type ArtifactAccess = 'owned' | 'project' | 'shared';

/** Body of GET /api/v1/artifacts?mine=1. */
export interface ArtifactListResponse {
  artifacts: ArtifactListItem[];
  /** Opaque cursor of the next page; absent on the last page. */
  nextCursor?: string;
  /**
   * On a list narrowed to a project (scope=): the caller may publish
   * artifacts there. Absent otherwise.
   */
  canCreate?: boolean;
}

/** Filters of the artifact list. */
export interface ArtifactListFilters {
  /** Title or key contains this text. */
  q?: string;
  /** Only artifacts whose current version is a review. */
  reviewPending?: boolean;
  /** Only artifacts the caller owns. */
  ownedOnly?: boolean;
  /** Only artifacts a grant shares with the caller. */
  sharedOnly?: boolean;
  /**
   * Only artifacts homed in this project, plus those shared with it when
   * the caller may read there (the list endpoint's scope filter).
   */
  scope?: string;
}

/**
 * URL of one page of the caller's artifact list. Empty filters are left
 * out, so a cursor (bound by the hub to the exact filters) stays valid.
 */
export function artifactListUrl(filters: ArtifactListFilters, cursor?: string): string {
  const params = new URLSearchParams({ mine: '1' });
  const q = filters.q?.trim();
  if (q) params.set('q', q);
  if (filters.reviewPending) params.set('review_pending', '1');
  if (filters.ownedOnly) params.set('owner', 'me');
  if (filters.sharedOnly) params.set('shared', '1');
  if (filters.scope) params.set('scope', filters.scope);
  if (cursor) params.set('cursor', cursor);
  return `/api/v1/artifacts?${params.toString()}`;
}

/**
 * Path of an artifact's page in the web UI; seq above 0 names one of its
 * versions, 0 the current one.
 */
export function artifactPagePath(a: Pick<Artifact, 'id' | 'scopeRef'>, seq = 0): string {
  const base = `/projects/${encodeURIComponent(a.scopeRef)}/artifacts/${encodeURIComponent(a.id)}`;
  return seq > 0 ? `${base}/v/${seq}` : base;
}

// ---------------------------------------------------------------------------
// Sharing: share links, grants, expiry and moving (pkg/artifacts/links.go,
// pkg/artifacts/grants.go)
// ---------------------------------------------------------------------------

/** A share link. The hub never returns its token after creation. */
export interface ShareLink {
  id: string;
  createdAt: string;
  expiresAt: string;
  /** "user:<id>" of the user who created it. */
  createdBy?: string;
}

/** POST /api/v1/artifacts/{id}/links. */
export interface CreateLinkResponse {
  link: ShareLink;
  /** Hub-relative path of the link; shown once, never retrievable later. */
  url: string;
  /** The link ends earlier than asked because the artifact expires then. */
  clampedToArtifactExpiry?: boolean;
}

/**
 * Grant permissions. Review (a user only) lets someone who can already read
 * the artifact publish reviews of it, and nothing else; it gives no read
 * access (ptone/scion#4538).
 */
export type GrantPermission = 'read' | 'review' | 'write' | 'admin';

/** A grant to a user or agent ("principal") or a project ("scope"). */
export interface ArtifactGrant {
  id: string;
  subjectKind: 'principal' | 'scope';
  /** "user:<id>" or "agent:<id>" for a principal, a project id for a scope. */
  subjectRef: string;
  permission: GrantPermission;
  /** The home project's grant: listed first, never removed. */
  home?: boolean;
  createdAt: string;
  createdBy?: string;
}

/** GET /api/v1/artifacts/{id}/grants. */
export interface GrantListResponse {
  grants: ArtifactGrant[];
  /** Grants to other projects, and moves to them, are turned on. */
  crossProjectSharing: boolean;
}

/** PATCH /api/v1/artifacts/{id}. */
export interface PatchArtifactResponse {
  artifact: Artifact;
  linksCutShort?: number;
  grantsRemoved?: number;
}

/** A share link lifetime the Share dialog offers. */
export interface LinkLifetime {
  label: string;
  hours: number;
}

/**
 * The preset share link lifetimes. The Share dialog offers those up to the
 * hub's maximum (see linkLifetimeChoices).
 */
export const LINK_LIFETIMES: ReadonlyArray<LinkLifetime> = [
  { label: '1 day', hours: 24 },
  { label: '7 days', hours: 7 * 24 },
  { label: '30 days', hours: 30 * 24 },
];

/** The share link lifetime preselected when the hub does not say, in hours. */
export const DEFAULT_LINK_HOURS = 7 * 24;

/** A label for a link lifetime: whole days in days, otherwise hours. */
export function linkLifetimeLabel(hours: number): string {
  if (hours % 24 === 0) return hours === 24 ? '1 day' : `${hours / 24} days`;
  return hours === 1 ? '1 hour' : `${hours} hours`;
}

/**
 * The share link lifetimes to offer, shortest first, and the one to
 * preselect, from the hub's default and maximum on GET /links
 * (ptone/scion#4585): the presets up to the maximum, plus the default and
 * the maximum when they are not presets. The hub refuses a longer lifetime,
 * and applies its default when none is given. Without both values (an older
 * hub) it is the presets with DEFAULT_LINK_HOURS.
 */
export function linkLifetimeChoices(
  defaultHours?: number,
  maxHours?: number
): { options: LinkLifetime[]; selected: number } {
  const valid = (h?: number): h is number => typeof h === 'number' && Number.isInteger(h) && h > 0;
  if (!valid(defaultHours) || !valid(maxHours)) {
    return { options: [...LINK_LIFETIMES], selected: DEFAULT_LINK_HOURS };
  }
  const selected = Math.min(defaultHours, maxHours);
  const hours = new Set(LINK_LIFETIMES.map((l) => l.hours).filter((h) => h <= maxHours));
  hours.add(selected);
  hours.add(maxHours);
  const sorted = [...hours].sort((a, b) => a - b);
  return { options: sorted.map((h) => ({ label: linkLifetimeLabel(h), hours: h })), selected };
}

/** Labels of the grant permissions. */
export const PERMISSION_LABELS: Record<GrantPermission, string> = {
  read: 'Can view',
  review: 'Can review',
  write: 'Can edit',
  admin: 'Can manage',
};

/**
 * The permissions the Share dialog's permission menus offer, weakest first.
 * Review is given through Grant review access, and a review grant is shown
 * with its label only.
 */
export const SELECTABLE_PERMISSIONS: readonly GrantPermission[] = ['read', 'write', 'admin'];

/** Body of GET /api/v1/artifacts/{id}/links. */
export interface ShareLinkList {
  links: ShareLink[];
  /** Lifetime, in hours, of a link created without one; absent from older hubs. */
  defaultTtlHours?: number;
  /** Longest lifetime, in hours, the hub accepts; absent from older hubs. */
  maxTtlHours?: number;
}

/** Lists an artifact's unexpired share links, with the hub's link lifetimes. */
export async function listLinks(id: string): Promise<ShareLinkList> {
  const body = await okJSON<ShareLinkList>(await apiFetch(`${artifactPath(id)}/links`));
  return { ...body, links: body.links ?? [] };
}

/** Creates a share link that lasts ttlHours. */
export async function createLink(id: string, ttlHours: number): Promise<CreateLinkResponse> {
  return okJSON(
    await apiFetch(`${artifactPath(id)}/links`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ttlHours }),
    })
  );
}

/** Revokes a share link. */
export async function revokeLink(id: string, linkId: string): Promise<void> {
  const res = await apiFetch(`${artifactPath(id)}/links/${encodeURIComponent(linkId)}`, {
    method: 'DELETE',
  });
  if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
}

/** Lists an artifact's grants, the home project's first. */
export async function listGrants(id: string): Promise<GrantListResponse> {
  const body = await okJSON<GrantListResponse>(await apiFetch(`${artifactPath(id)}/grants`));
  return { grants: body.grants ?? [], crossProjectSharing: !!body.crossProjectSharing };
}

/** Adds a grant, or changes the permission of the subject's grant. */
export async function putGrant(
  id: string,
  subjectKind: ArtifactGrant['subjectKind'],
  subjectRef: string,
  permission: GrantPermission
): Promise<ArtifactGrant> {
  const body = await okJSON<{ grant: ArtifactGrant }>(
    await apiFetch(`${artifactPath(id)}/grants`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ subjectKind, subjectRef, permission }),
    })
  );
  return body.grant;
}

/** Removes a grant. */
export async function deleteGrant(id: string, grantId: string): Promise<void> {
  const res = await apiFetch(`${artifactPath(id)}/grants/${encodeURIComponent(grantId)}`, {
    method: 'DELETE',
  });
  if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
}

/**
 * Changes an artifact: expiresAt sets its expiry (null clears it), scopeRef
 * moves it to another home project.
 */
export async function patchArtifact(
  id: string,
  change: { expiresAt?: string | null; scopeRef?: string }
): Promise<PatchArtifactResponse> {
  return okJSON(
    await apiFetch(artifactPath(id), {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(change),
    })
  );
}

/** The absolute URL of a share link from the hub-relative path it was created with. */
export function shareLinkUrl(path: string, origin: string): string {
  return new URL(path, origin).toString();
}

/**
 * What setting the artifact's expiry to expiresAt (null: kept until
 * deleted) would cut: the unexpired links that would end before their own
 * expiry, and the grants deleted with the artifact. Mirrors the counts the
 * hub returns from PATCH.
 */
export function expiryImpact(
  expiresAt: Date | null,
  links: readonly ShareLink[],
  grants: readonly ArtifactGrant[],
  now: Date
): { linksCutShort: number; grantsRemoved: number } {
  if (!expiresAt) return { linksCutShort: 0, grantsRemoved: 0 };
  let linksCutShort = 0;
  for (const l of links) {
    const end = new Date(l.expiresAt);
    if (end > now && expiresAt < end) linksCutShort++;
  }
  return { linksCutShort, grantsRemoved: grants.length };
}

// ---------------------------------------------------------------------------
// Artifact references in chat messages (ptone/scion#3224)
// ---------------------------------------------------------------------------

/**
 * Message metadata key the composer sends artifact references in: a JSON
 * array of scion://artifact/<id>[@<seq>] strings. The hub keeps only those
 * the sender can read.
 */
export const ARTIFACTS_METADATA_KEY = 'artifacts';

/** Most artifact references one message may carry (hub limit). */
export const MAX_MESSAGE_ARTIFACTS = 10;

/**
 * One artifact reference on a message, as the hub resolved it for the
 * viewer (chat history `messageArtifacts`, send response `artifacts`).
 * When available is false the viewer cannot read it (or it is gone) and
 * only ref, id and seq are set.
 */
export interface MessageArtifactRef {
  ref: string;
  id: string;
  seq?: number;
  available: boolean;
  title?: string;
  version?: number;
  ownerKind?: string;
  ownerRef?: string;
  ownerName?: string;
}

/** Canonical reference string of an artifact, pinned to seq when seq > 0. */
export function formatArtifactRef(id: string, seq?: number): string {
  return seq && seq > 0 ? `scion://artifact/${id}@${seq}` : `scion://artifact/${id}`;
}

/**
 * Orders a message's artifact references by where each artifact first
 * appears in the message text (the CLI adds the reference URLs to the text
 * in send order). References whose artifact the text does not name, such
 * as those attached in the web composer, keep their relative order after
 * the others: send order right after sending, artifact id order in
 * history, which is the order the hub returns them in.
 */
export function orderArtifactRefs(
  refs: readonly MessageArtifactRef[],
  body: string
): MessageArtifactRef[] {
  const lower = body.toLowerCase();
  const indexed = refs.map((r, i) => {
    const at = lower.indexOf(`scion://artifact/${r.id.toLowerCase()}`);
    return { r, i, at: at < 0 ? Number.MAX_SAFE_INTEGER : at };
  });
  indexed.sort((a, b) => a.at - b.at || a.i - b.i);
  return indexed.map((x) => x.r);
}
