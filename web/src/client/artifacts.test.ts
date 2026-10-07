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

import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  artifactFileUrl,
  artifactPagePath,
  baseName,
  formatBytes,
  isInlineType,
  PublishError,
  listProjectArtifacts,
  parseArtifactPagePath,
  publishErrorMessage,
  publishFiles,
  rendererFor,
  sha256Hex,
} from './artifacts.js';

describe('rendererFor', () => {
  it('maps media types to renderers', () => {
    expect(rendererFor('text/markdown')).toBe('markdown');
    expect(rendererFor('text/plain')).toBe('text');
    expect(rendererFor('application/json')).toBe('text');
    expect(rendererFor('text/csv')).toBe('text');
    expect(rendererFor('image/png')).toBe('image');
    expect(rendererFor('IMAGE/JPEG')).toBe('image');
  });

  it('renders HTML only in its own sandboxed view, and other active content not at all', () => {
    expect(rendererFor('text/html')).toBe('html');
    expect(rendererFor('image/svg+xml')).toBe('download');
    expect(rendererFor('application/pdf')).toBe('download');
    expect(rendererFor('application/octet-stream')).toBe('download');
  });
});

describe('artifactFileUrl', () => {
  it('builds current and versioned file URLs', () => {
    expect(artifactFileUrl('a1', 0, 'design.md')).toBe('/api/v1/artifacts/a1/files/design.md');
    expect(artifactFileUrl('a1', 2, 'dir/a b.md', true)).toBe(
      '/api/v1/artifacts/a1/versions/2/files/dir/a%20b.md?stream=1'
    );
  });

  it('escapes each path segment', () => {
    expect(artifactFileUrl('a/1', 0, 'x?y#z.md')).toBe(
      '/api/v1/artifacts/a%2F1/files/x%3Fy%23z.md'
    );
  });
});

describe('formatBytes', () => {
  it('formats sizes', () => {
    expect(formatBytes(12)).toBe('12 B');
    expect(formatBytes(2048)).toBe('2.0 KiB');
    expect(formatBytes(3 * 1024 * 1024)).toBe('3.0 MiB');
  });
});

describe('isInlineType and baseName', () => {
  it('mirrors the hub inline list', () => {
    for (const mt of [
      'text/markdown',
      'text/plain',
      'text/tab-separated-values',
      'image/png',
      'APPLICATION/JSON',
    ]) {
      expect(isInlineType(mt)).toBe(true);
    }
    for (const mt of [
      'text/html',
      'image/svg+xml',
      'application/pdf',
      'application/octet-stream',
    ]) {
      expect(isInlineType(mt)).toBe(false);
    }
  });

  it('takes the last path segment', () => {
    expect(baseName('dir/sub/page.html')).toBe('page.html');
    expect(baseName('page.html')).toBe('page.html');
  });
});

describe('artifact page paths', () => {
  it('round-trips current and versioned paths', () => {
    expect(artifactPagePath('p 1', 'a/b')).toBe('/projects/p%201/artifacts/a%2Fb');
    expect(artifactPagePath('p', 'a', 3)).toBe('/projects/p/artifacts/a/v/3');
    expect(parseArtifactPagePath('/projects/p%201/artifacts/a%2Fb')).toEqual({
      projectId: 'p 1',
      id: 'a/b',
      seq: 0,
    });
    expect(parseArtifactPagePath('/projects/p/artifacts/a/v/3?x=1')).toEqual({
      projectId: 'p',
      id: 'a',
      seq: 3,
    });
  });
  it('refuses other paths', () => {
    expect(parseArtifactPagePath('/projects/p/artifacts/a/v/0')).toBeNull();
    expect(parseArtifactPagePath('/projects/p/artifacts/a/v/x')).toBeNull();
    expect(parseArtifactPagePath('/projects/p/artifacts')).toBeNull();
    expect(parseArtifactPagePath('/projects/p/artifacts/%E0')).toBeNull();
  });
});

describe('sha256Hex', () => {
  it('hashes the bytes', async () => {
    expect(await sha256Hex(new Blob(['abc']))).toBe(
      'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'
    );
  });
});

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: unknown;
}

