/**
 * Tests for path-link utilities (#1148): parseContainerPath and buildFileApiUrl.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi } from 'vitest';

// The module under test imports from the app entry. Loading the real
// main.ts registers every Shoelace component and runs the app bootstrap,
// so happy-dom tries to fetch from 127.0.0.1:3000 and logs ECONNREFUSED on
// stderr. These tests only need a stub; it matches the one in
// chat-members.test.ts (ptone/scion#3392).
vi.mock('../../../client/main.js', () => ({
  navigateTo: vi.fn(),
  pushRoute: vi.fn((path: string) => {
    window.history.pushState({}, '', path);
    return Promise.resolve();
  }),
  replaceRoute: vi.fn((path: string) => {
    window.history.replaceState(
      window.history.state,
      '',
      path + window.location.search + window.location.hash
    );
    return Promise.resolve();
  }),
  stateManager: new EventTarget(),
}));
import { parseContainerPath, buildFileApiUrl, type PathLinkTarget } from './chat-thread.js';

describe('parseContainerPath', () => {
  it('parses /scion-volumes/{dirName}/{filePath}', () => {
    const result = parseContainerPath('/scion-volumes/scratchpad/reports/summary.md');
    expect(result).toEqual({
      kind: 'shared-dir',
      dirName: 'scratchpad',
      filePath: 'reports/summary.md',
    });
  });

  it('rejects a bare /scion-volumes/{dirName} with no file path — a directory is not a file', () => {
    expect(parseContainerPath('/scion-volumes/scratchpad')).toBeNull();
  });

  it('parses /workspace/.scion-volumes/{dirName}/{filePath}', () => {
    const result = parseContainerPath('/workspace/.scion-volumes/data/output.json');
    expect(result).toEqual({
      kind: 'shared-dir',
      dirName: 'data',
      filePath: 'output.json',
    });
  });

  it('parses /workspace/{filePath}', () => {
    const result = parseContainerPath('/workspace/src/main.go');
    expect(result).toEqual({
      kind: 'workspace',
      filePath: 'src/main.go',
    });
  });

  it('parses deeply nested workspace path', () => {
    const result = parseContainerPath('/workspace/src/components/shared/chat/chat-message.ts');
    expect(result).toEqual({
      kind: 'workspace',
      filePath: 'src/components/shared/chat/chat-message.ts',
    });
  });

  it('returns null for unrecognized paths', () => {
    expect(parseContainerPath('/etc/passwd')).toBeNull();
    expect(parseContainerPath('/root/.ssh/id_rsa')).toBeNull();
    expect(parseContainerPath('/tmp/something')).toBeNull();
  });

  it('returns null for bare /workspace with no sub-path', () => {
    expect(parseContainerPath('/workspace')).toBeNull();
    expect(parseContainerPath('/workspace/')).toBeNull();
  });

  it('handles paths with dots in directory names', () => {
    const result = parseContainerPath('/scion-volumes/my.data/file.txt');
    expect(result).toEqual({
      kind: 'shared-dir',
      dirName: 'my.data',
      filePath: 'file.txt',
    });
  });

  it('handles workspace path with .scion-volumes deeper than root', () => {
    const result = parseContainerPath(
      '/workspace/.scion-volumes/shared-stuff/deeply/nested/file.md'
    );
    expect(result).toEqual({
      kind: 'shared-dir',
      dirName: 'shared-stuff',
      filePath: 'deeply/nested/file.md',
    });
  });

  // A raw path is never normalized — a dot-segment, an empty segment, its
  // percent-encoded spelling, or a query/fragment delimiter is refused
  // outright, in every position the pattern can put it: workspace, the two
  // shared-dir spellings, and the shared-dir name itself.
  describe('rejects traversal and route-changing input rather than normalizing it', () => {
    const dotSegmentCases: Array<[string, string]> = [
      ['single ../ segment', '/workspace/../agents'],
      ['../../ climbing two levels', '/workspace/../../agents'],
      ['a trailing .. segment', '/workspace/foo/..'],
      ['a lone . segment', '/workspace/./foo.txt'],
      ['.. inside a shared-dir file path', '/scion-volumes/scratchpad/../../agents.txt'],
      [
        '.. inside the in-workspace shared-dir spelling',
        '/workspace/.scion-volumes/data/../../agents.txt',
      ],
      [
        '.. as the dirName in the in-workspace shared-dir spelling',
        '/workspace/.scion-volumes/../etc/passwd.txt',
      ],
      ['.. as the shared-dir name itself', '/scion-volumes/../etc/passwd.txt'],
      ['an empty segment (double slash)', '/workspace/foo//bar.txt'],
    ];
    it.each(dotSegmentCases)('%s: %s', (_label, path) => {
      expect(parseContainerPath(path)).toBeNull();
    });

    const encodedCases: Array<[string, string]> = [
      ['%2e%2e lowercase', '/workspace/%2e%2e/agents.txt'],
      ['%2E%2E uppercase', '/workspace/%2E%2E/agents.txt'],
      ['%2e%2E mixed case', '/workspace/%2e%2E/agents.txt'],
      ['.%2e mixed literal/encoded', '/workspace/.%2e/agents.txt'],
      ['%2e. mixed encoded/literal', '/workspace/%2e./agents.txt'],
      ['%2f encoded slash lowercase', '/workspace/foo%2f..%2fagents.txt'],
      ['%2F encoded slash uppercase', '/workspace/foo%2F..%2Fagents.txt'],
      ['%5c encoded backslash lowercase', '/workspace/foo%5c..%5cagents.txt'],
      ['%5C encoded backslash uppercase', '/workspace/foo%5C..%5Cagents.txt'],
      ['a literal backslash', '/workspace/foo\\..\\agents.txt'],
    ];
    it.each(encodedCases)('%s: %s', (_label, path) => {
      expect(parseContainerPath(path)).toBeNull();
    });

    // No `..` alongside the delimiter — a vector mixing both would still be
    // rejected by the dot-segment rule alone even if `?`/`#` rejection
    // itself were broken, masking the very thing this case isolates.
    const injectionCases: Array<[string, string]> = [
      ['a ? query delimiter (no ..)', '/workspace/notes.txt?x'],
      ['a # fragment delimiter (no ..)', '/workspace/notes.txt#x'],
    ];
    it.each(injectionCases)('%s: %s', (_label, path) => {
      expect(parseContainerPath(path)).toBeNull();
    });
  });
});

describe('buildFileApiUrl', () => {
  it('builds workspace file URL', () => {
    const target: PathLinkTarget = { kind: 'workspace', filePath: 'src/main.go' };
    const url = buildFileApiUrl('proj-123', target);
    expect(url).toBe('/api/v1/projects/proj-123/workspace/files/src/main.go');
  });

  it('builds shared-dir file URL', () => {
    const target: PathLinkTarget = {
      kind: 'shared-dir',
      dirName: 'scratchpad',
      filePath: 'reports/summary.md',
    };
    const url = buildFileApiUrl('proj-123', target);
    expect(url).toBe('/api/v1/projects/proj-123/shared-dirs/scratchpad/files/reports/summary.md');
  });

  it('encodes special characters in path segments', () => {
    const target: PathLinkTarget = { kind: 'workspace', filePath: 'src/my file.ts' };
    const url = buildFileApiUrl('proj-123', target);
    expect(url).toBe('/api/v1/projects/proj-123/workspace/files/src/my%20file.ts');
  });

  it('encodes special characters in project ID', () => {
    const target: PathLinkTarget = { kind: 'workspace', filePath: 'main.go' };
    const url = buildFileApiUrl('proj with space', target);
    expect(url).toBe('/api/v1/projects/proj%20with%20space/workspace/files/main.go');
  });

  it('encodes special characters in shared-dir name', () => {
    const target: PathLinkTarget = {
      kind: 'shared-dir',
      dirName: 'my dir',
      filePath: 'file.txt',
    };
    const url = buildFileApiUrl('proj-123', target);
    expect(url).toBe('/api/v1/projects/proj-123/shared-dirs/my%20dir/files/file.txt');
  });

  it('the built pathname never leaves the files endpoint, for a normal path', () => {
    const target: PathLinkTarget = { kind: 'workspace', filePath: 'deeply/nested/file.txt' };
    const url = buildFileApiUrl('proj-123', target);
    const pathname = new URL(url, 'http://example.test').pathname;
    expect(pathname.startsWith('/api/v1/projects/proj-123/workspace/files/')).toBe(true);
  });

  // buildFileApiUrl re-validates even a target that bypassed parseContainerPath
  // (e.g. read back from localStorage) — it must never guess a "cleaned" URL.
  it('throws rather than building a URL for an unsafe workspace filePath', () => {
    const target: PathLinkTarget = { kind: 'workspace', filePath: '../../agents' };
    expect(() => buildFileApiUrl('proj-123', target)).toThrow();
  });

  it('throws rather than building a URL for an unsafe shared-dir filePath', () => {
    const target: PathLinkTarget = { kind: 'shared-dir', dirName: 'scratchpad', filePath: '..' };
    expect(() => buildFileApiUrl('proj-123', target)).toThrow();
  });

  it('throws rather than building a URL for an unsafe shared-dir name', () => {
    const target: PathLinkTarget = { kind: 'shared-dir', dirName: '..', filePath: 'file.txt' };
    expect(() => buildFileApiUrl('proj-123', target)).toThrow();
  });

  // The full unsafe-path vector table, at the URL-build layer, for both
  // target kinds and both the filePath and dirName positions. The ?/#
  // vectors deliberately contain no `..`, so they can only be caught by the
  // `?`/`#` rule itself, not masked by the dot-segment rule.
  describe('rejects the full unsafe-path vector table rather than building a URL for it', () => {
    const unsafeSegments: Array<[string, string]> = [
      ['a raw .. segment', '../secret'],
      ['a raw . segment', './secret'],
      ['%2e%2e lowercase', '%2e%2e/secret'],
      ['%2E%2E uppercase', '%2E%2E/secret'],
      ['%2f encoded slash lowercase', 'foo%2fsecret'],
      ['%2F encoded slash uppercase', 'foo%2Fsecret'],
      ['%5c encoded backslash lowercase', 'foo%5csecret'],
      ['%5C encoded backslash uppercase', 'foo%5Csecret'],
      ['a literal backslash', 'foo\\secret'],
      ['a ? query delimiter (no ..)', 'notes.txt?x'],
      ['a # fragment delimiter (no ..)', 'notes.txt#x'],
    ];

    it.each(unsafeSegments)('workspace filePath: %s', (_label, filePath) => {
      const target: PathLinkTarget = { kind: 'workspace', filePath };
      expect(() => buildFileApiUrl('proj-123', target)).toThrow();
    });

    it.each(unsafeSegments)('shared-dir filePath: %s', (_label, filePath) => {
      const target: PathLinkTarget = { kind: 'shared-dir', dirName: 'scratchpad', filePath };
      expect(() => buildFileApiUrl('proj-123', target)).toThrow();
    });

    it.each(unsafeSegments)('shared-dir dirName: %s', (_label, dirName) => {
      const target: PathLinkTarget = { kind: 'shared-dir', dirName, filePath: 'file.txt' };
      expect(() => buildFileApiUrl('proj-123', target)).toThrow();
    });
  });

  // projectId occupies exactly one route segment. A dot
  // segment, an embedded slash, a raw or percent-encoded route-changing
  // character must throw rather than be encoded into a URL that a
  // browser/server could normalize onto a different, unintended API route
  // (e.g. `buildFileApiUrl('..', ...)` must never produce a URL whose
  // pathname resolves outside `/api/v1/projects/{id}/...`).
  describe('rejects an unsafe project ID rather than encoding it into a different route', () => {
    const unsafeProjectIds: Array<[string, string]> = [
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
    it.each(unsafeProjectIds)('%s: %s', (_label, projectId) => {
      const target: PathLinkTarget = { kind: 'workspace', filePath: 'main.go' };
      expect(() => buildFileApiUrl(projectId, target)).toThrow();
    });

    it('also rejects an unsafe project ID for a shared-dir target', () => {
      const target: PathLinkTarget = {
        kind: 'shared-dir',
        dirName: 'scratchpad',
        filePath: 'a.txt',
      };
      expect(() => buildFileApiUrl('..', target)).toThrow();
    });
  });
});
