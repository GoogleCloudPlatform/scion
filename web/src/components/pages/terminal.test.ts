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
 * Tests for drag-and-drop file upload in the terminal page component.
 *
 * Covers four areas introduced by the drag-drop feature:
 *
 *  1. _quoteForShell — pure function: safe paths, spaces, single quotes,
 *     shell metacharacters.
 *  2. _handleFileDrop size validation — per-file 50MB and total 100MB limits.
 *  3. resolveUploadTarget — shared dir selection logic (prefer scratchpad,
 *     filter read-only / in_workspace, disable when none available).
 *  4. _onDragEnter / _onDragLeave counter — overlay show/hide with
 *     flicker prevention across child elements.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

let ScionPageTerminal: any;

/** One recorded fetch call. */
interface Call {
  url: string;
  method: string;
  body: any;
}

function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Shared dirs response shape. */
interface SharedDir {
  name: string;
  read_only?: boolean;
  in_workspace?: boolean;
}

interface MockOptions {
  /** Shared dirs returned by the project shared-dirs endpoint. */
  sharedDirs?: SharedDir[];
  /** Status for the upload POST (default 200). */
  uploadStatus?: number;
}

/**
 * Build a fetch mock that records calls and answers the endpoints the
 * terminal component touches during tests.
 */
function makeFetchMock(calls: Call[], opts: MockOptions = {}) {
  return async (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const href = String(url);
    const method = init?.method ?? 'GET';
    let parsed: any = undefined;
    if (init?.body && typeof init.body === 'string') {
      try {
        parsed = JSON.parse(init.body);
      } catch {
        /* non-JSON body — ignore */
      }
    }
    calls.push({ url: href, method, body: parsed });

    // Shared dirs endpoint
    if (href.includes('/shared-dirs') && method === 'GET') {
      return jsonResponse({ sharedDirs: opts.sharedDirs ?? [] }, 200);
    }

    // Upload endpoint
    if (href.includes('/shared-dirs/') && href.includes('/files') && method === 'POST') {
      const status = opts.uploadStatus ?? 200;
      if (status >= 400) {
        return jsonResponse({ error: { message: 'upload failed' } }, status);
      }
      return jsonResponse({ ok: true }, status);
    }

    // Agent info (returns a minimal agent object)
    if (href.includes('/api/v1/agents/')) {
      return jsonResponse(
        {
          id: 'test-agent',
          name: 'test-agent',
          phase: 'running',
          projectId: 'proj-1',
          activity: '',
          exposedPorts: [],
        },
        200
      );
    }

    return jsonResponse({}, 200);
  };
}

/** Create an element instance without appending to DOM (avoids connectedCallback side effects). */
function createElement(): any {
  // Use document.createElement but don't append — this gives us an instance
  // we can call private methods on without triggering loadAgentInfo().
  const el = document.createElement('scion-page-terminal') as any;
  return el;
}

/** Create an element, set up basic state, and stub fetch for resolveUploadTarget tests. */
async function createForUploadTest(calls: Call[], opts: MockOptions = {}): Promise<any> {
  vi.stubGlobal('fetch', vi.fn(makeFetchMock(calls, opts)));
  const el = createElement();
  el.projectId = 'proj-1';
  return el;
}

function cleanup() {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
}

// ── _quoteForShell ──────────────────────────────────────────────────────────