function recordFetch(handler: (c: Call) => Response): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const c: Call = {
        url: String(input),
        method: init?.method ?? 'GET',
        headers: (init?.headers as Record<string, string>) ?? {},
        body: init?.body,
      };
      calls.push(c);
      return Promise.resolve(handler(c));
    })
  );
  return calls;
}

const json = (v: unknown, status = 200): Response =>
  new Response(JSON.stringify(v), { status, headers: { 'Content-Type': 'application/json' } });

describe('publishFiles', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('creates the version, uploads only what the hub asks for, then finalizes', async () => {
    const calls = recordFetch((c) => {
      if (c.method === 'POST' && c.url === '/api/v1/artifacts/a-1/versions') {
        return json(
          { artifact: { id: 'a-1' }, version: { seq: 4 }, upload: { required: ['docs/new.md'] } },
          201
        );
      }
      if (c.method === 'PUT') return new Response(null, { status: 204 });
      if (c.url.endsWith('/finalize'))
        return json({ artifact: { id: 'a-1' }, version: { seq: 4 } });
      return json({ error: { message: 'unexpected' } }, 500);
    });
    const progress: string[] = [];
    await publishFiles({
      artifactId: 'a-1',
      entry: 'docs/new.md',
      note: 'edit',
      files: [
        { path: 'docs/new.md', data: new Blob(['abc']) },
        { path: 'img/kept.png', size: 9, sha256: 'f'.repeat(64) },
      ],
      onProgress: (done, total, path) => progress.push(`${done}/${total} ${path}`),
    });
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'POST /api/v1/artifacts/a-1/versions',
      'PUT /api/v1/artifacts/a-1/versions/4/files/docs/new.md',
      'POST /api/v1/artifacts/a-1/versions/4/finalize',
    ]);
    expect(JSON.parse(calls[0].body as string)).toEqual({
      entry: 'docs/new.md',
      note: 'edit',
      files: [
        {
          path: 'docs/new.md',
          size: 3,
          sha256: 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
        },
        { path: 'img/kept.png', size: 9, sha256: 'f'.repeat(64) },
      ],
    });
    expect(calls[1].headers['X-Content-SHA256']).toBe(
      'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'
    );
    expect(progress).toEqual(['1/1 docs/new.md']);
  });

  it('sends title, key and scope only for a new artifact', async () => {
    const calls = recordFetch((c) => {
      if (c.method === 'POST' && c.url === '/api/v1/artifacts') {
        return json({ artifact: { id: 'n' }, version: { seq: 1 }, upload: { required: [] } }, 201);
      }
      return json({ artifact: { id: 'n' }, version: { seq: 1 } });
    });
    await publishFiles({
      scope: 'p-1',
      title: 'T',
      key: 'k',
      entry: 'a.md',
      files: [{ path: 'a.md', data: new Blob(['x']) }],
    });
    expect(JSON.parse(calls[0].body as string)).toMatchObject({
      scope: 'p-1',
      title: 'T',
      key: 'k',
    });
  });

  it('stops when the hub asks for a file it was not given the bytes of', async () => {
    recordFetch(() =>
      json({ artifact: { id: 'a' }, version: { seq: 2 }, upload: { required: ['kept.png'] } }, 201)
    );
    await expect(
      publishFiles({
        artifactId: 'a',
        entry: 'a.md',
        files: [
          { path: 'a.md', data: new Blob(['x']) },
          { path: 'kept.png', size: 1, sha256: 'e'.repeat(64) },
        ],
      })
    ).rejects.toThrow('kept.png');
  });

  it('reports the hub error of a failed upload', async () => {
    recordFetch((c) => {
      if (c.method === 'PUT')
        return json({ error: { code: 'too_large', message: 'too big' } }, 413);
      return json(
        { artifact: { id: 'a' }, version: { seq: 2 }, upload: { required: ['a.md'] } },
        201
      );
    });
    await expect(
      publishFiles({
        artifactId: 'a',
        entry: 'a.md',
        files: [{ path: 'a.md', data: new Blob(['x']) }],
      })
    ).rejects.toThrow(/a\.md: .*too big/);
  });
});

