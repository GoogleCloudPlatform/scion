/**
 * Tests for path-link utilities (#1148): parseContainerPath and buildFileApiUrl.
 */

// @vitest-environment happy-dom

import { describe, it, expect } from 'vitest';
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

  it('parses /scion-volumes/{dirName} with no nested path', () => {
    const result = parseContainerPath('/scion-volumes/scratchpad');
    expect(result).toEqual({
      kind: 'shared-dir',
      dirName: 'scratchpad',
      filePath: '',
    });
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
    const result = parseContainerPath('/workspace/.scion-volumes/shared-stuff/deeply/nested/file.md');
    expect(result).toEqual({
      kind: 'shared-dir',
      dirName: 'shared-stuff',
      filePath: 'deeply/nested/file.md',
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
});
