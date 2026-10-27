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
      nfs: { server: 'user:secretpw@filer', export: '/e', subPathRoot: 'r', healthy: true },
    });
    expect(rows['NFS server']).toBe('***@filer');
    expect(JSON.stringify(rows)).not.toContain('secretpw');
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
