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
 * Pure container-file-path helpers shared by the chat message renderer
 * (`chat-message.ts`'s clickable path links) and the recent-files recorder
 * (`chat-recent-files.ts`). Keeping one copy of the recognized-path rules and
 * the container-path parser/URL builder means both agree on exactly which
 * strings are "a file" and how to resolve them.
 *
 * This module has no DOM, Lit or network dependency: everything here is a
 * pure function over strings, safe to unit test directly and to import from
 * either a component or a plain data module.
 */

// ---------------------------------------------------------------------------
// Recognized container-path pattern (moved from chat-message.ts's
// ENTITY_PATTERNS file-path entry, and chat-thread.ts's parseContainerPath).
// ---------------------------------------------------------------------------

/** Known filenames that should be linked/indexed even without a file extension. */
export const EXTENSIONLESS_FILES = new Set([
  'makefile',
  'dockerfile',
  'license',
  'readme',
  'changelog',
  'gemfile',
  'rakefile',
  'procfile',
  'vagrantfile',
  'justfile',
  'taskfile',
  'caddyfile',
]);

/**
 * Matches `/scion-volumes/...` and `/workspace/...` container paths (which
 * includes the in-workspace shared-dir mount `/workspace/.scion-volumes/...`).
 * Does not by itself decide whether the match is "a file" — see
 * {@link isRecognizedFilePath} — nor whether it sits inside an HTTP(S) URL —
 * see {@link extractContainerPaths}.
 */
export const CONTAINER_PATH_PATTERN =
  /(?:\/scion-volumes\/[a-zA-Z0-9_.-]*[a-zA-Z0-9_-](?:\/[a-zA-Z0-9_.-]*[a-zA-Z0-9_-])*|\/workspace\/(?:\.scion-volumes\/[a-zA-Z0-9_.-]*[a-zA-Z0-9_-](?:\/[a-zA-Z0-9_.-]*[a-zA-Z0-9_-])*|[a-zA-Z0-9_.-]*[a-zA-Z0-9_-](?:\/[a-zA-Z0-9_.-]*[a-zA-Z0-9_-])*))/g;

/** Matches an `http(s)://` destination, stopping at whitespace or common delimiters. */
const HTTP_URL_PATTERN = /https?:\/\/[^\s<>"')]+/gi;

/**
 * True when a matched container-path's last segment looks like a real file:
 * it has an extension, or is a well-known extensionless filename (Makefile,
 * Dockerfile, ...). A bare directory reference ("/workspace/src") is not a
 * file and must not be linked or indexed.
 */
export function isRecognizedFilePath(path: string): boolean {
  const lastSegment = path.split('/').pop() || '';
  return lastSegment.includes('.') || EXTENSIONLESS_FILES.has(lastSegment.toLowerCase());
}

/**
 * Extract distinct, order-preserving, recognized container file paths from
 * raw message text (not rendered HTML). Ptone's decision: only files, never
 * URLs — a path substring that is part of an `http(s)://` destination is
 * excluded even when its suffix matches a supported path shape, so a message
 * containing `https://example.com/workspace/a.md` never creates a local file
 * entry for `/workspace/a.md`.
 */
export function extractContainerPaths(text: string): string[] {
  if (!text) return [];

  const urlRanges: Array<[number, number]> = [];
  const urlRe = new RegExp(HTTP_URL_PATTERN.source, HTTP_URL_PATTERN.flags);
  let um: RegExpExecArray | null;
  while ((um = urlRe.exec(text)) !== null) {
    if (um[0].length === 0) {
      urlRe.lastIndex++;
      continue;
    }
    urlRanges.push([um.index, um.index + um[0].length]);
  }

  const insideUrl = (index: number): boolean =>
    urlRanges.some(([start, end]) => index >= start && index < end);

  const pathRe = new RegExp(CONTAINER_PATH_PATTERN.source, CONTAINER_PATH_PATTERN.flags);
  const seen = new Set<string>();
  const out: string[] = [];
  let pm: RegExpExecArray | null;
  while ((pm = pathRe.exec(text)) !== null) {
    if (pm[0].length === 0) {
      pathRe.lastIndex++;
      continue;
    }
    const path = pm[0];
    if (!insideUrl(pm.index) && isRecognizedFilePath(path) && !seen.has(path)) {
      seen.add(path);
      out.push(path);
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Container-path parsing and API URL construction (moved from
// chat-thread.ts's parseContainerPath / buildFileApiUrl / encodeFilePath).
// ---------------------------------------------------------------------------

export interface PathLinkTarget {
  kind: 'workspace' | 'shared-dir';
  /** Shared-directory name (only for kind === 'shared-dir'). */
  dirName?: string;
  /** File path within the workspace or shared directory. */
  filePath: string;
}

/** Encode each segment of a file path for use in API URLs. */
export function encodeFilePath(filePath: string): string {
  return filePath
    .split('/')
    .map((seg) => encodeURIComponent(seg))
    .join('/');
}

/**
 * A percent-encoded `.`, `/` or `\`, any hex-digit case (`%2e`, `%2E`, `%2f`,
 * `%5c`, ...). Checked as a raw substring — anywhere it appears, the path is
 * refused outright, never decoded or otherwise "cleaned up": a single
 * decode pass downstream could turn `%2e%2e` back into `..`.
 */
const ENCODED_DANGEROUS_BYTE = /%(?:2e|2f|5c)/i;

/** Characters with no legitimate place in a container file path segment: query/fragment delimiters and a literal backslash. */
const DISALLOWED_PATH_CHARS = /[?#\\]/;

/**
 * True when `path` is safe to treat as a real file path: non-empty, every
 * `/`-separated segment is a real single component (not empty, not `.` or
 * `..`), and it contains no raw or percent-encoded traversal/route-changing
 * character. Used to reject a traversal segment (`/workspace/../../agents`,
 * which the browser and the hub's file server would otherwise collapse into
 * a request that never reaches `/files/`), its encoded spellings, a
 * directory-like empty segment (`//`), a bare directory reference (an empty
 * `path` altogether), and `?`/`#` injection — before a container path is
 * ever turned into an API URL or an indexed record. Exported so the
 * recent-files store can apply the identical rule to a path read back from
 * `localStorage` or a cross-tab `storage` event, which never went through
 * this parser the first time.
 */
export function hasOnlySafePathSegments(path: string): boolean {
  if (path === '' || DISALLOWED_PATH_CHARS.test(path) || ENCODED_DANGEROUS_BYTE.test(path)) {
    return false;
  }
  return path.split('/').every((seg) => seg !== '' && seg !== '.' && seg !== '..');
}

/**
 * True when `id` is safe to place as a single, exact URL path segment on its
 * own — stricter than {@link hasOnlySafePathSegments}, which permits internal
 * `/` separators because a file path is expected to have more than one
 * segment. A project ID must never contain a `/` at all: an embedded
 * separator would add or remove a route segment rather than staying inside
 * the one slot it's given, which is exactly the failure mode
 * `buildFileApiUrl`'s fixed route prefix exists to prevent. Used for
 * `projectId` wherever it's placed into an API route or persisted alongside
 * a record that will be.
 */
export function hasOnlySafeSingleSegment(id: string): boolean {
  if (
    id === '' ||
    id.includes('/') ||
    DISALLOWED_PATH_CHARS.test(id) ||
    ENCODED_DANGEROUS_BYTE.test(id)
  ) {
    return false;
  }
  return id !== '.' && id !== '..';
}

/**
 * Parse a container path into the API type and parameters.
 *
 * Supported patterns:
 *   /scion-volumes/{dirName}/{filePath}       -> shared-dir
 *   /workspace/.scion-volumes/{dirName}/{fp}   -> shared-dir (in-workspace mount)
 *   /workspace/{filePath}                      -> workspace
 *
 * The two shared-dir spellings normalize to the identical
 * `{kind:'shared-dir', dirName, filePath}` shape, so a caller keying identity
 * off the parsed target (rather than the raw string) treats them as the same
 * file — see `pathIdentityKey`.
 *
 * Rejects (returns null for) a `dirName` or `filePath` containing a `.`/`..`
 * segment or an empty segment, and a bare directory reference (no filePath at
 * all) — none of those name an actual file, and passing one through to
 * `buildFileApiUrl` would build a URL a browser/server may normalize into a
 * request for a completely different, non-file API endpoint.
 */
export function parseContainerPath(containerPath: string): PathLinkTarget | null {
  // /scion-volumes/{dirName}/...
  const sharedDirMatch = containerPath.match(/^\/scion-volumes\/([^/]+)(?:\/(.+))?$/);
  if (sharedDirMatch) {
    const dirName = sharedDirMatch[1];
    const filePath = sharedDirMatch[2] || '';
    if (!hasOnlySafePathSegments(dirName) || !hasOnlySafePathSegments(filePath)) return null;
    return { kind: 'shared-dir', dirName, filePath };
  }

  // /workspace/.scion-volumes/{dirName}/...
  const inWorkspaceMatch = containerPath.match(
    /^\/workspace\/\.scion-volumes\/([^/]+)(?:\/(.+))?$/
  );
  if (inWorkspaceMatch) {
    const dirName = inWorkspaceMatch[1];
    const filePath = inWorkspaceMatch[2] || '';
    if (!hasOnlySafePathSegments(dirName) || !hasOnlySafePathSegments(filePath)) return null;
    return { kind: 'shared-dir', dirName, filePath };
  }

  // /workspace/...
  const workspaceMatch = containerPath.match(/^\/workspace\/(.+)$/);
  if (workspaceMatch) {
    const filePath = workspaceMatch[1];
    if (!hasOnlySafePathSegments(filePath)) return null;
    return { kind: 'workspace', filePath };
  }

  return null;
}

/**
 * A single expected path segment for {@link pinBuiltUrl}: an exact literal
 * string — always the segment's own already-percent-encoded value, e.g.
 * `encodeURIComponent(projectId)`, never the raw, unencoded value — or
 * `null` for a variable part (a file-path segment) whose value isn't known
 * ahead of time. Every segment this module's builders know the exact value
 * of (a project id, a shared-dir name, an attachment id) is pinned by that
 * exact value, not just checked for "some safe segment": this documents,
 * per segment, exactly which value a caller expects the built URL to carry
 * there. It is not what rejects a same-shape retargeting traversal, though
 * — a builder always compares the literal against the very value it just
 * interpolated into that position, so the comparison is true by
 * construction. What rejects a retarget is check 4's per-segment dot
 * comparison and check 5's raw-vs-normalized equality (see
 * {@link pinBuiltUrl}); see the retargeting-traversal describe block in this
 * module's tests.
 */
export type PinSegment = string | null;

/**
 * Sink-side pin (independent defence in depth, downstream of every other
 * validator): re-parses a URL a builder is about to return and asserts its
 * *raw* pathname — the exact string the builder assembled, before any
 * WHATWG URL normalization — is exactly the expected shape. This runs
 * unconditionally, right before every URL-building function returns, so a
 * bypass anywhere upstream — a validator with a gap, a future caller that
 * skips validation, a corrupted value that somehow got this far — still
 * fails closed here instead of producing a request against a different
 * route.
 *
 * Critically, this checks the raw string directly, split on its own literal
 * `/` characters — it never lets `new URL()` parse the pathname first. The
 * WHATWG URL Standard resolves a `..`/`.` segment (and every `%2e` spelling
 * of one, any case) against whatever real segment precedes it *before*
 * `.pathname` is ever returned, so a traversal that climbs out of one
 * segment and back into another of the same name is invisible by the time a
 * normalized pathname is inspected — including a traversal that climbs out
 * of the current project or shared-dir entirely and back into a
 * *different* one, which still normalizes to a route of the identical
 * shape and so would otherwise pass every shape/count check undetected.
 * Splitting the raw string ourselves means every literal `..`/`.` segment
 * — and every segment that decodes to one — is seen and rejected directly,
 * regardless of what it would have normalized to.
 *
 * Checks, in order:
 * 1. No query string or fragment in the raw string at all — every caller
 *    appends its own `?view=`/`?format=` *after* calling the builder, never
 *    before, so the builder's own output must carry neither. Detected by
 *    scanning the raw string directly, not by asking `new URL()` to split
 *    it, since a raw `?`/`#` the builder failed to encode should be treated
 *    as part of the path it corrupted, not silently reinterpreted as a
 *    query/fragment delimiter.
 * 2. The raw path starts with exactly `fixedShape`'s segments, in order — a
 *    literal string must match that exact encoded value; `null` matches any
 *    single real segment there (checked below for safety, but not pinned to
 *    a specific value — used only for a segment this module doesn't know
 *    the expected value of ahead of time).
 * 3. The number of segments after `fixedShape` falls within
 *    `trailingRange` (pass the same value for `min`/`max` to pin an exact
 *    count; leave `max` unset for a variable-length file path).
 * 4. Every segment in the *whole* raw path — fixed, variable, and trailing
 *    alike — is non-empty, validly percent-encoded, and, once decoded, is
 *    neither `.` nor `..` nor contains a `/` or `\`. Segments are decoded
 *    one at a time, never the joined pathname at once, so an embedded
 *    `%2f`/`%5c` can never merge two segments into one or split one into
 *    two before this check sees it.
 * 5. Finally, as independent defence in depth against this function's own
 *    raw-parsing logic missing some WHATWG edge case: re-parse the same URL
 *    with `new URL()` and require its normalized pathname to be
 *    character-for-character identical to the raw path checked above. A
 *    URL this module builds itself should never be normalized at all —
 *    every segment already passed the checks above — so any difference
 *    means something upstream produced an unexpected shape, and this fails
 *    closed rather than trusting that steps 1–4 alone cover every case.
 *
 * On check 4's dot-segment comparison and check 5, for a raw or %2e-spelled
 * `.`/`..` segment: each is individually sufficient against this module's
 * own test suite — removing *either one alone* (leaving the other active)
 * still catches every such vector tested, because check 5's blanket
 * normalization-equality comparison and check 4's explicit per-segment dot
 * comparison happen to cover the same ground for that case. Removing *both
 * together* does lose real coverage (every traversal/retargeting test then
 * fails). Both are kept: check 4 states the reject-not-normalize rule
 * explicitly, in one place per segment, independent of whatever `new URL()`
 * happens to do; check 5 is the backstop against a segment-level case check
 * 4 doesn't enumerate, or a different `URL` implementation behaving
 * unexpectedly.
 *
 * Check 5 is *not* redundant, though, for a dot segment split by a raw
 * ASCII tab, CR or LF (e.g. `.` + tab + `.`, or `%2e` + LF + `%2e`), or for
 * a leading/trailing tab or space on the whole built string: the WHATWG URL
 * parser deletes every ASCII tab/CR/LF from its input, and trims leading/
 * trailing C0-control-or-space characters, before resolving dot segments —
 * so `.\t.` becomes `..` under `new URL()` even though `decodeURIComponent`
 * on the raw segment leaves the tab exactly where it is, meaning it is
 * never `.` or `..` and check 4 cannot see it at all. For this class, check
 * 5 is the *sole* defence; see the control-character-split describe block in
 * this module's tests.
 *
 * Check 1's raw `#` scan is likewise the sole defence against a trailing,
 * empty fragment (e.g. `…/notes.txt#`): it normalizes to a pathname
 * identical to the raw path with an empty `search`/`hash`, so check 5
 * cannot tell it apart from a URL with no fragment at all. Check 1's raw
 * `?` scan, by contrast, is redundant with check 5: a raw `?` always
 * changes the raw path relative to what `new URL()` reports once it splits
 * the query off, so check 5 alone still catches it.
 *
 * Throws rather than returning a boolean: a builder that fails this check
 * must never return a URL at all, so every caller fails closed by
 * construction, not by remembering to check a return value.
 */
export function pinBuiltUrl(
  url: string,
  fixedShape: readonly PinSegment[],
  trailingRange: { min: number; max?: number }
): void {
  const hashIndex = url.indexOf('#');
  const beforeHash = hashIndex === -1 ? url : url.slice(0, hashIndex);
  const queryIndex = beforeHash.indexOf('?');
  if (hashIndex !== -1 || queryIndex !== -1) {
    throw new Error('pinBuiltUrl: unexpected query string or fragment');
  }
  const rawPath = beforeHash;

  const rawParts = rawPath.split('/');
  if (rawParts[0] !== '') {
    throw new Error('pinBuiltUrl: pathname does not start with /');
  }
  const segments = rawParts.slice(1);
  if (segments.length < fixedShape.length) {
    throw new Error('pinBuiltUrl: fewer segments than the expected route prefix');
  }
  for (let i = 0; i < fixedShape.length; i++) {
    const expected = fixedShape[i];
    if (expected !== null && segments[i] !== expected) {
      throw new Error('pinBuiltUrl: pathname does not match the expected route prefix');
    }
  }
  const trailing = segments.slice(fixedShape.length);
  if (
    trailing.length < trailingRange.min ||
    (trailingRange.max !== undefined && trailing.length > trailingRange.max)
  ) {
    throw new Error('pinBuiltUrl: unexpected number of trailing path segments');
  }
  for (const seg of segments) {
    if (seg === '') {
      throw new Error('pinBuiltUrl: an empty path segment');
    }
    let decoded: string;
    try {
      decoded = decodeURIComponent(seg);
    } catch {
      throw new Error('pinBuiltUrl: a path segment is not validly percent-encoded');
    }
    if (decoded === '.' || decoded === '..') {
      throw new Error('pinBuiltUrl: a path segment resolves to a dot segment');
    }
    if (decoded.includes('/') || decoded.includes('\\')) {
      throw new Error('pinBuiltUrl: a path segment decodes to contain a path separator');
    }
  }

  // Independent re-check: a URL this module built itself must never be
  // normalized by URL parsing at all. If it is, the raw-segment checks
  // above missed something — fail closed rather than trust them alone.
  const parsed = new URL(url, 'http://pin.invalid');
  if (parsed.pathname !== rawPath || parsed.search !== '' || parsed.hash !== '') {
    throw new Error('pinBuiltUrl: the built URL was normalized by URL parsing');
  }
}

/**
 * Constructs the URL string and pins it — with no source-side safety
 * validation of `projectId`/`target` at all. Exported *only* so the test
 * suite can prove the sink pin alone — independent of
 * `hasOnlySafeSingleSegment`/`hasOnlySafePathSegments` — rejects every
 * bypass class, by calling this directly with values a source validator
 * would normally have already rejected. Every real caller must go through
 * {@link buildFileApiUrl}, which validates first and then delegates here.
 */
export function buildFileApiUrlPinOnly(projectId: string, target: PathLinkTarget): string {
  const encodedProjectId = encodeURIComponent(projectId);
  if (target.kind === 'shared-dir') {
    const encodedDirName = encodeURIComponent(target.dirName!);
    const url = `/api/v1/projects/${encodedProjectId}/shared-dirs/${encodedDirName}/files/${encodeFilePath(target.filePath)}`;
    pinBuiltUrl(
      url,
      ['api', 'v1', 'projects', encodedProjectId, 'shared-dirs', encodedDirName, 'files'],
      { min: 1 }
    );
    return url;
  }
  const url = `/api/v1/projects/${encodedProjectId}/workspace/files/${encodeFilePath(target.filePath)}`;
  pinBuiltUrl(url, ['api', 'v1', 'projects', encodedProjectId, 'workspace', 'files'], { min: 1 });
  return url;
}

/**
 * Build the API URL for a parsed path-link target. Every segment is
 * percent-encoded individually (never joined-then-encoded, which would let
 * an embedded `/` re-introduce a path separator), and the route prefix
 * (`.../workspace/files/` or `.../shared-dirs/{dir}/files/`) is a fixed
 * literal — the only variable part of the pathname is the encoded, already
 * validated `filePath`, so no input can change which API route is hit.
 *
 * Re-validates `projectId`/`dirName`/`filePath` even though every caller is
 * expected to have gone through `parseContainerPath` (or the equivalent check
 * in `sanitizeRecord`) first: a value read back from `localStorage`, a
 * cross-tab `storage` event, or any future caller that skips that step must
 * not silently build a URL that escapes the files endpoint. `projectId` is
 * checked against {@link hasOnlySafeSingleSegment} rather than
 * `hasOnlySafePathSegments` — it must occupy exactly one route segment, so
 * unlike a file path, an embedded `/` is unsafe for it too (it would add a
 * segment rather than staying inside the one slot the project ID has). Throws
 * rather than guessing a "cleaned" URL — reject, don't normalize.
 */
export function buildFileApiUrl(projectId: string, target: PathLinkTarget): string {
  if (!hasOnlySafeSingleSegment(projectId)) {
    throw new Error('buildFileApiUrl: unsafe project id');
  }
  if (target.kind === 'shared-dir') {
    if (!hasOnlySafePathSegments(target.dirName!) || !hasOnlySafePathSegments(target.filePath)) {
      throw new Error('buildFileApiUrl: unsafe shared-dir path');
    }
  } else if (!hasOnlySafePathSegments(target.filePath)) {
    throw new Error('buildFileApiUrl: unsafe workspace path');
  }
  return buildFileApiUrlPinOnly(projectId, target);
}

/**
 * Constructs the URL string and pins it — with no source-side safety
 * validation of `id` at all. Exported *only* so the test suite can prove
 * the sink pin alone rejects every bypass class; every real caller must go
 * through {@link buildAttachmentApiUrl}.
 */
export function buildAttachmentApiUrlPinOnly(id: string): string {
  const encodedId = encodeURIComponent(id);
  const url = `/api/v1/chat/attachments/${encodedId}`;
  pinBuiltUrl(url, ['api', 'v1', 'chat', 'attachments', encodedId], { min: 0, max: 0 });
  return url;
}

/**
 * Build the API URL for an attachment, addressed by its opaque id. Colocated
 * here with `buildFileApiUrl`, not left as a private helper inside a
 * component, since both are "turn a persisted/hydrated identifier into a
 * fetchable API URL" — exactly the category of function that needs the same
 * reject-then-pin treatment every time a new one is added, not re-derived
 * per component. Throws on an unsafe id (never guesses a "cleaned" URL), and
 * independently pins the built URL's route shape before returning it, the
 * same as `buildFileApiUrl`.
 *
 * The id-safety check and the pin are complementary, not redundant —
 * removing the id check alone still lets some vectors reach a
 * technically-inert output: `encodeURIComponent` turns a raw `?`/`#`/`%`
 * into its own percent-encoded form before the pin ever sees it, so a
 * decoded `?`/`#` in a segment — unlike a decoded `/`/`\` — is not itself a
 * route hazard once safely encoded, and a value like `%2e%2e` becomes
 * double-encoded (`%252e%252e`), which decodes only once server-side to a
 * literal, harmless filename, not a traversal. The id check is what rejects
 * those textually-suspicious inputs outright, independent of whether this
 * particular encoding step happens to neutralize them; the pin is what
 * independently re-verifies the *actual resulting route*, including a raw
 * `.`/`..`, which `encodeURIComponent` leaves untouched (both are in its
 * unreserved-character set) and which the pin rejects by splitting the raw
 * string itself.
 */
export function buildAttachmentApiUrl(id: string): string {
  if (!hasOnlySafeSingleSegment(id)) {
    throw new Error('buildAttachmentApiUrl: unsafe attachment id');
  }
  return buildAttachmentApiUrlPinOnly(id);
}

// ---------------------------------------------------------------------------
// Identity keys — attachment/path identity for the recent-files index.
// ---------------------------------------------------------------------------

/** Stable identity tuple for an attachment: `['attachment', id]`. */
export function attachmentIdentityKey(id: string): string {
  return JSON.stringify(['attachment', id]);
}

/**
 * Stable identity tuple for a resolved path:
 * `['path', projectId, location.kind, dirName-or-empty, filePath]`, serialized
 * as a tuple to avoid delimiter collisions. Two different projects with the
 * same file path are different files; the two shared-dir path spellings that
 * `parseContainerPath` normalizes to the same target are the same file.
 */
export function pathIdentityKey(projectId: string, target: PathLinkTarget): string {
  return JSON.stringify([
    'path',
    projectId,
    target.kind,
    target.kind === 'shared-dir' ? (target.dirName ?? '') : '',
    target.filePath,
  ]);
}

// ---------------------------------------------------------------------------
// Name-based classification shared by preview rendering.
// ---------------------------------------------------------------------------

/** Largest file fetched for an inline text preview, in bytes (512 KiB). */
export const TEXT_PREVIEW_MAX_BYTES = 512 * 1024;

/** Human-readable file size, shared by the message attachment chip and the Documents palette row. */
export function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

const IMAGE_EXTENSIONS = new Set([
  '.png',
  '.jpg',
  '.jpeg',
  '.gif',
  '.svg',
  '.webp',
  '.bmp',
  '.ico',
]);
const MARKDOWN_EXTENSIONS = new Set(['.md', '.markdown']);

/** Lowercase extension including the dot, or '' when the name has none. */
export function extensionOf(name: string): string {
  const dot = name.lastIndexOf('.');
  return dot > 0 ? name.slice(dot).toLowerCase() : '';
}

/** True when a file name's extension is a known previewable image type. */
export function isImageFileName(name: string): boolean {
  return IMAGE_EXTENSIONS.has(extensionOf(name));
}

/** True when a file name's extension marks it as Markdown. */
export function isMarkdownFileName(name: string): boolean {
  return MARKDOWN_EXTENSIONS.has(extensionOf(name));
}

/**
 * Extensions (beyond Markdown, checked separately) known to be safe to fetch
 * and render as plain text/code, not binary data.
 */
const KNOWN_TEXT_EXTENSIONS = new Set([
  '.txt',
  '.json',
  '.jsonc',
  '.xml',
  '.yaml',
  '.yml',
  '.toml',
  '.ini',
  '.conf',
  '.cfg',
  '.env',
  '.csv',
  '.tsv',
  '.log',
  '.js',
  '.mjs',
  '.cjs',
  '.ts',
  '.tsx',
  '.jsx',
  '.py',
  '.go',
  '.java',
  '.kt',
  '.c',
  '.h',
  '.cpp',
  '.hpp',
  '.rs',
  '.rb',
  '.php',
  '.sh',
  '.bash',
  '.zsh',
  '.sql',
  '.css',
  '.scss',
  '.html',
  '.htm',
  '.proto',
]);

/** `application/*` MIME types (beyond `text/*`, checked separately) known to be plain-text bodies, not binary data. */
const KNOWN_TEXT_APPLICATION_MIMES = new Set([
  'application/json',
  'application/xml',
  'application/x-yaml',
  'application/yaml',
  'application/javascript',
  'application/typescript',
  'application/x-sh',
  'application/toml',
  'application/x-ndjson',
]);

/**
 * True for a file name whose extension (or well-known extensionless name —
 * Makefile, Dockerfile, README, ...) is known to be safe to fetch and render
 * as plain text/code — the classification a container-path target uses,
 * since it never carries a MIME type of its own.
 */
export function isLikelyTextFileName(name: string): boolean {
  if (isMarkdownFileName(name)) return true;
  if (KNOWN_TEXT_EXTENSIONS.has(extensionOf(name))) return true;
  const lastSegment = name.split('/').pop() || name;
  return !lastSegment.includes('.') && EXTENSIONLESS_FILES.has(lastSegment.toLowerCase());
}

/**
 * Lowercased MIME type with any `;`-delimited parameters (e.g.
 * `; charset=utf-8`) stripped, or '' for an empty input. Use this before
 * comparing a raw MIME string against a specific type, so a parameter or
 * unexpected case never defeats the comparison.
 */
export function baseMimeType(mime: string): string {
  return mime.toLowerCase().split(';')[0]?.trim() ?? '';
}

/**
 * True for a MIME type known to be safe to fetch and render as plain
 * text/code — the classification an attachment target uses, since it always
 * carries a MIME type (unlike a container path, which never does). A
 * structured-syntax suffix (`+json`, `+xml` — e.g. `application/ld+json`,
 * `image/svg+xml`) is text regardless of its top-level type, per RFC 6839.
 */
export function isLikelyTextMime(mime: string): boolean {
  const lower = baseMimeType(mime);
  if (lower.startsWith('text/')) return true;
  if (lower.endsWith('+json') || lower.endsWith('+xml')) return true;
  return KNOWN_TEXT_APPLICATION_MIMES.has(lower);
}

/**
 * Extensions of compressed archives, compiled/executable binaries and office
 * documents. A container-path target with one of these extensions is shown
 * as download-only without fetching its body. Any other path is fetched: the
 * server rejects non-UTF-8 workspace content, which the preview shows as its
 * error state.
 */
const KNOWN_BINARY_EXTENSIONS = new Set([
  '.zip',
  '.tar',
  '.gz',
  '.tgz',
  '.bz2',
  '.xz',
  '.7z',
  '.rar',
  '.exe',
  '.dll',
  '.so',
  '.dylib',
  '.bin',
  '.o',
  '.a',
  '.class',
  '.jar',
  '.war',
  '.wasm',
  '.pyc',
  '.pdf',
  '.doc',
  '.docx',
  '.xls',
  '.xlsx',
  '.ppt',
  '.pptx',
  '.db',
  '.sqlite',
  '.mp3',
  '.wav',
  '.ogg',
  '.m4a',
  '.flac',
  '.aac',
  '.mp4',
  '.mkv',
  '.mov',
  '.webm',
  '.avi',
  '.woff',
  '.woff2',
  '.ttf',
  '.otf',
  '.eot',
  '.dmg',
  '.iso',
  '.pkg',
  '.deb',
  '.rpm',
]);

/**
 * True for a file name whose extension is in KNOWN_BINARY_EXTENSIONS. Used
 * only for container-path targets, which carry no MIME type.
 */
export function isLikelyBinaryFileName(name: string): boolean {
  return KNOWN_BINARY_EXTENSIONS.has(extensionOf(name));
}

// ---------------------------------------------------------------------------
// Project resolution: a pure context-based project-resolution helper,
// shared by path clicks and recording. Mirrors chat-thread.ts's
// resolvePathLinkProjectId exactly.
// ---------------------------------------------------------------------------

/** Inputs needed to resolve which project a message's file paths belong to. */
export interface MessageProjectContext {
  /** True when the conversation is a DM (never a thread). */
  isDM: boolean;
  /** The thread's own project ID; irrelevant in a DM — see {@link resolveMessageProjectId}. */
  threadProjectId: string;
  /** The message's server-derived `senderProjectId`, if any. */
  senderProjectId?: string;
  /** The message's `projectId` field, if any. */
  messageProjectId?: string;
  /** The DM peer agent's project ID, from the shared in-memory agent cache. */
  peerAgentProjectId?: string;
}

/**
 * Resolve the best-available project ID for a message's file paths/links.
 *
 * For a project-scoped thread, the thread's own project ID takes priority
 * (falling back to the message's own project fields only if that's somehow
 * empty). A DM's thread-level project ID is not a project the DM belongs to —
 * it's whatever project the composer happened to be viewing before opening
 * the DM — so a DM never falls back to it; a DM prefers the message's own
 * project, then the peer agent's project, then nothing (never an unrelated
 * project — the caller must omit rather than guess).
 */
export function resolveMessageProjectId(ctx: MessageProjectContext): string {
  const fromMessage = ctx.senderProjectId || ctx.messageProjectId || '';
  if (!ctx.isDM) return ctx.threadProjectId || fromMessage;
  return fromMessage || ctx.peerAgentProjectId || '';
}
