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

/** Feature flag that gates every artifact surface in the web UI. */
export const ARTIFACTS_FLAG = 'hub.artifacts';

export interface ArtifactFile {
  path: string;
  size: number;
  sha256: string;
  mediaType: string;
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
}

/** How the artifact page renders an entry file. */
export type ArtifactRenderer = 'markdown' | 'text' | 'image' | 'download';

/** Text media types that are not text/*. */
const TEXT_APPLICATION_TYPES = new Set([
  'application/json',
  'application/yaml',
  'application/toml',
  'application/xml',
]);

/**
 * Raster image types rendered with <img>. SVG is excluded: the hub serves
 * it as an attachment, and HTML-like content waits for the sandboxed
 * renderer of a later phase.
 */
const IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);

/** Picks the renderer for a file's media type. */
export function rendererFor(mediaType: string): ArtifactRenderer {
  const mt = mediaType.toLowerCase();
  if (mt === 'text/markdown') return 'markdown';
  if (IMAGE_TYPES.has(mt)) return 'image';
  if (mt === 'text/html') return 'download';
  if (mt.startsWith('text/') || TEXT_APPLICATION_TYPES.has(mt)) return 'text';
  return 'download';
}

/**
 * URL of a file of an artifact. seq 0 means the current version. stream
 * asks the hub to serve the bytes itself instead of redirecting to object
 * storage, which fetch() needs because the redirect is cross-origin.
 */
export function artifactFileUrl(id: string, seq: number, path: string, stream = false): string {
  const encodedPath = path
    .split('/')
    .map((seg) => encodeURIComponent(seg))
    .join('/');
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

/** Path of an artifact's page in the web UI. */
export function artifactPagePath(a: Pick<Artifact, 'id' | 'scopeRef'>): string {
  return `/projects/${encodeURIComponent(a.scopeRef)}/artifacts/${encodeURIComponent(a.id)}`;
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
