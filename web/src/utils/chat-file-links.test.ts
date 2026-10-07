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

import { describe, it, expect } from 'vitest';
import {
  isRecognizedFilePath,
  extractContainerPaths,
  parseContainerPath,
  buildFileApiUrl,
  buildFileApiUrlPinOnly,
  buildAttachmentApiUrl,
  buildAttachmentApiUrlPinOnly,
  hasOnlySafePathSegments,
  hasOnlySafeSingleSegment,
  pinBuiltUrl,
  attachmentIdentityKey,
  pathIdentityKey,
  isImageFileName,
  isGcsImageContentType,
  isGcsImageExtension,
  isMarkdownFileName,
  isLikelyTextFileName,
  isLikelyTextMime,
  isLikelyBinaryFileName,
  baseMimeType,
  extensionOf,
  resolveMessageProjectId,
  GCS_URI_PATTERN,
  resolveGcsMatch,
  parseGcsUri,
  escAttr,
  buildGcsLinkHtml,
  buildGcsObjectApiUrl,
  pinGcsObjectUrl,
  buildCloudConsoleUrl,
} from './chat-file-links.js';

describe('isRecognizedFilePath', () => {
  it('accepts a path whose last segment has an extension', () => {
    expect(isRecognizedFilePath('/workspace/src/main.go')).toBe(true);
  });

  it('rejects a bare directory reference with no extension', () => {
    expect(isRecognizedFilePath('/workspace/src')).toBe(false);
  });

  it('accepts a known extensionless filename', () => {
    expect(isRecognizedFilePath('/workspace/Makefile')).toBe(true);
  });

  it('is case-insensitive for the extensionless filename set', () => {
    expect(isRecognizedFilePath('/workspace/DOCKERFILE')).toBe(true);
  });

  it('rejects an unknown extensionless name', () => {
    expect(isRecognizedFilePath('/workspace/src/notes')).toBe(false);
  });
});

describe('extractContainerPaths', () => {
  it('extracts a single /workspace path from prose', () => {
    expect(extractContainerPaths('see /workspace/src/main.go for details')).toEqual([
      '/workspace/src/main.go',
    ]);
  });

  it('extracts a /scion-volumes path', () => {
    expect(extractContainerPaths('in /scion-volumes/scratchpad/notes.md')).toEqual([
      '/scion-volumes/scratchpad/notes.md',
    ]);
  });

  it('extracts more than one distinct path', () => {
    expect(extractContainerPaths('/workspace/a.md and also /workspace/b.py')).toEqual([
      '/workspace/a.md',
      '/workspace/b.py',
    ]);
  });

  it('de-duplicates a path mentioned twice, preserving first-seen order', () => {
    expect(extractContainerPaths('/workspace/a.md then again /workspace/a.md')).toEqual([
      '/workspace/a.md',
    ]);
  });

  it('drops a bare directory reference (no recognized file name)', () => {
    expect(extractContainerPaths('cd /workspace/src and look around')).toEqual([]);
  });

  it('excludes a path that is part of an http destination', () => {
    expect(extractContainerPaths('see https://example.com/workspace/a.md')).toEqual([]);
  });

  it('excludes a path inside an https destination specifically', () => {
    expect(extractContainerPaths('see https://example.com/scion-volumes/x/a.md')).toEqual([]);
  });

  it('still extracts a real path that appears after an unrelated URL', () => {
    expect(extractContainerPaths('docs at https://example.com/docs then /workspace/a.md')).toEqual([
      '/workspace/a.md',
    ]);
  });

  // Isolates insideUrl's lower bound (`index >= start`): without it, any
  // match earlier in the text than a later URL's *end* index is wrongly
  // treated as "inside" that URL, even though it sits entirely before the
  // URL even starts.
  it('still extracts a real path that appears before an unrelated URL later in the text', () => {
    expect(extractContainerPaths('/workspace/a.md then docs at https://example.com/docs')).toEqual([
      '/workspace/a.md',
    ]);
  });

  it('returns an empty array for empty text', () => {
    expect(extractContainerPaths('')).toEqual([]);
  });

  it('returns an empty array for text with no paths', () => {
    expect(extractContainerPaths('nothing to see here')).toEqual([]);
  });

  // The pattern's per-segment character class only allows a run of
  // word/dot/hyphen characters ending in a non-dot character, so a bare `..`
  // segment (or a `%`-containing encoded one) cannot complete a match at
  // all — these never reach isRecognizedFilePath/parseContainerPath in the
  // first place. Pinned here so a future loosening of the pattern is caught.
  const unmatchableInProse: Array<[string, string]> = [
    ['a .. segment', 'see /workspace/../../agents.txt for details'],
    ['a lone . segment', 'see /workspace/./agents.txt for details'],
    ['a %2e%2e encoded segment', 'see /workspace/%2e%2e/agents.txt for details'],
    ['a %2f encoded slash', 'see /workspace/foo%2f..%2fagents.txt for details'],
    ['a %5c encoded backslash', 'see /workspace/foo%5c..%5cagents.txt for details'],
  ];
  it.each(unmatchableInProse)('extracts nothing for %s: %s', (_label, text) => {
    expect(extractContainerPaths(text)).toEqual([]);
  });

  // A `?`/`#` delimiter isn't in the pattern's character class either, so the
  // match simply stops right before it — the safe prefix is extracted and
  // the dangerous suffix is never part of the recorded path at all.
  it('stops the match at a ? query delimiter, dropping the injected suffix', () => {
    expect(extractContainerPaths('see /workspace/notes.txt?../../secret for details')).toEqual([
      '/workspace/notes.txt',
    ]);
  });

  it('stops the match at a # fragment delimiter, dropping the injected suffix', () => {
    expect(extractContainerPaths('see /workspace/notes.txt#../../secret for details')).toEqual([
      '/workspace/notes.txt',
    ]);
  });

  // A gs:// object name may itself contain a '/'-separated run that happens
  // to look like a local container path — e.g. an object named
  // `workspace/report.md` sits right after the bucket's own `/` separator,
  // so the whole URI contains the literal substring `/workspace/report.md`.
  // That substring is not a local file at all, so the recent-files recorder
  // must not record it, the same way it already excludes a path embedded in
  // an http(s) URL.
  it('excludes a /workspace path embedded inside a gs:// URI', () => {
    expect(extractContainerPaths('see gs://bkt/workspace/report.md for details')).toEqual([]);
  });

  it('excludes a /scion-volumes path embedded inside a gs:// URI', () => {
    expect(extractContainerPaths('see gs://bkt/scion-volumes/shared/notes.md')).toEqual([]);
  });

  it('still extracts a real path elsewhere in the text while excluding the one embedded in a gs:// URI', () => {
    expect(extractContainerPaths('gs://bkt/workspace/a.md and also /workspace/b.md')).toEqual([
      '/workspace/b.md',
    ]);
  });

  // Isolates insideGcsUri's lower bound the same way the http(s) case above
  // does for insideUrl: a real path positioned before a gs:// URI that
  // appears later in the text must not be wrongly treated as "inside" that
  // URI merely because its index is less than the URI's end index.
  it('still extracts a real path that appears before an unrelated gs:// URI later in the text', () => {
    expect(extractContainerPaths('/workspace/a.md then gs://bkt/workspace/b.md')).toEqual([
      '/workspace/a.md',
    ]);
  });

  it('does not exclude a /workspace path that merely follows a gs:// URI with no embedded collision', () => {
    // A real product URI (bucket scion-xproject-exchange, object
    // workspace-volumes/dev-brief.md) contains "/workspace-volumes/", not
    // "/workspace/" — CONTAINER_PATH_PATTERN never matches inside it at all,
    // so a real, separate /workspace path later in the same message is
    // unaffected by the exclusion.
    expect(
      extractContainerPaths(
        'see gs://scion-xproject-exchange/workspace-volumes/dev-brief.md and /workspace/notes.md'
      )
    ).toEqual(['/workspace/notes.md']);
  });
});

describe('hasOnlySafePathSegments', () => {
  it('accepts an ordinary multi-segment path', () => {
    expect(hasOnlySafePathSegments('src/main.go')).toBe(true);
  });

  it('rejects an empty path', () => {
    expect(hasOnlySafePathSegments('')).toBe(false);
  });

  it('rejects a lone .. segment', () => {
    expect(hasOnlySafePathSegments('..')).toBe(false);
  });

  it('rejects a lone . segment', () => {
    expect(hasOnlySafePathSegments('.')).toBe(false);
  });

  it('rejects .. anywhere among several segments', () => {
    expect(hasOnlySafePathSegments('foo/../bar')).toBe(false);
  });

  it('rejects an empty segment from a doubled slash', () => {
    expect(hasOnlySafePathSegments('foo//bar')).toBe(false);
  });

  it('rejects a percent-encoded dot, case-insensitively', () => {
    expect(hasOnlySafePathSegments('foo/%2e%2e/bar')).toBe(false);
    expect(hasOnlySafePathSegments('foo/%2E%2E/bar')).toBe(false);
  });

  it('rejects a percent-encoded slash', () => {
    expect(hasOnlySafePathSegments('foo%2fbar')).toBe(false);
    expect(hasOnlySafePathSegments('foo%2Fbar')).toBe(false);
  });

  it('rejects a percent-encoded backslash', () => {
    expect(hasOnlySafePathSegments('foo%5cbar')).toBe(false);
    expect(hasOnlySafePathSegments('foo%5Cbar')).toBe(false);
  });

  it('rejects a literal backslash', () => {
    expect(hasOnlySafePathSegments('foo\\bar')).toBe(false);
  });

  it('rejects a query delimiter', () => {
    expect(hasOnlySafePathSegments('foo.txt?bar')).toBe(false);
  });

  it('rejects a fragment delimiter', () => {
    expect(hasOnlySafePathSegments('foo.txt#bar')).toBe(false);
  });

  it('accepts a filename that merely contains dots, not a dot segment', () => {
    expect(hasOnlySafePathSegments('my.file.tar.gz')).toBe(true);
  });
});