describe('terminal — _quoteForShell', () => {
  beforeAll(async () => {
    const mod = await import('./terminal.js');
    ScionPageTerminal = mod.ScionPageTerminal;
    expect(ScionPageTerminal).toBeDefined();
  });

  afterEach(cleanup);

  it('returns safe paths unquoted', () => {
    const el = createElement();
    expect((el as any)._quoteForShell('foo/bar.txt')).toBe('foo/bar.txt');
    expect((el as any)._quoteForShell('/scion-volumes/scratchpad/file.tar.gz')).toBe(
      '/scion-volumes/scratchpad/file.tar.gz'
    );
  });

  it('quotes paths with spaces', () => {
    const el = createElement();
    expect((el as any)._quoteForShell('foo bar.txt')).toBe("'foo bar.txt'");
  });

  it('escapes single quotes in paths', () => {
    const el = createElement();
    expect((el as any)._quoteForShell("it's.txt")).toBe("'it'\\''s.txt'");
  });

  it('quotes paths with dollar signs', () => {
    const el = createElement();
    const result = (el as any)._quoteForShell('file$var.txt');
    expect(result).toBe("'file$var.txt'");
  });

  it('quotes paths with backticks', () => {
    const el = createElement();
    const result = (el as any)._quoteForShell('file`cmd`.txt');
    expect(result).toBe("'file`cmd`.txt'");
  });

  it('quotes paths with backslashes', () => {
    const el = createElement();
    const result = (el as any)._quoteForShell('file\\name.txt');
    expect(result).toBe("'file\\name.txt'");
  });
});

// ── _handleFileDrop size validation ─────────────────────────────────────────

describe('terminal — _handleFileDrop size validation', () => {
  beforeAll(async () => {
    await import('./terminal.js');
  });

  afterEach(cleanup);

  it('shows error when a single file exceeds 50MB', async () => {
    const calls: Call[] = [];
    const el = await createForUploadTest(calls);
    el.uploadEnabled = true;
    el.uploadTargetDir = 'scratchpad';
    el.uploadBasePath = '/scion-volumes/scratchpad';

    const bigFile = new File(['x'], 'big.bin');
    Object.defineProperty(bigFile, 'size', { value: 51 * 1024 * 1024 });
    const fileList = {
      length: 1,
      0: bigFile,
      [Symbol.iterator]: function* () {
        yield bigFile;
      },
    } as unknown as FileList;

    await (el as any)._handleFileDrop(fileList);

    expect(el.uploadStatus).toContain('exceeds 50MB');
    expect(el.isUploading).toBe(false);
  });

  it('shows error when total exceeds 100MB', async () => {
    const calls: Call[] = [];
    const el = await createForUploadTest(calls);
    el.uploadEnabled = true;
    el.uploadTargetDir = 'scratchpad';
    el.uploadBasePath = '/scion-volumes/scratchpad';

    const file1 = new File(['x'], 'a.bin');
    Object.defineProperty(file1, 'size', { value: 40 * 1024 * 1024 });
    const file2 = new File(['x'], 'b.bin');
    Object.defineProperty(file2, 'size', { value: 40 * 1024 * 1024 });
    const file3 = new File(['x'], 'c.bin');
    Object.defineProperty(file3, 'size', { value: 25 * 1024 * 1024 });
    const files = [file1, file2, file3];
    const fileList = {
      length: 3,
      0: file1,
      1: file2,
      2: file3,
      [Symbol.iterator]: function* () {
        for (const f of files) yield f;
      },
    } as unknown as FileList;

    await (el as any)._handleFileDrop(fileList);

    expect(el.uploadStatus).toContain('100MB');
    expect(el.isUploading).toBe(false);
  });
});

// ── resolveUploadTarget ─────────────────────────────────────────────────────

