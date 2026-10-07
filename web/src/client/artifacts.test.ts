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

import { describe, expect, it } from 'vitest';

import {
  artifactFileUrl,
  baseName,
  formatArtifactRef,
  formatBytes,
  isInlineType,
  orderArtifactRefs,
  rendererFor,
  type MessageArtifactRef,
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

  it('never renders active content inline', () => {
    expect(rendererFor('text/html')).toBe('download');
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

describe('message artifact helpers', () => {
  const A = '5f1c2d3e-0000-4000-8000-0000000000aa';
  const B = '5f1c2d3e-0000-4000-8000-0000000000bb';
  const ref = (id: string): MessageArtifactRef => ({
    ref: `scion://artifact/${id}`,
    id,
    available: false,
  });

  it('formats references', () => {
    expect(formatArtifactRef(A)).toBe(`scion://artifact/${A}`);
    expect(formatArtifactRef(A, 0)).toBe(`scion://artifact/${A}`);
    expect(formatArtifactRef(A, 4)).toBe(`scion://artifact/${A}@4`);
  });

  it('orders refs by first appearance in the body, case-insensitively, others last in input order', () => {
    const body = `see scion://artifact/${B.toUpperCase()} and scion://artifact/${A}@2`;
    const c = { ...ref('5f1c2d3e-0000-4000-8000-0000000000cc') };
    const d = { ...ref('5f1c2d3e-0000-4000-8000-0000000000dd') };
    expect(orderArtifactRefs([c, ref(A), d, ref(B)], body).map((r) => r.id)).toEqual([
      B,
      A,
      c.id,
      d.id,
    ]);
  });
});