describe('hasOnlySafeSingleSegment', () => {
  it('accepts a plain project id', () => {
    expect(hasOnlySafeSingleSegment('proj-123')).toBe(true);
  });

  it('accepts a project id containing a dot as part of a larger token', () => {
    expect(hasOnlySafeSingleSegment('proj.v2')).toBe(true);
  });

  it('rejects empty', () => {
    expect(hasOnlySafeSingleSegment('')).toBe(false);
  });

  it('rejects a lone . segment', () => {
    expect(hasOnlySafeSingleSegment('.')).toBe(false);
  });

  it('rejects a lone .. segment', () => {
    expect(hasOnlySafeSingleSegment('..')).toBe(false);
  });

  // Unlike hasOnlySafePathSegments, an embedded slash is unsafe here — a
  // project id occupies exactly one route segment, so a `/` would add a
  // segment rather than staying inside the one slot it's given.
  it('rejects an embedded slash, even between two otherwise-safe segments', () => {
    expect(hasOnlySafeSingleSegment('a/b')).toBe(false);
  });

  it('rejects an encoded dot segment', () => {
    expect(hasOnlySafeSingleSegment('%2e%2e')).toBe(false);
    expect(hasOnlySafeSingleSegment('%2E%2E')).toBe(false);
  });

  it('rejects an encoded slash', () => {
    expect(hasOnlySafeSingleSegment('a%2fb')).toBe(false);
  });

  it('rejects an encoded backslash', () => {
    expect(hasOnlySafeSingleSegment('a%5cb')).toBe(false);
  });

  it('rejects a literal backslash', () => {
    expect(hasOnlySafeSingleSegment('a\\b')).toBe(false);
  });

  it('rejects a query delimiter', () => {
    expect(hasOnlySafeSingleSegment('a?b')).toBe(false);
  });

  it('rejects a fragment delimiter', () => {
    expect(hasOnlySafeSingleSegment('a#b')).toBe(false);
  });
});

