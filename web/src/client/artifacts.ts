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
  createdAt: string;
  updatedAt: string;
}

export interface ArtifactResponse {
  artifact: Artifact;
  version?: ArtifactVersion;
  warnings?: string[];
}

/** GET /api/v1/artifacts/{id}/versions: ready versions, newest first, without files. */
export interface VersionListResponse {
  versions: ArtifactVersion[];
  nextBefore?: number;
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
  opts: { q?: string; cursor?: string; limit?: number; signal?: AbortSignal } = {}
): Promise<ArtifactListResponse> {
  const params = new URLSearchParams({ mine: '1', scope: projectId });
  if (opts.q) params.set('q', opts.q);
  if (opts.cursor) params.set('cursor', opts.cursor);
  if (opts.limit) params.set('limit', String(opts.limit));
  return okJSON(
    await apiFetch(`/api/v1/artifacts?${params.toString()}`, { signal: opts.signal ?? null })
  );
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
    readonly pending: PendingPublish | null
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
      throw new PublishError(reviewRejectedMessage(body), null);
    }
    if (code === 'stale_review') {
      throw new PublishError(
        'The version you reviewed is no longer the latest; your review was not saved. Review the current version.',
        null
      );
    }
    const message = prefix + (await extractApiError(res, `HTTP ${res.status}`));
    // The version is gone (404), no longer pending (409 conflict), or not
    // the caller's (403): another attempt must start a new version.
    const gone =
      res.status === 404 || res.status === 403 || (res.status === 409 && code === 'conflict');
    if (gone) throw new PublishError(message, null);
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
    throw new PublishError(message, left());
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
}

/** Body of GET /api/v1/artifacts?mine=1. */
export interface ArtifactListResponse {
  artifacts: ArtifactListItem[];
  /** Opaque cursor of the next page; absent on the last page. */
  nextCursor?: string;
}

/** Filters of the artifact list. */
export interface ArtifactListFilters {
  /** Title or key contains this text. */
  q?: string;
  /** Only artifacts whose current version is a review. */
  reviewPending?: boolean;
  /** Only artifacts the caller owns. */
  ownedOnly?: boolean;
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