describe('terminal — resolveUploadTarget', () => {
  beforeAll(async () => {
    await import('./terminal.js');
  });

  afterEach(cleanup);

  it('prefers scratchpad when available', async () => {
    const calls: Call[] = [];
    const el = await createForUploadTest(calls, {
      sharedDirs: [{ name: 'data' }, { name: 'scratchpad' }, { name: 'other' }],
    });

    await (el as any).resolveUploadTarget();

    expect(el.uploadEnabled).toBe(true);
    expect(el.uploadTargetDir).toBe('scratchpad');
    expect(el.uploadBasePath).toBe('/scion-volumes/scratchpad');
  });

  it('falls back to first writable non-in_workspace dir', async () => {
    const calls: Call[] = [];
    const el = await createForUploadTest(calls, {
      sharedDirs: [
        { name: 'workspace-dir', in_workspace: true },
        { name: 'data' },
        { name: 'other' },
      ],
    });

    await (el as any).resolveUploadTarget();

    expect(el.uploadEnabled).toBe(true);
    expect(el.uploadTargetDir).toBe('data');
  });

  it('disables upload when no writable dir is available', async () => {
    const calls: Call[] = [];
    const el = await createForUploadTest(calls, {
      sharedDirs: [
        { name: 'readonly-dir', read_only: true },
        { name: 'workspace-dir', in_workspace: true },
      ],
    });

    await (el as any).resolveUploadTarget();

    expect(el.uploadEnabled).toBe(false);
    expect(el.uploadDisabledReason).toContain('No writable shared directory');
  });

  it('filters out read-only and in_workspace dirs', async () => {
    const calls: Call[] = [];
    const el = await createForUploadTest(calls, {
      sharedDirs: [
        { name: 'ro', read_only: true },
        { name: 'iw', in_workspace: true },
        { name: 'ro-iw', read_only: true, in_workspace: true },
      ],
    });

    await (el as any).resolveUploadTarget();

    expect(el.uploadEnabled).toBe(false);
  });

  it('disables upload when empty dir list returned', async () => {
    const calls: Call[] = [];
    const el = await createForUploadTest(calls, { sharedDirs: [] });

    await (el as any).resolveUploadTarget();

    expect(el.uploadEnabled).toBe(false);
  });
});

// ── _handleFileDrop upload paths (409, network error, success) ────────────

describe('terminal — _handleFileDrop upload paths', () => {
  beforeAll(async () => {
    await import('./terminal.js');
  });

  afterEach(cleanup);

  /** Create a small valid file and wrap it in a FileList-like object. */
  function makeFileList(...files: File[]): FileList {
    const fl: any = {
      length: files.length,
      [Symbol.iterator]: function* () {
        for (const f of files) yield f;
      },
    };
    files.forEach((f, i) => {
      fl[i] = f;
    });
    return fl as FileList;
  }

  /** Set up an element ready for upload tests with the given fetch mock. */
  function setupElement(fetchMock: typeof fetch): any {
    vi.stubGlobal('fetch', fetchMock);
    const el = createElement();
    el.uploadEnabled = true;
    el.uploadTargetDir = 'scratchpad';
    el.uploadBasePath = '/scion-volumes/scratchpad';
    el.projectId = 'test-project';
    return el;
  }

  it('shows error and disables upload on 409 response', async () => {
    const el = setupElement(
      vi.fn(async () => jsonResponse({ error: { message: 'upload failed' } }, 409))
    );

    const file = new File(['hello'], 'test.txt');
    await (el as any)._handleFileDrop(makeFileList(file));

    // Error message should be preserved (not clobbered by finally)
    expect(el.uploadStatus).toContain('co-located runtime broker');
    expect(el.isUploading).toBe(false);
    expect(el.uploadEnabled).toBe(false);
    // Overlay should be visible to show the error
    expect(el.isDragOver).toBe(true);
  });

  it('shows error on non-ok HTTP response', async () => {
    const el = setupElement(
      vi.fn(async () => jsonResponse({ error: { message: 'server error details' } }, 500))
    );

    const file = new File(['hello'], 'test.txt');
    await (el as any)._handleFileDrop(makeFileList(file));

    // Should show the extracted error message
    expect(el.uploadStatus).toBeTruthy();
    expect(el.isUploading).toBe(false);
    expect(el.isDragOver).toBe(true);
  });

  it('shows error on network failure', async () => {
    const el = setupElement(
      vi.fn(async () => {
        throw new Error('Network failure');
      })
    );

    const file = new File(['hello'], 'test.txt');
    await (el as any)._handleFileDrop(makeFileList(file));

    expect(el.uploadStatus).toContain('network error');
    expect(el.isUploading).toBe(false);
    expect(el.isDragOver).toBe(true);
  });

  it('calls sendData with shell-quoted paths on success', async () => {
    const el = setupElement(vi.fn(async () => jsonResponse({ ok: true }, 200)));
    // Mock sendData to capture what gets sent to the terminal
    const sendDataCalls: string[] = [];
    el.sendData = (data: string) => {
      sendDataCalls.push(data);
    };

    const file = new File(['hello'], 'test.txt');
    await (el as any)._handleFileDrop(makeFileList(file));

    // Should have sent the path to the terminal
    expect(sendDataCalls.length).toBe(1);
    expect(sendDataCalls[0]).toContain('/scion-volumes/scratchpad/.attachments/_web/');
    expect(sendDataCalls[0]).toContain('test.txt');
    // Upload state should be cleared on success
    expect(el.isUploading).toBe(false);
    expect(el.uploadStatus).toBe('');
  });

  it('sends multiple shell-quoted paths on multi-file success', async () => {
    const el = setupElement(vi.fn(async () => jsonResponse({ ok: true }, 200)));
    const sendDataCalls: string[] = [];
    el.sendData = (data: string) => {
      sendDataCalls.push(data);
    };

    const file1 = new File(['a'], 'doc.pdf');
    const file2 = new File(['b'], 'has spaces.txt');
    await (el as any)._handleFileDrop(makeFileList(file1, file2));

    expect(sendDataCalls.length).toBe(1);
    // "has spaces.txt" should be quoted
    expect(sendDataCalls[0]).toContain("'");
    expect(sendDataCalls[0]).toContain('doc.pdf');
    expect(sendDataCalls[0]).toContain('has spaces.txt');
  });
});