describe('publishFiles after a failure', () => {
  afterEach(() => vi.unstubAllGlobals());

  const files = (): { path: string; data: Blob }[] => [{ path: 'a.md', data: new Blob(['abc']) }];

  it('reports the pending version and resumes it on the next attempt', async () => {
    let putStatus = 500;
    const calls = recordFetch((c) => {
      if (c.method === 'POST' && c.url === '/api/v1/artifacts') {
        return json(
          { artifact: { id: 'n-1' }, version: { seq: 1 }, upload: { required: ['a.md'] } },
          201
        );
      }
      if (c.method === 'PUT') {
        return putStatus === 204
          ? new Response(null, { status: 204 })
          : json({ error: { code: 'internal', message: 'boom' } }, putStatus);
      }
      if (c.url.endsWith('/finalize'))
        return json({ artifact: { id: 'n-1' }, version: { seq: 1 } });
      return json({}, 500);
    });
    const req = { scope: 'p-1', title: 'T', entry: 'a.md', files: files() };
    let failure: unknown;
    try {
      await publishFiles(req);
    } catch (err) {
      failure = err;
    }
    expect(failure).toBeInstanceOf(PublishError);
    const pending = (failure as PublishError).pending!;
    expect(pending).toMatchObject({ artifactId: 'n-1', seq: 1, required: ['a.md'] });
    expect(publishErrorMessage(failure)).toContain('removed after 24 hours');

    putStatus = 204;
    await publishFiles({ ...req, files: files(), resume: pending });
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'POST /api/v1/artifacts',
      'PUT /api/v1/artifacts/n-1/versions/1/files/a.md',
      'PUT /api/v1/artifacts/n-1/versions/1/files/a.md',
      'POST /api/v1/artifacts/n-1/versions/1/finalize',
    ]);
  });

  it('starts a new version when the content changed since the failure', async () => {
    const calls = recordFetch((c) => {
      if (c.method === 'POST' && !c.url.endsWith('/finalize')) {
        return json({ artifact: { id: 'a' }, version: { seq: 3 }, upload: { required: [] } }, 201);
      }
      return json({ artifact: { id: 'a' }, version: { seq: 3 } });
    });
    await publishFiles({
      artifactId: 'a',
      entry: 'a.md',
      files: [{ path: 'a.md', data: new Blob(['changed']) }],
      resume: { artifactId: 'a', seq: 2, required: ['a.md'], fingerprint: 'old' },
    });
    expect(calls[0].url).toBe('/api/v1/artifacts/a/versions');
    expect(calls[1].url).toBe('/api/v1/artifacts/a/versions/3/finalize');
  });

  it('does not offer to resume a version that is no longer pending', async () => {
    recordFetch((c) => {
      if (c.url.endsWith('/finalize')) {
        return json({ error: { code: 'conflict', message: 'the version is not pending' } }, 409);
      }
      return json({ artifact: { id: 'a' }, version: { seq: 2 }, upload: { required: [] } }, 201);
    });
    const err = await publishFiles({ artifactId: 'a', entry: 'a.md', files: files() }).catch(
      (e: unknown) => e
    );
    expect((err as PublishError).pending).toBeNull();
    expect(publishErrorMessage(err)).not.toContain('24 hours');
  });

  it('keeps the version resumable when finalize finds files missing', async () => {
    recordFetch((c) => {
      if (c.url.endsWith('/finalize')) {
        return json({ error: { code: 'incomplete', message: '1 file(s) missing' } }, 409);
      }
      return json({ artifact: { id: 'a' }, version: { seq: 2 }, upload: { required: [] } }, 201);
    });
    const err = await publishFiles({ artifactId: 'a', entry: 'a.md', files: files() }).catch(
      (e: unknown) => e
    );
    expect((err as PublishError).pending).toMatchObject({ artifactId: 'a', seq: 2 });
  });
});

describe('listProjectArtifacts', () => {
  afterEach(() => vi.unstubAllGlobals());
  it("asks for the caller's artifacts homed in the project", async () => {
    const calls = recordFetch(() => json({ artifacts: [] }));
    await listProjectArtifacts('p 1', { q: 'rep', cursor: 'c1' });
    expect(calls[0].url).toBe('/api/v1/artifacts?mine=1&scope=p+1&q=rep&cursor=c1');
  });
});