// Every URL-building function must re-parse its own output and assert its
// exact route shape, as independent defence in depth downstream of whatever
// validation ran before. These tests call `pinBuiltUrl` directly with
// hand-built URL strings — never through `buildFileApiUrl`/`buildAttachmentApiUrl`
// and their own source-side validators — so they prove the pin *alone*
// rejects every bypass class, not that an earlier check already caught it first.
describe('pinBuiltUrl — sink-side pin, proven independent of the source validators', () => {
  const workspaceShape = ['api', 'v1', 'projects', null, 'workspace', 'files'] as const;
  const attachmentShape = ['api', 'v1', 'chat', 'attachments'] as const;
  const sharedDirShape = ['api', 'v1', 'projects', null, 'shared-dirs', null, 'files'] as const;

  it('accepts a legitimate workspace file URL', () => {
    expect(() =>
      pinBuiltUrl('/api/v1/projects/proj-1/workspace/files/src/main.go', workspaceShape, { min: 1 })
    ).not.toThrow();
  });

  it('accepts a legitimate nested workspace file URL', () => {
    expect(() =>
      pinBuiltUrl('/api/v1/projects/proj-1/workspace/files/a/b/c.txt', workspaceShape, { min: 1 })
    ).not.toThrow();
  });

  it('accepts a legitimate shared-dir file URL', () => {
    expect(() =>
      pinBuiltUrl(
        '/api/v1/projects/proj-1/shared-dirs/scratchpad/files/notes.txt',
        sharedDirShape,
        { min: 1 }
      )
    ).not.toThrow();
  });

  it('accepts a legitimate attachment URL', () => {
    expect(() =>
      pinBuiltUrl('/api/v1/chat/attachments/att-123', attachmentShape, { min: 1, max: 1 })
    ).not.toThrow();
  });

  it('rejects a URL whose fixed literal prefix does not match the pinned shape', () => {
    // Same total segment count as a valid match (so the trailing-count check
    // alone would not catch this) but the literal segment at the
    // `workspace` position has been swapped for something else — a future
    // bug that builds against a different route than the one this call site
    // is pinned to.
    expect(() =>
      pinBuiltUrl('/api/v1/projects/proj-1/agents/files/notes.txt', workspaceShape, { min: 1 })
    ).toThrow();
  });

  const workspaceBypassVectors: Array<[string, string]> = [
    ['a raw .. segment as the file path', '/api/v1/projects/proj-1/workspace/files/..'],
    ['a raw . segment as the file path', '/api/v1/projects/proj-1/workspace/files/.'],
    ['%2e%2e lowercase', '/api/v1/projects/proj-1/workspace/files/%2e%2e'],
    ['%2E%2E uppercase', '/api/v1/projects/proj-1/workspace/files/%2E%2E'],
    ['%2e%2E mixed case', '/api/v1/projects/proj-1/workspace/files/%2e%2E'],
    ['%2f encoded slash', '/api/v1/projects/proj-1/workspace/files/foo%2f..'],
    ['%5c encoded backslash', '/api/v1/projects/proj-1/workspace/files/foo%5c..'],
    ['a raw .. as the projectId itself', '/api/v1/projects/../workspace/files/notes.txt'],
    [
      'a raw .. deep in the file path, via a legitimate-looking project route',
      '/api/v1/projects/proj-1/workspace/files/../../agents',
    ],
    ['an empty trailing segment (a trailing slash)', '/api/v1/projects/proj-1/workspace/files/'],
  ];
  it.each(workspaceBypassVectors)('rejects, workspace shape, %s: %s', (_label, url) => {
    expect(() => pinBuiltUrl(url, workspaceShape, { min: 1 })).toThrow();
  });

  it('rejects a ? query string even when it would otherwise be a valid workspace URL', () => {
    expect(() =>
      pinBuiltUrl('/api/v1/projects/proj-1/workspace/files/notes.txt?x=1', workspaceShape, {
        min: 1,
      })
    ).toThrow();
  });

  it('rejects a # fragment even when it would otherwise be a valid workspace URL', () => {
    expect(() =>
      pinBuiltUrl('/api/v1/projects/proj-1/workspace/files/notes.txt#x', workspaceShape, {
        min: 1,
      })
    ).toThrow();
  });

  // The attachment shape is pinned to exactly one trailing segment — this is
  // the exact route an unsafe id can reach in production (an id of `..`
  // reaching `/api/v1/chat/attachments/..`, which normalizes to
  // `/api/v1/chat/`).
  const attachmentBypassVectors: Array<[string, string]> = [
    ['a raw .. id', '/api/v1/chat/attachments/..'],
    ['a raw . id', '/api/v1/chat/attachments/.'],
    ['%2e%2e id lowercase', '/api/v1/chat/attachments/%2e%2e'],
    ['%2E%2E id uppercase', '/api/v1/chat/attachments/%2E%2E'],
    ['an id with an encoded slash', '/api/v1/chat/attachments/a%2fb'],
    ['an id with an encoded backslash', '/api/v1/chat/attachments/a%5cb'],
    ['an id with a literal backslash', '/api/v1/chat/attachments/a\\b'],
    ['a ? query injected into the id position', '/api/v1/chat/attachments/att?../../x'],
    ['a # fragment injected into the id position', '/api/v1/chat/attachments/att#../../x'],
    ['an empty id (trailing slash)', '/api/v1/chat/attachments/'],
    ['two segments where exactly one id is pinned', '/api/v1/chat/attachments/a/b'],
  ];
  it.each(attachmentBypassVectors)('rejects, attachment shape, %s: %s', (_label, url) => {
    expect(() => pinBuiltUrl(url, attachmentShape, { min: 1, max: 1 })).toThrow();
  });

  const sharedDirBypassVectors: Array<[string, string]> = [
    ['a raw .. dirName', '/api/v1/projects/proj-1/shared-dirs/../files/notes.txt'],
    [
      'a raw .. deep in the file path',
      '/api/v1/projects/proj-1/shared-dirs/scratchpad/files/../../x',
    ],
    ['%2e%2e dirName', '/api/v1/projects/proj-1/shared-dirs/%2e%2e/files/notes.txt'],
  ];
  it.each(sharedDirBypassVectors)('rejects, shared-dir shape, %s: %s', (_label, url) => {
    expect(() => pinBuiltUrl(url, sharedDirShape, { min: 1 })).toThrow();
  });

  // A URL with no trailing segments at all beyond the fixed prefix — e.g. a
  // builder bug that drops the file path entirely — must be rejected by the
  // minimum-count half of the trailing-segment bound specifically, not by
  // any other check (the fixed prefix still matches exactly, no segment is
  // empty or a dot segment, and there is no query/fragment).
  it('rejects a URL with zero trailing segments when at least one is required', () => {
    expect(() =>
      pinBuiltUrl('/api/v1/projects/proj-1/workspace/files', workspaceShape, { min: 1 })
    ).toThrow();
  });

  // The defect this whole suite exists to close: a raw or %2e-spelled dot
  // segment that climbs out of the current project or shared directory and
  // back into a *different* one still normalizes, under `new URL()`, to a
  // route of the identical shape — same segment count, same literal
  // `workspace`/`files` (or `shared-dirs`) markers — so a pin that only
  // inspects the normalized pathname cannot tell it apart from a legitimate
  // request for that project. These tests pin the *expected* project/dir
  // value as an exact literal (as the real builders do); the literal
  // comparison itself is always true here (a builder compares its own
  // interpolated value against itself), so what actually rejects the
  // retarget is the raw per-segment dot check and the raw-vs-normalized
  // equality check inside `pinBuiltUrl`, not the literal match.
  describe('rejects a same-shape retargeting traversal (the pin checks the raw string, never a URL-normalized copy)', () => {
    const retargetingVectors: Array<[string, string, readonly (string | null)[]]> = [
      [
        'raw .. climbs out of the pinned project into another, workspace route',
        '/api/v1/projects/P/workspace/files/../../../OTHER/workspace/files/secret.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
      [
        '%2e%2e (any case) climbs out of the pinned project into another, workspace route',
        '/api/v1/projects/P/workspace/files/%2e%2e/%2E%2e/%2e%2e/OTHER/workspace/files/secret.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
      [
        'raw .. climbs out of the pinned shared directory into another',
        '/api/v1/projects/P/shared-dirs/d/files/../../OTHERDIR/files/x.txt',
        ['api', 'v1', 'projects', 'P', 'shared-dirs', 'd', 'files'],
      ],
      [
        '%2e%2e climbs out of the pinned shared directory into another',
        '/api/v1/projects/P/shared-dirs/d/files/%2e%2e/%2e%2e/OTHERDIR/files/x.txt',
        ['api', 'v1', 'projects', 'P', 'shared-dirs', 'd', 'files'],
      ],
      [
        'raw .. climbs out of the pinned project and back in — same project, same shape, but the traversal itself must still be rejected',
        '/api/v1/projects/P/workspace/files/a/../notes.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
      [
        'a lone . segment mid-path normalizes to the identical shape',
        '/api/v1/projects/P/workspace/files/a/./b.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
    ];
    it.each(retargetingVectors)('%s', (_label, url, shape) => {
      expect(() => pinBuiltUrl(url, shape, { min: 1 })).toThrow();
    });
  });

  // The WHATWG URL parser deletes every ASCII tab, CR and LF from its input
  // before resolving dot segments, so a raw `.` + tab/CR/LF + `.` (or the
  // %2e-encoded equivalent with a raw tab between the two encoded dots)
  // becomes `..` by the time `new URL()` returns a pathname — even though
  // `decodeURIComponent` on the raw segment leaves the tab/CR/LF character
  // exactly where it is, so it is never `.`/`..` and check 4's per-segment
  // dot comparison cannot see it. Only check 5 (the raw-vs-normalized
  // pathname comparison) catches this class.
  describe('rejects a control-character-split dot segment the raw per-segment check cannot see (only check 5 catches this)', () => {
    const controlSplitVectors: Array<[string, string, readonly (string | null)[]]> = [
      [
        'a raw tab climbs .\\t. out of the pinned project into another, workspace route',
        '/api/v1/projects/P/workspace/files/.\t./.\t./.\t./OTHER/workspace/files/secret.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
      [
        'a raw LF climbs .\\n. out of the pinned project into another, workspace route',
        '/api/v1/projects/P/workspace/files/.\n./.\n./.\n./OTHER/workspace/files/secret.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
      [
        'a raw CR climbs .\\r. out of the pinned project into another, workspace route',
        '/api/v1/projects/P/workspace/files/.\r./.\r./.\r./OTHER/workspace/files/secret.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
      [
        'a raw tab climbs .\\t. out of the pinned shared directory into another',
        '/api/v1/projects/P/shared-dirs/d/files/.\t./.\t./OTHERDIR/files/x.txt',
        ['api', 'v1', 'projects', 'P', 'shared-dirs', 'd', 'files'],
      ],
      [
        'a raw LF climbs .\\n. out of the pinned shared directory into another',
        '/api/v1/projects/P/shared-dirs/d/files/.\n./.\n./OTHERDIR/files/x.txt',
        ['api', 'v1', 'projects', 'P', 'shared-dirs', 'd', 'files'],
      ],
      [
        'a raw CR climbs .\\r. out of the pinned shared directory into another',
        '/api/v1/projects/P/shared-dirs/d/files/.\r./.\r./OTHERDIR/files/x.txt',
        ['api', 'v1', 'projects', 'P', 'shared-dirs', 'd', 'files'],
      ],
      [
        '%2e + raw tab + %2e climbs out of the pinned project into another, workspace route',
        '/api/v1/projects/P/workspace/files/%2e\t%2e/%2e\t%2e/%2e\t%2e/OTHER/workspace/files/secret.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
      [
        'a lone .\\t segment (single-dot elision after tab stripping) changes the pathname shape',
        '/api/v1/projects/P/workspace/files/.\t/OTHER/workspace/files/secret.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
      [
        'a lone \\t. segment (single-dot elision after tab stripping) changes the pathname shape',
        '/api/v1/projects/P/workspace/files/\t./OTHER/workspace/files/secret.txt',
        ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
      ],
    ];
    it.each(controlSplitVectors)('%s', (_label, url, shape) => {
      expect(() => pinBuiltUrl(url, shape, { min: 1 })).toThrow();
    });

    // A trailing tab or space on the *whole* built URL is stripped/trimmed by
    // `new URL()` (the WHATWG parser also trims leading/trailing C0-control-
    // or-space characters from the input as a whole), so the normalized
    // pathname loses that character while the raw path still has it — a
    // mismatch only check 5 can observe, since check 4 only inspects
    // individual segments already split on `/`.
    it('rejects a URL with a trailing tab that the whole-input trim strips', () => {
      expect(() =>
        pinBuiltUrl(
          '/api/v1/projects/P/workspace/files/x.txt\t',
          ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
          { min: 1 }
        )
      ).toThrow();
    });

    it('rejects a URL with a trailing space that the whole-input trim strips', () => {
      expect(() =>
        pinBuiltUrl(
          '/api/v1/projects/P/workspace/files/x.txt ',
          ['api', 'v1', 'projects', 'P', 'workspace', 'files'],
          { min: 1 }
        )
      ).toThrow();
    });
  });

  // The raw `#` scan (the first half of check 1) is what rejects a trailing
  // fragment delimiter specifically: an empty fragment normalizes to a
  // pathname identical to the raw path with search/hash both empty, so
  // check 5 cannot tell it apart from a legitimate URL with no fragment at
  // all — only the raw scan for a literal `#` in the string catches it.
  it('rejects a URL with a trailing # that check 5 alone cannot distinguish from a legitimate URL', () => {
    expect(() =>
      pinBuiltUrl(
        '/api/v1/projects/proj-1/workspace/files/notes.txt#',
        ['api', 'v1', 'projects', 'proj-1', 'workspace', 'files'],
        { min: 1 }
      )
    ).toThrow();
  });

  it('rejects an attachment URL with a trailing # that check 5 alone cannot distinguish from a legitimate URL', () => {
    expect(() =>
      pinBuiltUrl('/api/v1/chat/attachments/att-1#', attachmentShape, { min: 1, max: 1 })
    ).toThrow();
  });
});

// These tests call `buildFileApiUrlPinOnly`/`buildAttachmentApiUrlPinOnly`
// directly — the real URL-assembly-plus-pin logic the public builders
// delegate to, but with the source-side segment-safety validation skipped
// entirely (as if it had a gap, or a future caller forgot to call it). This
// proves the sink pin alone — not `hasOnlySafeSingleSegment`/
// `hasOnlySafePathSegments` — is what rejects each bypass class through the
// real builders, not just through hand-built strings passed to `pinBuiltUrl`
// in isolation.
describe('the pin alone, through the real builders, with source validation skipped', () => {
  // Note: a value like `%2e%2e` or `a%2fb` is *not* a useful vector for
  // these builder-level tests specifically — `encodeFilePath`/
  // `encodeURIComponent` re-encode the literal `%` in a string the caller
  // already spelled as percent-encoded, producing an inert, double-encoded
  // segment (`%252e%252e`) that decodes only once, server-side, to a
  // harmless literal filename — not a traversal. Those vectors are already
  // covered directly against `pinBuiltUrl` above, with a hand-built raw
  // string standing in for whatever produced it. Here, only vectors that
  // survive the builder's own encoding step unencoded are meaningful: a raw
  // `.`/`..` (both are in `encodeURIComponent`'s unreserved set and pass
  // through untouched) and a raw backslash (which *does* get encoded, then
  // decoded back by the pin, so it's still a real vector here too).
  const unsafeWorkspaceFilePaths: Array<[string, string]> = [
    ['a raw .. segment', '../secret'],
    ['a raw . segment', './secret'],
    ['a literal backslash', 'foo\\secret'],
    ['empty', ''],
    ['a same-shape cross-project retarget', '../../../OTHER-PROJECT/workspace/files/secret.txt'],
  ];
  it.each(unsafeWorkspaceFilePaths)(
    'buildFileApiUrlPinOnly, workspace, rejects %s: %s',
    (_label, filePath) => {
      expect(() => buildFileApiUrlPinOnly('proj-1', { kind: 'workspace', filePath })).toThrow();
    }
  );

  const unsafeSharedDirFilePaths: Array<[string, string]> = [
    ['a raw .. segment', '../secret'],
    ['a same-shape cross-dir retarget', '../../OTHER-DIR/files/secret.txt'],
  ];
  it.each(unsafeSharedDirFilePaths)(
    'buildFileApiUrlPinOnly, shared-dir filePath, rejects %s: %s',
    (_label, filePath) => {
      expect(() =>
        buildFileApiUrlPinOnly('proj-1', { kind: 'shared-dir', dirName: 'scratchpad', filePath })
      ).toThrow();
    }
  );

  const unsafeDirNames: Array<[string, string]> = [['a raw .. dirName', '..']];
  it.each(unsafeDirNames)(
    'buildFileApiUrlPinOnly, shared-dir dirName, rejects %s: %s',
    (_label, dirName) => {
      expect(() =>
        buildFileApiUrlPinOnly('proj-1', { kind: 'shared-dir', dirName, filePath: 'notes.txt' })
      ).toThrow();
    }
  );

  const unsafeProjectIds: Array<[string, string]> = [['a raw .. projectId', '..']];
  it.each(unsafeProjectIds)(
    'buildFileApiUrlPinOnly, workspace, rejects %s: %s',
    (_label, projectId) => {
      expect(() =>
        buildFileApiUrlPinOnly(projectId, { kind: 'workspace', filePath: 'notes.txt' })
      ).toThrow();
    }
  );

  const unsafeAttachmentIds: Array<[string, string]> = [
    ['a raw .. id', '..'],
    ['a raw . id', '.'],
    ['a literal backslash', 'a\\b'],
    ['empty', ''],
  ];
  it.each(unsafeAttachmentIds)('buildAttachmentApiUrlPinOnly rejects %s: %s', (_label, id) => {
    expect(() => buildAttachmentApiUrlPinOnly(id)).toThrow();
  });

  it('buildFileApiUrlPinOnly still returns a normal URL for a safe workspace target (sanity)', () => {
    expect(buildFileApiUrlPinOnly('proj-1', { kind: 'workspace', filePath: 'src/main.go' })).toBe(
      '/api/v1/projects/proj-1/workspace/files/src/main.go'
    );
  });

  it('buildAttachmentApiUrlPinOnly still returns a normal URL for a safe id (sanity)', () => {
    expect(buildAttachmentApiUrlPinOnly('att-123')).toBe('/api/v1/chat/attachments/att-123');
  });
});

describe('buildAttachmentApiUrl', () => {
  it('builds the attachment URL', () => {
    expect(buildAttachmentApiUrl('att-123')).toBe('/api/v1/chat/attachments/att-123');
  });

  it('encodes special characters in the id', () => {
    expect(buildAttachmentApiUrl('att with space')).toBe(
      '/api/v1/chat/attachments/att%20with%20space'
    );
  });

  it('the built pathname never leaves the attachments endpoint, for a normal id', () => {
    const url = buildAttachmentApiUrl('att-123');
    const pathname = new URL(url, 'http://example.test').pathname;
    expect(pathname).toBe('/api/v1/chat/attachments/att-123');
  });

  // An attachment id occupies exactly one route segment, same as a project
  // id, and needs the identical reject-then-pin treatment.
  const unsafeIds: Array<[string, string]> = [
    ['a lone . segment', '.'],
    ['a lone .. segment', '..'],
    ['%2e%2e lowercase', '%2e%2e'],
    ['%2E%2E uppercase', '%2E%2E'],
    ['an embedded slash', 'a/b'],
    ['a query delimiter', 'a?b'],
    ['a fragment delimiter', 'a#b'],
    ['an encoded slash', 'a%2fb'],
    ['an encoded backslash', 'a%5cb'],
    ['a literal backslash', 'a\\b'],
    ['empty', ''],
  ];
  it.each(unsafeIds)(
    'throws rather than building a URL for an unsafe id — %s: %s',
    (_label, id) => {
      expect(() => buildAttachmentApiUrl(id)).toThrow();
    }
  );
});

describe('parseContainerPath — traversal, encoding and injection rejection', () => {
  const rejected: Array<[string, string]> = [
    ['.. climbing out of workspace', '/workspace/../../agents'],
    ['.. inside a shared-dir file path', '/scion-volumes/scratchpad/../../agents.txt'],
    ['.. as the shared-dir name itself', '/scion-volumes/../etc/passwd.txt'],
    [
      '.. inside the in-workspace shared-dir spelling',
      '/workspace/.scion-volumes/data/../secret.txt',
    ],
    // Distinct from the case above: here the dirName *itself* (not a
    // segment further down the file path) is the traversal segment, in the
    // in-workspace shared-dir spelling specifically. Both this branch and
    // the /scion-volumes/{dir} branch reject it today; this case exists so a
    // future change that drops just this branch's own dirName check (as
    // opposed to the other branch's, which the case above already covers)
    // is still caught.
    [
      '.. as the dirName in the in-workspace shared-dir spelling',
      '/workspace/.scion-volumes/../etc/passwd.txt',
    ],
    ['%2e%2e lowercase', '/workspace/%2e%2e/agents.txt'],
    ['%2E%2E uppercase', '/workspace/%2E%2E/agents.txt'],
    ['%2f encoded slash', '/workspace/foo%2f..%2fagents.txt'],
    ['%5c encoded backslash', '/workspace/foo%5c..%5cagents.txt'],
    // No `..` alongside the delimiter here — a vector that mixes both would
    // still be rejected by the dot-segment rule alone even if the `?`/`#`
    // rejection itself were broken, masking the very thing this case exists
    // to isolate.
    ['a ? query delimiter (no ..)', '/workspace/notes.txt?x'],
    ['a # fragment delimiter (no ..)', '/workspace/notes.txt#x'],
    ['a bare directory reference', '/scion-volumes/scratchpad'],
  ];
  it.each(rejected)('rejects %s: %s', (_label, path) => {
    expect(parseContainerPath(path)).toBeNull();
  });
});

describe('identity keys', () => {
  it('attachmentIdentityKey encodes a stable tuple', () => {
    expect(attachmentIdentityKey('att-1')).toBe(JSON.stringify(['attachment', 'att-1']));
  });

  it('pathIdentityKey encodes project + kind + dirName + filePath', () => {
    const key = pathIdentityKey('proj-1', {
      kind: 'shared-dir',
      dirName: 'scratchpad',
      filePath: 'a.md',
    });
    expect(key).toBe(JSON.stringify(['path', 'proj-1', 'shared-dir', 'scratchpad', 'a.md']));
  });

  it('a workspace target has an empty dirName slot', () => {
    const key = pathIdentityKey('proj-1', { kind: 'workspace', filePath: 'a.md' });
    expect(key).toBe(JSON.stringify(['path', 'proj-1', 'workspace', '', 'a.md']));
  });

  it('two shared-dir path spellings that parse to the same target share identity', () => {
    const a = parseContainerPath('/scion-volumes/scratchpad/a.md')!;
    const b = parseContainerPath('/workspace/.scion-volumes/scratchpad/a.md')!;
    expect(pathIdentityKey('proj-1', a)).toBe(pathIdentityKey('proj-1', b));
  });

  it('the same file path in two different projects has different identity', () => {
    const loc = parseContainerPath('/workspace/a.md')!;
    expect(pathIdentityKey('proj-1', loc)).not.toBe(pathIdentityKey('proj-2', loc));
  });
});

describe('extensionOf / isImageFileName / isMarkdownFileName', () => {
  it('extensionOf returns the lowercased extension with dot', () => {
    expect(extensionOf('Notes.MD')).toBe('.md');
  });

  it('extensionOf returns empty string for a name with no extension', () => {
    expect(extensionOf('Makefile')).toBe('');
  });

  it('a leading dot alone is not treated as an extension', () => {
    expect(extensionOf('.gitignore')).toBe('');
  });

  it('isImageFileName is true for a known image extension', () => {
    expect(isImageFileName('shot.png')).toBe(true);
  });

  it('isImageFileName is false for a non-image extension', () => {
    expect(isImageFileName('notes.md')).toBe(false);
  });

  it('isMarkdownFileName is true for .md and .markdown', () => {
    expect(isMarkdownFileName('a.md')).toBe(true);
    expect(isMarkdownFileName('a.markdown')).toBe(true);
  });

  it('isMarkdownFileName is false for other extensions', () => {
    expect(isMarkdownFileName('a.txt')).toBe(false);
  });
});

describe('isGcsImageExtension', () => {
  it.each(['pic.png', 'pic.jpg', 'pic.jpeg', 'pic.gif', 'pic.webp'])(
    'is true for %s (one of the four hub-sniffed raster types)',
    (name) => {
      expect(isGcsImageExtension(name)).toBe(true);
    }
  );

  it('is false for .svg, even though isImageFileName treats it as an image', () => {
    expect(isGcsImageExtension('pic.svg')).toBe(false);
    expect(isImageFileName('pic.svg')).toBe(true);
  });

  it('is false for .bmp and .ico, even though isImageFileName treats them as images', () => {
    expect(isGcsImageExtension('pic.bmp')).toBe(false);
    expect(isGcsImageExtension('pic.ico')).toBe(false);
  });

  it('is false for a non-image extension', () => {
    expect(isGcsImageExtension('notes.md')).toBe(false);
  });

  it('is case-insensitive, matching extensionOf', () => {
    expect(isGcsImageExtension('PIC.PNG')).toBe(true);
  });
});

describe('isGcsImageContentType', () => {
  it.each(['image/png', 'image/jpeg', 'image/gif', 'image/webp'])(
    'is true for %s (one of the four hub-sniffed raster types)',
    (mime) => {
      expect(isGcsImageContentType(mime)).toBe(true);
    }
  );

  it.each(['image/bmp', 'image/x-icon', 'image/svg+xml', 'image/avif', 'image/tiff'])(
    'is false for %s, another image type',
    (mime) => {
      expect(isGcsImageContentType(mime)).toBe(false);
    }
  );

  it.each(['text/plain', 'application/octet-stream', ''])('is false for %j', (mime) => {
    expect(isGcsImageContentType(mime)).toBe(false);
  });
});

describe('isLikelyTextFileName / isLikelyTextMime', () => {
  it('isLikelyTextFileName is true for a known text/code extension', () => {
    expect(isLikelyTextFileName('main.go')).toBe(true);
    expect(isLikelyTextFileName('data.json')).toBe(true);
    expect(isLikelyTextFileName('notes.txt')).toBe(true);
  });

  it('isLikelyTextFileName is true for Markdown (delegates to isMarkdownFileName)', () => {
    expect(isLikelyTextFileName('readme.md')).toBe(true);
  });

  it('isLikelyTextFileName is true for a well-known extensionless name', () => {
    expect(isLikelyTextFileName('Dockerfile')).toBe(true);
    expect(isLikelyTextFileName('Makefile')).toBe(true);
  });

  it('isLikelyTextFileName is false for an unrecognized extension', () => {
    expect(isLikelyTextFileName('bundle.zip')).toBe(false);
    expect(isLikelyTextFileName('archive.tar.gz')).toBe(false);
  });

  it('isLikelyTextFileName is false for an unrecognized extensionless name', () => {
    expect(isLikelyTextFileName('notes')).toBe(false);
  });

  it('isLikelyTextFileName is false for a name with an unrecognized extension, even if its basename is a well-known extensionless name', () => {
    // A name with an extension is never treated as "well-known extensionless"
    // even if its basename-without-extension happens to collide with one.
    expect(isLikelyTextFileName('README.bin')).toBe(false);
    expect(isLikelyTextFileName('a.out')).toBe(false);
  });

  it('isLikelyTextMime is true for any text/* MIME type', () => {
    expect(isLikelyTextMime('text/plain')).toBe(true);
    expect(isLikelyTextMime('text/csv')).toBe(true);
  });

  it('isLikelyTextMime is true for a known text-like application/* MIME type', () => {
    expect(isLikelyTextMime('application/json')).toBe(true);
    expect(isLikelyTextMime('application/xml')).toBe(true);
  });

  it('isLikelyTextMime is true for a structured-syntax +json or +xml suffix, regardless of top-level type', () => {
    expect(isLikelyTextMime('application/ld+json')).toBe(true);
    expect(isLikelyTextMime('image/svg+xml')).toBe(true);
    expect(isLikelyTextMime('application/atom+xml')).toBe(true);
  });

  it('isLikelyTextMime is false for a binary MIME type', () => {
    expect(isLikelyTextMime('application/zip')).toBe(false);
    expect(isLikelyTextMime('application/octet-stream')).toBe(false);
    expect(isLikelyTextMime('image/png')).toBe(false);
  });

  it('isLikelyTextMime is case-insensitive', () => {
    expect(isLikelyTextMime('APPLICATION/JSON')).toBe(true);
  });

  it('isLikelyTextMime ignores a trailing charset parameter', () => {
    expect(isLikelyTextMime('text/plain; charset=utf-8')).toBe(true);
  });
});

describe('baseMimeType', () => {
  it('lowercases the MIME type', () => {
    expect(baseMimeType('APPLICATION/OCTET-STREAM')).toBe('application/octet-stream');
  });

  it('strips a trailing parameter', () => {
    expect(baseMimeType('application/octet-stream; charset=binary')).toBe(
      'application/octet-stream'
    );
  });

  it('trims surrounding whitespace around the base type', () => {
    expect(baseMimeType('  application/octet-stream  ')).toBe('application/octet-stream');
  });

  it('is empty for an empty or parameter-only input', () => {
    expect(baseMimeType('')).toBe('');
    expect(baseMimeType(';charset=x')).toBe('');
  });
});

describe('isLikelyBinaryFileName', () => {
  it('is true for a known archive extension', () => {
    expect(isLikelyBinaryFileName('bundle.zip')).toBe(true);
    expect(isLikelyBinaryFileName('archive.tar.gz')).toBe(true);
  });

  it('is true for a known executable/compiled extension', () => {
    expect(isLikelyBinaryFileName('app.exe')).toBe(true);
    expect(isLikelyBinaryFileName('lib.so')).toBe(true);
  });

  it.each([
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
  ])('is true for the known audio, video, font or disk-image extension %s', (ext) => {
    expect(isLikelyBinaryFileName(`file${ext}`)).toBe(true);
  });

  it('is true for a known audio/video/font/disk-image extension regardless of case', () => {
    expect(isLikelyBinaryFileName('CLIP.MP3')).toBe(true);
    expect(isLikelyBinaryFileName('Icon.WOFF2')).toBe(true);
  });

  it('is false for ordinary text/code files that no allow-list enumerates', () => {
    expect(isLikelyBinaryFileName('.gitignore')).toBe(false);
    expect(isLikelyBinaryFileName('go.mod')).toBe(false);
    expect(isLikelyBinaryFileName('Main.vue')).toBe(false);
    expect(isLikelyBinaryFileName('main.swift')).toBe(false);
    expect(isLikelyBinaryFileName('init.lua')).toBe(false);
    expect(isLikelyBinaryFileName('main.tf')).toBe(false);
  });

  it('is false for a name with no extension at all', () => {
    expect(isLikelyBinaryFileName('Makefile')).toBe(false);
    expect(isLikelyBinaryFileName('README')).toBe(false);
  });
});

describe('resolveMessageProjectId', () => {
  it('a non-DM thread prefers the thread project id', () => {
    expect(
      resolveMessageProjectId({
        isDM: false,
        threadProjectId: 'proj-thread',
        senderProjectId: 'proj-sender',
      })
    ).toBe('proj-thread');
  });

  it('a non-DM thread falls back to the message project when the thread has none', () => {
    expect(
      resolveMessageProjectId({ isDM: false, threadProjectId: '', messageProjectId: 'proj-msg' })
    ).toBe('proj-msg');
  });

  it('a DM never uses the thread project id, even when present', () => {
    expect(
      resolveMessageProjectId({
        isDM: true,
        threadProjectId: 'proj-inherited',
        senderProjectId: 'proj-sender',
      })
    ).toBe('proj-sender');
  });

  it('a DM prefers senderProjectId over messageProjectId', () => {
    expect(
      resolveMessageProjectId({
        isDM: true,
        threadProjectId: '',
        senderProjectId: 'proj-sender',
        messageProjectId: 'proj-msg',
      })
    ).toBe('proj-sender');
  });

  it('a DM falls back to messageProjectId when senderProjectId is absent', () => {
    expect(
      resolveMessageProjectId({ isDM: true, threadProjectId: '', messageProjectId: 'proj-msg' })
    ).toBe('proj-msg');
  });

  it('a DM falls back to the peer agent project when the message has no project of its own', () => {
    expect(
      resolveMessageProjectId({ isDM: true, threadProjectId: '', peerAgentProjectId: 'proj-peer' })
    ).toBe('proj-peer');
  });

  it('a DM prefers the message project over the cached peer-agent project', () => {
    expect(
      resolveMessageProjectId({
        isDM: true,
        threadProjectId: '',
        senderProjectId: 'proj-msg',
        peerAgentProjectId: 'proj-peer',
      })
    ).toBe('proj-msg');
  });

  it('a DM with nothing resolvable returns empty (never guesses the inherited project)', () => {
    expect(resolveMessageProjectId({ isDM: true, threadProjectId: 'proj-inherited' })).toBe('');
  });
});

// ---------------------------------------------------------------------------
// gs:// link detection, parsing and URL construction.
// ---------------------------------------------------------------------------

/**
 * Runs GCS_URI_PATTERN once against body and returns the first match's
 * bucket and its actually-extracted object (via {@link resolveGcsMatch}, not
 * the pattern's raw group 3), or null if there is no match or this
 * occurrence extracts no valid object at all.
 */
function firstGcsMatch(body: string): { bucket: string; object: string } | null {
  const re = new RegExp(GCS_URI_PATTERN.source, GCS_URI_PATTERN.flags);
  const m = re.exec(body);
  if (!m) return null;
  const resolved = resolveGcsMatch(m);
  if (!resolved) return null;
  return { bucket: m[2], object: resolved.object };
}

/**
 * Runs GCS_URI_PATTERN over the whole body and returns every occurrence
 * that resolves to a valid object, in order — unlike {@link firstGcsMatch},
 * which only ever looks at the first. Needed to assert "linked" for a body
 * with more than one independent gs:// occurrence, where checking only the
 * first match cannot prove anything about the second.
 */
function allGcsMatches(body: string): Array<{ bucket: string; object: string }> {
  const re = new RegExp(GCS_URI_PATTERN.source, GCS_URI_PATTERN.flags);
  const results: Array<{ bucket: string; object: string }> = [];
  let m: RegExpExecArray | null;
  let prevIndex = 0;
  while ((m = re.exec(body)) !== null) {
    const resolved = resolveGcsMatch(m);
    if (resolved) {
      results.push({ bucket: m[2], object: resolved.object });
    }
    if (re.lastIndex === prevIndex) break;
    prevIndex = re.lastIndex;
  }
  return results;
}

/**
 * Shared parity vector table: one table of {body, bucket, object, linked,
 * serverAllowed} cases, present verbatim in this file and in
 * pkg/hub/gcs_link_test.go. The `name` field cross-references the matching row in
 * pkg/hub/gcs_link_test.go's gcsParityVectors — the two files list the same
 * cases under the same names. `serverAllowed` documents the value the Go
 * test asserts (bodyReferencesGCSURI); only `linked` is exercised here.
 */
const GCS_PARITY_VECTORS: Array<{
  name: string;
  body: string;
  bucket: string;
  object: string;
  linked: boolean;
  serverAllowed: boolean;
}> = [
  // Required parity vector: a bucket name containing a hyphenated
  // cross-project-style segment, to prove the bucket pattern accepts it.
  {
    name: 'cross-project-exchange-uri',
    body: 'see gs://scion-xproject-exchange/workspace-volumes/dev-brief.md',
    bucket: 'scion-xproject-exchange',
    object: 'workspace-volumes/dev-brief.md',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'exact-object',
    body: 'gs://bkt/abc.md',
    bucket: 'bkt',
    object: 'abc.md',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'prefix-single-char',
    body: 'gs://bkt/abc.md',
    bucket: 'bkt',
    object: 'a',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'prefix-stem',
    body: 'gs://bkt/abc.md',
    bucket: 'bkt',
    object: 'abc',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'prefix-partial-extension',
    body: 'gs://bkt/abc.md',
    bucket: 'bkt',
    object: 'abc.m',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'scheme-prefixed-not-a-link',
    body: 'xgs://bkt/o',
    bucket: 'bkt',
    object: 'o',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'leading-slash-not-a-link',
    body: '/gs://bkt/o',
    bucket: 'bkt',
    object: 'o',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'parenthesized-trailing-dot',
    body: '(gs://bkt/o.md).',
    bucket: 'bkt',
    object: 'o.md',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'nested-workspace-path-no-collision',
    body: 'gs://bkt/workspace/x.md, ok',
    bucket: 'bkt',
    object: 'workspace/x.md',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'trailing-slash-directory',
    body: 'gs://bkt/dir/',
    bucket: 'bkt',
    object: 'dir',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'backtick-wrapped',
    body: '`gs://scion-xproject-exchange/workspace-volumes/dev-brief.md`',
    bucket: 'scion-xproject-exchange',
    object: 'workspace-volumes/dev-brief.md',
    linked: true,
    serverAllowed: true,
  },
  // A trailing underscore is a word character, so it blocks the right
  // boundary exactly like a trailing letter would: the real link in this
  // body is "abc.md_extra", not "abc.md".
  {
    name: 'underscore-blocks-right-boundary',
    body: 'gs://bkt/abc.md_extra',
    bucket: 'bkt',
    object: 'abc.md',
    linked: false,
    serverAllowed: false,
  },
  // A leading underscore likewise blocks the left boundary.
  {
    name: 'underscore-blocks-left-boundary',
    body: 'x_gs://bkt/o.txt',
    bucket: 'bkt',
    object: 'o.txt',
    linked: false,
    serverAllowed: false,
  },
  // A trailing run of `~+=@%` is part of the object, not a boundary — the
  // full object links and is allowed; a truncated form of the same posted
  // text is denied.
  {
    name: 'trailing-tilde-full-object',
    body: 'gs://bkt/secret~',
    bucket: 'bkt',
    object: 'secret~',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'trailing-tilde-truncated-object',
    body: 'gs://bkt/secret~',
    bucket: 'bkt',
    object: 'secret',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'trailing-plus-full-object',
    body: 'gs://bkt/data+',
    bucket: 'bkt',
    object: 'data+',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'trailing-plus-truncated-object',
    body: 'gs://bkt/data+',
    bucket: 'bkt',
    object: 'data',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'trailing-double-equals-full-object',
    body: 'gs://bkt/key==',
    bucket: 'bkt',
    object: 'key==',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'trailing-double-equals-truncated-object',
    body: 'gs://bkt/key==',
    bucket: 'bkt',
    object: 'key',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'trailing-percent-full-object',
    body: 'gs://bkt/report%',
    bucket: 'bkt',
    object: 'report%',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'trailing-percent-truncated-object',
    body: 'gs://bkt/report%',
    bucket: 'bkt',
    object: 'report',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'trailing-at-full-object',
    body: 'gs://bkt/a@',
    bucket: 'bkt',
    object: 'a@',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'trailing-at-truncated-object',
    body: 'gs://bkt/a@',
    bucket: 'bkt',
    object: 'a',
    linked: false,
    serverAllowed: false,
  },
  // A fragment-, query- or param-like continuation immediately after the
  // object — '#', '?' or '&' followed by a further non-whitespace
  // character — is a full reject, not a truncation.
  {
    name: 'fragment-continuation-hash',
    body: 'gs://bkt/a#frag',
    bucket: 'bkt',
    object: 'a',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'fragment-continuation-query',
    body: 'gs://bkt/a?x=1',
    bucket: 'bkt',
    object: 'a',
    linked: false,
    serverAllowed: false,
  },
  // The posted text is the same "a&b" as the Go row of this name, but here
  // it is written pre-escaped, matching what this function actually
  // consumes: production always HTML-escapes a message body before
  // GCS_URI_PATTERN ever runs on it, so a raw '&' is never in this table's
  // input as a bare byte — it is a raw '&' the moment it reaches the Go
  // side's bodyReferencesGCSURI (see the exact same row in gcs_link_test.go).
  {
    name: 'fragment-continuation-amp',
    body: 'gs://bkt/a&amp;b',
    bucket: 'bkt',
    object: 'a',
    linked: false,
    serverAllowed: false,
  },
  // A trailing '?' at the very end of a sentence (nothing after it, or only
  // whitespace) is not a continuation — it links normally, truncated at '?'
  // exactly like a period or comma would be.
  {
    name: 'trailing-question-end-of-sentence',
    body: 'see gs://bkt/o.md?',
    bucket: 'bkt',
    object: 'o.md',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'double-quoted-wrapping',
    body: '"gs://bkt/o.md"',
    bucket: 'bkt',
    object: 'o.md',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'parenthesized-wrapping',
    body: '(gs://bkt/o.md)',
    bucket: 'bkt',
    object: 'o.md',
    linked: true,
    serverAllowed: true,
  },
  // A name containing whitespace links, and is served, only up to the
  // whitespace — an accepted edge, not a truncation bug: the agent posted
  // this text, and only a viewer of this message can fetch it, so this does
  // not widen access beyond what renders.
  {
    name: 'space-in-name-accepted-edge',
    body: 'gs://bkt/my file.txt',
    bucket: 'bkt',
    object: 'my',
    linked: true,
    serverAllowed: true,
  },
  // A gs:// URI nested inside another URI's object run belongs entirely to
  // the outer candidate's raw run — the atomic `(?=(...))\3` group consumes
  // the whole run in one match, so the inner occurrence, including one
  // naming a different bucket, is never a candidate on its own.
  {
    name: 'nested-tilde-inner-never-a-candidate',
    body: 'gs://b1b/a~gs://b1b/c',
    bucket: 'b1b',
    object: 'c',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'nested-tilde-outer-allowed',
    body: 'gs://b1b/a~gs://b1b/c',
    bucket: 'b1b',
    object: 'a~gs://b1b/c',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'nested-comma-inner-never-a-candidate',
    body: 'gs://b1b/a,gs://b1b/c',
    bucket: 'b1b',
    object: 'c',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'nested-comma-outer-allowed',
    body: 'gs://b1b/a,gs://b1b/c',
    bucket: 'b1b',
    object: 'a,gs://b1b/c',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'nested-cross-bucket-inner-never-a-candidate',
    body: 'gs://pub/x=gs://sec/key',
    bucket: 'sec',
    object: 'key',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'nested-cross-bucket-outer-allowed',
    body: 'gs://pub/x=gs://sec/key',
    bucket: 'pub',
    object: 'x=gs://sec/key',
    linked: true,
    serverAllowed: true,
  },
  // Two independent, space-separated occurrences: each is its own
  // candidate, both allowed — this is not the nested case above.
  {
    name: 'multiple-independent-occurrences-first',
    body: 'gs://bkt/a gs://bkt/b',
    bucket: 'bkt',
    object: 'a',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'multiple-independent-occurrences-second',
    body: 'gs://bkt/a gs://bkt/b',
    bucket: 'bkt',
    object: 'b',
    linked: true,
    serverAllowed: true,
  },
  // Applied directly to this raw, unrendered text (no markdown escape
  // resolution happens here), the client's object-class run simply stops
  // at the backslash, the same as it would at whitespace, and links
  // "secret" — a third value, distinct from both the server's unconditional
  // void (serverAllowed: false for every object) and what the client
  // actually links once this text has gone through real markdown
  // rendering ("secret_v2", pinned separately in gcs-link.pw.ts, where the
  // backslash escape is already resolved before the linkifier ever runs).
  {
    name: 'backslash-voids-candidate',
    body: 'gs://bkt/secret\\_v2',
    bucket: 'bkt',
    object: 'secret',
    linked: true,
    serverAllowed: false,
  },
  // The server's void-on-backslash rule covers the unescaped object too,
  // not only the truncated one: neither "secret" nor "secret_v2" is ever
  // authorized from this raw body. The client can never produce
  // "secret_v2" by applying the regex directly to raw, unrendered text
  // (only real markdown rendering resolves the escape), so this is false
  // here on both sides, for different reasons.
  {
    name: 'backslash-voids-candidate-unescaped-object',
    body: 'gs://bkt/secret\\_v2',
    bucket: 'bkt',
    object: 'secret_v2',
    linked: false,
    serverAllowed: false,
  },
  // A bucket not immediately followed by '/' is not a valid occurrence at
  // all: the whole match fails, not just a shorter bucket. Regression guard
  // for the scan that replays the client's global match on the server
  // (dropping this guard would let the server treat "bkt" as a bucket and
  // extract an object starting right after the ':' or '~').
  {
    name: 'bucket-not-followed-by-slash-colon',
    body: 'gs://bkt:o.txt',
    bucket: 'bkt',
    object: 'o.txt',
    linked: false,
    serverAllowed: false,
  },
  {
    name: 'bucket-not-followed-by-slash-tilde',
    body: 'gs://bkt~o.txt',
    bucket: 'bkt',
    object: 'o.txt',
    linked: false,
    serverAllowed: false,
  },
  // A question mark followed by more text, itself followed by whitespace,
  // is not a continuation.
  {
    name: 'trailing-question-then-whitespace',
    body: 'is it gs://bkt/o.md? next',
    bucket: 'bkt',
    object: 'o.md',
    linked: true,
    serverAllowed: true,
  },
  // Trailing sentence punctuation trimmed from the object is what the
  // continuation check must see next, not the character after that
  // punctuation.
  {
    name: 'trimmed-punct-then-fragment-hash',
    body: 'gs://bkt/a.#frag',
    bucket: 'bkt',
    object: 'a',
    linked: true,
    serverAllowed: true,
  },
  {
    name: 'trimmed-punct-then-fragment-query',
    body: 'gs://bkt/a,?x',
    bucket: 'bkt',
    object: 'a',
    linked: true,
    serverAllowed: true,
  },
];

describe('GCS_PARITY_VECTORS (cross-referenced with pkg/hub/gcs_link_test.go)', () => {
  for (const v of GCS_PARITY_VECTORS) {
    it(`${v.name}: linked=${v.linked}`, () => {
      // allGcsMatches, not firstGcsMatch: a row like *-inner-never-a-candidate
      // asserts something about the SECOND occurrence in its body, which
      // checking only the first match can never disprove.
      const linked = allGcsMatches(v.body).some(
        (m) => m.bucket === v.bucket && m.object === v.object
      );
      expect(linked).toBe(v.linked);
    });
  }
});

describe('GCS_URI_PATTERN', () => {
  it('links a gs:// URI at the start of the body', () => {
    const re = new RegExp(GCS_URI_PATTERN.source, GCS_URI_PATTERN.flags);
    const m = re.exec('gs://bkt/o.txt rest');
    expect(m).not.toBeNull();
    expect(m![1]).toBe('');
    expect(m![2]).toBe('bkt');
    expect(resolveGcsMatch(m!)).toEqual({ object: 'o.txt', suffix: '' });
  });

  it('links a gs:// URI directly after a </code> boundary char', () => {
    const re = new RegExp(GCS_URI_PATTERN.source, GCS_URI_PATTERN.flags);
    const m = re.exec('>gs://bkt/o.txt');
    expect(m).not.toBeNull();
    expect(m![1]).toBe('>');
  });

  it('excludes trailing punctuation: comma, period, closing paren', () => {
    const re = new RegExp(GCS_URI_PATTERN.source, GCS_URI_PATTERN.flags);
    const m = re.exec('gs://bkt/o.txt, next');
    expect(m).not.toBeNull();
    // Group 3 is the raw run (includes the comma); resolveGcsMatch trims it.
    expect(m![3]).toBe('o.txt,');
    expect(resolveGcsMatch(m!)).toEqual({ object: 'o.txt', suffix: ',' });
  });

  it('does not link a bucket shorter than 3 characters', () => {
    const re = new RegExp(GCS_URI_PATTERN.source, GCS_URI_PATTERN.flags);
    expect(re.exec('gs://ab/o.txt')).toBeNull();
  });

  it('never contains a lookbehind assertion', () => {
    expect(GCS_URI_PATTERN.source).not.toMatch(/\(\?<[=!]/);
  });
});

describe('resolveGcsMatch (exact extraction, not a widened reject set)', () => {
  function resolve(body: string): { object: string; suffix: string } | null {
    const re = new RegExp(GCS_URI_PATTERN.source, GCS_URI_PATTERN.flags);
    const m = re.exec(body);
    return m ? resolveGcsMatch(m) : null;
  }

  it.each([
    ['secret~', 'secret~'],
    ['data+', 'data+'],
    ['key==', 'key=='],
    ['report%', 'report%'],
    ['a@', 'a@'],
  ])('links the full object %s, trailing final-class run included', (objectPart, expected) => {
    expect(resolve(`gs://bkt/${objectPart}`)).toEqual({ object: expected, suffix: '' });
  });

  it.each([
    ['a#frag', 'a#frag'],
    ['a?x=1', 'a?x=1'],
    // A raw '&' is HTML-escaped to the 5-character entity '&amp;' before
    // this function ever sees it — matching real usage, where the markdown
    // renderer escapes text before GCS_URI_PATTERN runs on it.
    ['a&b', 'a&amp;b'],
  ])('does not link %s: a fragment/query/param continuation immediately follows', (_, escaped) => {
    expect(resolve(`gs://bkt/${escaped}`)).toBeNull();
  });

  it('links up to a trailing ? at the end of a sentence', () => {
    // '?' is not part of the object class at all, so it was never in the
    // raw run to begin with — it sits entirely outside the match, which is
    // why the caller does not need a non-empty suffix to re-emit it.
    expect(resolve('gs://bkt/o.md?')).toEqual({ object: 'o.md', suffix: '' });
  });

  it('links up to a trailing ? followed by whitespace', () => {
    expect(resolve('gs://bkt/o.md? next')).toEqual({ object: 'o.md', suffix: '' });
  });

  it('links the double-quoted form', () => {
    // The quote, like '?', is outside the object class and outside the
    // match entirely — no suffix needed.
    expect(resolve('"gs://bkt/o.md"')).toEqual({ object: 'o.md', suffix: '' });
  });

  it('links the parenthesized form, re-emitting the trimmed trailing paren as the suffix', () => {
    // Unlike '?' or a quote, ')' IS in the object class, so it is captured
    // into the raw run and then trimmed off — the caller must re-emit it
    // via the non-empty suffix, not drop it.
    expect(resolve('(gs://bkt/o.md)')).toEqual({ object: 'o.md', suffix: ')' });
  });

  it('never rejects a directory-like trailing slash by falling back to a shorter object', () => {
    expect(resolve('gs://bkt/dir/')).toBeNull();
  });

  it.each([
    ['U+3000 (ideographic space)', '　'],
    ['U+00A0 (NBSP)', ' '],
  ])(
    "rejects a continuation follower that is Unicode whitespace (%s), matching the server's ASCII-only check",
    (_label, unicodeSpace) => {
      expect(resolve(`gs://bkt/o.md?${unicodeSpace}next`)).toBeNull();
    }
  );

  // Every ASCII whitespace byte individually, not just the plain space every
  // other case here happens to use — mirrors
  // TestGCSLink_EveryWhitespaceByteTerminatesContinuation in
  // pkg/hub/gcs_link_test.go.
  it.each([
    ['space', ' '],
    ['tab', '\t'],
    ['newline', '\n'],
    ['carriage return', '\r'],
    ['form feed', '\f'],
    ['vertical tab', '\v'],
  ])(
    'a continuation follower that is ASCII whitespace (%s) is not a continuation',
    (_label, ws) => {
      expect(resolve(`gs://bkt/o.md?${ws}next`)).toEqual({ object: 'o.md', suffix: '' });
    }
  );

  // Every trim byte individually, not just '.' and ',' — mirrors
  // TestGCSLink_EveryTrimByteIsTrimmed in pkg/hub/gcs_link_test.go.
  it.each(['.', ',', ':', '!', '$', '*', '(', ')', '-'])(
    'trims a trailing %s from the object',
    (trimByte) => {
      expect(resolve(`gs://bkt/a${trimByte}`)).toEqual({ object: 'a', suffix: trimByte });
    }
  );

  // Uppercase word bytes are object-class and final-class too — every
  // other row here happens to use an all-lowercase object. Mirrors
  // TestGCSLink_UppercaseObjectBytesAreObjectClass.
  it('links an object made of uppercase letters', () => {
    expect(resolve('gs://bkt/ABC')).toEqual({ object: 'ABC', suffix: '' });
  });
});

// The two cases below are accepted client-wider mismatches: the client
// links these, but the hub always refuses to serve them (a bucket
// containing ".." fails the server's own validation; an object over 1024
// bytes fails its own length check), so activating either link always ends
// in the uniform "not available" state. This asserts only the actual
// current client behaviour — it is not a claim that this is correct UX.
describe('accepted mismatches: client links what the server always refuses to serve', () => {
  it('links a bucket containing ".." (the server rejects any such bucket)', () => {
    expect(firstGcsMatch('gs://a..b/o.txt')).toEqual({ bucket: 'a..b', object: 'o.txt' });
  });

  it("links an object over the server's 1024-byte cap", () => {
    const longObject = 'a'.repeat(1025);
    expect(firstGcsMatch(`gs://bkt/${longObject}`)).toEqual({ bucket: 'bkt', object: longObject });
  });

  it('buildGcsObjectApiUrl itself refuses the over-cap object the regex just linked', () => {
    const longObject = 'a'.repeat(1025);
    expect(() =>
      buildGcsObjectApiUrl('00000000-0000-4000-8000-000000000000', 'bkt', longObject)
    ).toThrow();
  });
});

describe('parseGcsUri', () => {
  it('parses a well-formed URI', () => {
    expect(parseGcsUri('gs://bkt/dir/o.txt')).toEqual({ bucket: 'bkt', object: 'dir/o.txt' });
  });

  it('rejects a URI with a trailing slash (directory-like)', () => {
    expect(parseGcsUri('gs://bkt/dir/')).toBeNull();
  });

  it('rejects a non-gs scheme', () => {
    expect(parseGcsUri('https://bkt/o.txt')).toBeNull();
  });

  it('rejects trailing garbage after a valid URI', () => {
    expect(parseGcsUri('gs://bkt/o.txt)')).toBeNull();
  });
});

describe('escAttr', () => {
  it('escapes all five reserved characters', () => {
    expect(escAttr(`&<>"'`)).toBe('&amp;&lt;&gt;&quot;&#39;');
  });

  it('escapes & before other entities so it never double-encodes', () => {
    expect(escAttr('&amp;')).toBe('&amp;amp;');
  });

  it('leaves an ordinary gs:// URI unchanged', () => {
    expect(escAttr('gs://bkt/o.txt')).toBe('gs://bkt/o.txt');
  });
});

describe('buildGcsLinkHtml', () => {
  it('builds an anchor with the expected class, data attribute and text', () => {
    const html = buildGcsLinkHtml('bkt', 'dir/o.txt');
    expect(html).toBe(
      '<a class="entity-link gcs-link" data-gcs-uri="gs://bkt/dir/o.txt" href="javascript:void(0)" title="Open gs://bkt/dir/o.txt">gs://bkt/dir/o.txt</a>'
    );
  });

  it('escapes an ampersand that survives into the object text (defense in depth)', () => {
    // GCS_URI_PATTERN's object class has no '&', so this exercises escAttr's
    // own behaviour, not a value the regex could actually capture.
    const html = buildGcsLinkHtml('bkt', 'a&b');
    expect(html).toContain('data-gcs-uri="gs://bkt/a&amp;b"');
  });
});

/**
 * A hostile object name producing no extra attributes or elements is a
 * property of GCS_URI_PATTERN's object character class, which excludes
 * `& < > " '` and whitespace — never of buildGcsLinkHtml, which only ever
 * receives what the regex already captured. A hostile name is proven safe by
 * showing the regex stops the object capture before the dangerous
 * character, not by feeding the dangerous string to the builder directly
 * (the real-Chromium spec in web/e2e/chat-file-preview/ proves the full
 * pipeline end to end).
 */
describe('GCS_URI_PATTERN excludes dangerous characters from the object capture', () => {
  it('stops before a double quote', () => {
    expect(firstGcsMatch('gs://bkt/a"onmouseover=alert(1)')).toEqual({
      bucket: 'bkt',
      object: 'a',
    });
  });

  it('stops before a <', () => {
    expect(firstGcsMatch('gs://bkt/a<img src=x>')).toEqual({ bucket: 'bkt', object: 'a' });
  });

  it('stops the object capture before a literal & at all (the pattern runs on already-HTML-escaped text, so a raw & in the source is "&amp;" by the time this regex sees it), then rejects the whole occurrence under the continuation rule since more text follows', () => {
    // The '&' that starts "&amp;" is itself the very next character after
    // "a", followed by non-whitespace ("mp;b...") — the same continuation
    // rule the server applies to the raw '&' in the unescaped body, so
    // client and server reject this occurrence identically, not just
    // truncate it to "a".
    expect(firstGcsMatch('gs://bkt/a&amp;b')).toBeNull();
  });

  it('stops the raw run before & directly, independent of the continuation rule', () => {
    // Isolates "the object class excludes &" itself from the continuation
    // rule above: inspecting the regex's own raw group 3 directly (bypassing
    // resolveGcsMatch) shows the character run already stops at 'a', before
    // the '&' is ever considered — the continuation rule only decides
    // whether that stop counts as a full reject, never how the run itself
    // is bounded.
    const re = new RegExp(GCS_URI_PATTERN.source, GCS_URI_PATTERN.flags);
    const m = re.exec('gs://bkt/a&amp;b');
    expect(m).not.toBeNull();
    expect(m![3]).toBe('a');
  });
});

describe('buildGcsObjectApiUrl', () => {
  const messageId = '11111111-2222-4333-8444-555555555555';

  it('builds the expected URL', () => {
    expect(
      buildGcsObjectApiUrl(messageId, 'scion-xproject-exchange', 'workspace-volumes/dev-brief.md')
    ).toBe(
      `/api/v1/gcs/object?message=${messageId}&bucket=scion-xproject-exchange&object=workspace-volumes%2Fdev-brief.md`
    );
  });

  it('throws, and does not normalise, on a malformed message id', () => {
    expect(() => buildGcsObjectApiUrl('not-a-uuid', 'bkt', 'o.txt')).toThrow();
  });

  it('throws on a malformed bucket', () => {
    expect(() => buildGcsObjectApiUrl(messageId, 'AB', 'o.txt')).toThrow();
    expect(() => buildGcsObjectApiUrl(messageId, 'bk..t', 'o.txt')).toThrow();
  });

  it('throws on a malformed object', () => {
    expect(() => buildGcsObjectApiUrl(messageId, 'bkt', '')).toThrow();
    expect(() => buildGcsObjectApiUrl(messageId, 'bkt', 'o file.txt')).toThrow();
  });

  it('round-trips &, =, %2F and ~+@ in an object name exactly', () => {
    const object = 'a~b+c@d';
    const url = buildGcsObjectApiUrl(messageId, 'bkt', object);
    const params = new URL(url, 'http://x').searchParams;
    expect(params.get('object')).toBe(object);
  });

  it('round-trips a nested path (encoded /) exactly', () => {
    const object = 'dir/sub/file.name-1.txt';
    const url = buildGcsObjectApiUrl(messageId, 'bkt', object);
    expect(url).toContain('object=dir%2Fsub%2Ffile.name-1.txt');
    const params = new URL(url, 'http://x').searchParams;
    expect(params.get('object')).toBe(object);
  });
});

describe('pinGcsObjectUrl', () => {
  const messageId = '11111111-2222-4333-8444-555555555555';
  const validUrl = () => buildGcsObjectApiUrl(messageId, 'bkt', 'o.txt');

  it('accepts a URL built by buildGcsObjectApiUrl', () => {
    expect(() => pinGcsObjectUrl(validUrl(), messageId, 'bkt', 'o.txt')).not.toThrow();
  });

  it('rejects a wrong path', () => {
    expect(() =>
      pinGcsObjectUrl(
        '/api/v1/gcs/object-x?message=' + messageId + '&bucket=bkt&object=o.txt',
        messageId,
        'bkt',
        'o.txt'
      )
    ).toThrow();
  });

  it('rejects an extra query key', () => {
    expect(() => pinGcsObjectUrl(validUrl() + '&agent=x', messageId, 'bkt', 'o.txt')).toThrow();
  });

  it('rejects a duplicated object key', () => {
    expect(() =>
      pinGcsObjectUrl(validUrl() + '&object=other.txt', messageId, 'bkt', 'o.txt')
    ).toThrow();
  });

  it('rejects a duplicated bucket key', () => {
    expect(() =>
      pinGcsObjectUrl(validUrl() + '&bucket=other-bucket', messageId, 'bkt', 'o.txt')
    ).toThrow();
  });

  it('rejects a duplicated message key', () => {
    const otherMessageId = '99999999-8888-4777-8666-555555555555';
    expect(() =>
      pinGcsObjectUrl(validUrl() + `&message=${otherMessageId}`, messageId, 'bkt', 'o.txt')
    ).toThrow();
  });

  it('rejects a built value with no query string at all', () => {
    expect(() => pinGcsObjectUrl('/api/v1/gcs/object', messageId, 'bkt', 'o.txt')).toThrow();
  });

  it('rejects a hash fragment', () => {
    expect(() => pinGcsObjectUrl(validUrl() + '#frag', messageId, 'bkt', 'o.txt')).toThrow();
  });

  it('rejects a bucket value mismatch', () => {
    expect(() => pinGcsObjectUrl(validUrl(), messageId, 'other-bucket', 'o.txt')).toThrow();
  });

  it('rejects an object value mismatch', () => {
    expect(() => pinGcsObjectUrl(validUrl(), messageId, 'bkt', 'other.txt')).toThrow();
  });

  it('rejects a message-id value mismatch', () => {
    const otherMessageId = '99999999-8888-4777-8666-555555555555';
    expect(() => pinGcsObjectUrl(validUrl(), otherMessageId, 'bkt', 'o.txt')).toThrow();
  });

  it('rejects an absolute URL smuggled in as the built value', () => {
    expect(() =>
      pinGcsObjectUrl(
        `https://evil.example/api/v1/gcs/object?message=${messageId}&bucket=bkt&object=o.txt`,
        messageId,
        'bkt',
        'o.txt'
      )
    ).toThrow();
  });
});

describe('buildCloudConsoleUrl', () => {
  it('builds the console link for the cross-project-exchange URI', () => {
    expect(buildCloudConsoleUrl('scion-xproject-exchange', 'workspace-volumes/dev-brief.md')).toBe(
      'https://console.cloud.google.com/storage/browser/_details/scion-xproject-exchange/workspace-volumes/dev-brief.md'
    );
  });

  it('encodes a space in an object name', () => {
    expect(buildCloudConsoleUrl('bkt', 'a b.txt')).toBe(
      'https://console.cloud.google.com/storage/browser/_details/bkt/a%20b.txt'
    );
  });

  it('encodes # and ? in an object name', () => {
    expect(buildCloudConsoleUrl('bkt', 'a#b?c.txt')).toBe(
      'https://console.cloud.google.com/storage/browser/_details/bkt/a%23b%3Fc.txt'
    );
  });

  it('encodes % in an object name', () => {
    expect(buildCloudConsoleUrl('bkt', 'a%b.txt')).toBe(
      'https://console.cloud.google.com/storage/browser/_details/bkt/a%25b.txt'
    );
  });

  it('preserves nested / as path structure rather than encoding it', () => {
    expect(buildCloudConsoleUrl('bkt', 'a/b/c.txt')).toBe(
      'https://console.cloud.google.com/storage/browser/_details/bkt/a/b/c.txt'
    );
  });
});

describe('artifact references', () => {
  const ID = '5f1c2d3e-0000-4000-8000-0000000000aa';

  it('parses canonical references, lowercasing the id', async () => {
    const { parseArtifactRef } = await import('./chat-file-links.js');
    expect(parseArtifactRef(`scion://artifact/${ID}`)).toEqual({ id: ID, seq: 0 });
    expect(parseArtifactRef(`scion://artifact/${ID.toUpperCase()}@12`)).toEqual({
      id: ID,
      seq: 12,
    });
    expect(parseArtifactRef(`  scion://artifact/${ID}@3  `)).toEqual({ id: ID, seq: 3 });
  });

  it('rejects malformed references', async () => {
    const { parseArtifactRef } = await import('./chat-file-links.js');
    for (const bad of [
      ID,
      `scion://artifact/${ID}@0`,
      `scion://artifact/${ID}@01`,
      `scion://artifact/${ID}@2x`,
      'scion://artifact/not-a-uuid',
      `https://example.com/${ID}`,
    ]) {
      expect(parseArtifactRef(bad)).toBeNull();
    }
  });

  it('finds references in running text without swallowing what follows', async () => {
    const { ARTIFACT_REF_PATTERN } = await import('./chat-file-links.js');
    const re = new RegExp(ARTIFACT_REF_PATTERN.source, 'g');
    const text = `a scion://artifact/${ID}@2, b scion://artifact/${ID}. c scion://artifact/${ID}@3x`;
    expect([...text.matchAll(re)].map((m) => m[0])).toEqual([
      `scion://artifact/${ID}@2`,
      `scion://artifact/${ID}`,
    ]);
  });

  it('builds escaped link markup carrying id and seq', async () => {
    const { buildArtifactLinkHtml } = await import('./chat-file-links.js');
    expect(buildArtifactLinkHtml(`scion://artifact/${ID}@2`, { id: ID, seq: 2 })).toBe(
      `<a class="entity-link artifact-link" data-artifact-id="${ID}" data-artifact-seq="2" href="javascript:void(0)" title="Open artifact">scion://artifact/${ID}&#64;2</a>`
    );
    expect(buildArtifactLinkHtml('<x>', { id: '"', seq: 0 })).toBe(
      '<a class="entity-link artifact-link" data-artifact-id="&quot;" href="javascript:void(0)" title="Open artifact">&lt;x&gt;</a>'
    );
  });
});