// ── _onDragEnter / _onDragLeave counter ─────────────────────────────────────

describe('terminal — drag enter/leave counter', () => {
  beforeAll(async () => {
    await import('./terminal.js');
  });

  afterEach(cleanup);

  function makeDragEvent(type: string): DragEvent {
    const event = new Event(type, { bubbles: true }) as any;
    event.preventDefault = vi.fn();
    event.dataTransfer = { dropEffect: '', files: [] };
    return event as DragEvent;
  }

  it('shows overlay on first dragenter', () => {
    const el = createElement();
    expect(el.isDragOver).toBe(false);

    (el as any)._onDragEnter(makeDragEvent('dragenter'));

    expect(el.isDragOver).toBe(true);
    expect((el as any)._dragCounter).toBe(1);
  });

  it('keeps overlay visible during child element transitions', () => {
    const el = createElement();

    // Enter wrapper
    (el as any)._onDragEnter(makeDragEvent('dragenter'));
    expect(el.isDragOver).toBe(true);

    // Enter child (second enter before first leave)
    (el as any)._onDragEnter(makeDragEvent('dragenter'));
    expect(el.isDragOver).toBe(true);
    expect((el as any)._dragCounter).toBe(2);

    // Leave child (first leave)
    (el as any)._onDragLeave(makeDragEvent('dragleave'));
    expect(el.isDragOver).toBe(true);
    expect((el as any)._dragCounter).toBe(1);
  });

  it('hides overlay on final dragleave', () => {
    const el = createElement();

    // Enter
    (el as any)._onDragEnter(makeDragEvent('dragenter'));
    expect(el.isDragOver).toBe(true);

    // Leave
    (el as any)._onDragLeave(makeDragEvent('dragleave'));
    expect(el.isDragOver).toBe(false);
    expect((el as any)._dragCounter).toBe(0);
  });

  it('resets counter to 0 on drop', async () => {
    const el = createElement();
    el.uploadEnabled = false; // Will exit early from _onDrop but still reset counter

    // Simulate multiple enters
    (el as any)._onDragEnter(makeDragEvent('dragenter'));
    (el as any)._onDragEnter(makeDragEvent('dragenter'));
    expect((el as any)._dragCounter).toBe(2);

    // Drop event
    const dropEvent = makeDragEvent('drop') as any;
    dropEvent.dataTransfer = { files: { length: 0 } };
    await (el as any)._onDrop(dropEvent);

    expect((el as any)._dragCounter).toBe(0);
    expect(el.isDragOver).toBe(false);
  });
});
