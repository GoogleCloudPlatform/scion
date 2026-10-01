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
  isMarkdownFileName,
  isLikelyTextFileName,
  isLikelyTextMime,
  isLikelyBinaryFileName,
  baseMimeType,
  extensionOf,
  resolveMessageProjectId,
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
