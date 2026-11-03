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

/** Display helpers for a broker's reported storage layout. */

import { describe, it, expect } from 'vitest';

import {
  mismatchLines,
  redactMismatchLine,
  redactUserinfo,
  storageRows,
} from './broker-storage.js';
import type { BrokerWorkspaceStorage } from './types.js';

function rowMap(desc: BrokerWorkspaceStorage | undefined): Record<string, string> {
  return Object.fromEntries(storageRows(desc).map((r) => [r.label, r.value]));
}

describe('redactUserinfo', () => {
  it('keeps a value without userinfo as it is', () => {
    expect(redactUserinfo('filer.example.internal')).toBe('filer.example.internal');
    expect(redactUserinfo('/vol/scion')).toBe('/vol/scion');
    expect(redactUserinfo('')).toBe('');
  });

  it('replaces userinfo up to the last "@"', () => {
    expect(redactUserinfo('user:pw@filer')).toBe('***@filer');
    expect(redactUserinfo('a@b@filer:2049')).toBe('***@filer:2049');
    expect(redactUserinfo('user:p/w@filer/path')).toBe('***@filer/path');
  });

  it('keeps a leading scheme', () => {
    expect(redactUserinfo('nfs://user:pw@filer/vol')).toBe('nfs://***@filer/vol');
  });
});

describe('mismatchLines', () => {
  it('redacts each quoted value and keeps the rest of the line', () => {
    expect(redactMismatchLine('nfs server: hub "u:pw@filer", broker "other"')).toBe(
      'nfs server: hub "***@filer", broker "other"'
    );
    expect(redactMismatchLine('shared dirs: hub nfs, broker local')).toBe(
      'shared dirs: hub nfs, broker local'
    );
  });

  it('redacts a quoted value that contains an escaped quote as a whole', () => {
    const out = redactMismatchLine('nfs server: hub "filer", broker "user:p\\"w@filer"');
    expect(out).toBe('nfs server: hub "filer", broker "***@filer"');
    expect(out).not.toContain('user');
    expect(out).not.toContain('p\\"w');
  });

  it('ends a quoted value at a quote that follows an escaped backslash', () => {
    expect(redactMismatchLine('nfs server: hub "a\\\\", broker "u:pw@filer"')).toBe(
      'nfs server: hub "a\\\\", broker "***@filer"'
    );
  });

  it('redacts the rest of a mismatch line after an unpaired quote', () => {
    const out = redactMismatchLine('nfs server: hub "filer", broker "user:pw@filer');
    expect(out).toBe('nfs server: hub "***');
    expect(out).not.toContain('user');
    expect(out).not.toContain(':pw');
  });

  it('redacts the rest of a mismatch line after an unpaired quote with an escaped quote', () => {
    const out = redactMismatchLine('nfs server: hub "filer", broker "a\\"b@c');
    expect(out).toBe('nfs server: hub "***');
    expect(out).not.toContain('b@c');
  });

  it('redacts the rest of a mismatch line when the unpaired quote is not the last one', () => {
    const out = redactMismatchLine('x: hub "u:p@a, broker "v:q@b"');
    expect(out).toBe('x: hub "***');
    expect(out).not.toContain('u:p');
    expect(out).not.toContain('v:q');
  });

  it('redacts the rest of a mismatch line after an unpaired quote followed by a pair', () => {
    const out = redactMismatchLine('x: hub "u:p@a, broker "filer", other "v:q@b"');
    expect(out).toBe('x: hub "***');
    expect(out).not.toContain('u:p');
    expect(out).not.toContain('v:q');
  });

  it('redacts the rest of a mismatch line when an escaped quote leaves a value unclosed', () => {
    const out = redactMismatchLine('x: hub "u:p@a\\", broker "v:q@b"');
    expect(out).toBe('x: hub "***');
    expect(out).not.toContain('u:p');
    expect(out).not.toContain('v:q');
  });

  it('pairs a backslash before a quote outside a value as an opening quote', () => {
    const out = redactMismatchLine('x: a \\"u:p@a" hub "v:q@b"');
    expect(out).toBe('x: a \\"***@a" hub "***@b"');
    expect(out).not.toContain('u:p');
    expect(out).not.toContain('v:q');
  });

  it('redacts the rest of a mismatch line that ends in a backslash inside a value', () => {
    const out = redactMismatchLine('x: hub "filer", broker "u:p@b\\');
    expect(out).toBe('x: hub "***');
    expect(out).not.toContain('u:p');
  });

  it('redacts the rest of a mismatch line with a backslash before a line break inside a value', () => {
    const out = redactMismatchLine('x: hub "u:p@b\\\n", broker "v:q@c"');
    expect(out).toBe('x: hub "***');
    expect(out).not.toContain('u:p');
    expect(out).not.toContain('v:q');
  });

  it('is empty for null, undefined or no lines', () => {
    expect(mismatchLines(null)).toEqual([]);
    expect(mismatchLines(undefined)).toEqual([]);
    expect(mismatchLines([])).toEqual([]);
  });
});

describe('storageRows', () => {
  it('names the NFS share fields and omits the export marker', () => {
    const rows = rowMap({
      backend: 'nfs',
      nfs: {
        server: 'filer',
        export: '/vol/scion',
        subPathRoot: 'projects',
        healthy: false,
        exportId: 'export-marker-0001',
      },
      homes: 'nfs',
    });
    expect(rows).toEqual({
      Backend: 'nfs',
      'NFS server': 'filer',
      'NFS export': '/vol/scion',
      'Subpath root': 'projects',
      'Share health': 'not healthy',
      Homes: 'nfs',
    });
  });

  it('redacts userinfo in a reported server', () => {
    const rows = rowMap({
      backend: 'nfs',
      nfs: { server: 'user:pw123@filer', export: '/e', subPathRoot: 'r', healthy: true },
    });
    expect(rows['NFS server']).toBe('***@filer');
    expect(JSON.stringify(rows)).not.toContain('pw123');
  });

  it('shows the volume fields and a split shared-dir placement', () => {
    expect(
      rowMap({
        backend: 'gke-shared-volume',
        volume: { claimName: 'scion-pvc', subPathRoot: 'projects' },
        sharedDirs: 'local',
        homes: 'local',
      })
    ).toEqual({
      Backend: 'gke-shared-volume',
      'Volume claim': 'scion-pvc',
      'Subpath root': 'projects',
      'Shared dirs': 'local',
      Homes: 'local',
    });
  });

  it('says homes not reported for an older broker', () => {
    expect(rowMap({ backend: 'local' })).toEqual({ Backend: 'local', Homes: 'not reported' });
  });

  it('is empty when nothing was reported', () => {
    expect(storageRows(undefined)).toEqual([]);
    expect(storageRows({ backend: '' })).toEqual([]);
  });
});
