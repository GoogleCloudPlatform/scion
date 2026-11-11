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

import { describe, it, expect, vi, afterEach } from 'vitest';

vi.mock('../utils/toast.js', () => ({ showToast: vi.fn() }));

import { showToast } from '../utils/toast.js';
import {
  decodeBase64Text,
  entryStateLabel,
  fetchPendingSecretCount,
  pendingCount,
  pendingEffectText,
  pendingOverridesText,
  pendingReplacesText,
  pendingSecretsHintText,
  pendingWasReplaced,
  pendingWriterText,
  secretEntries,
  showPendingSecretsStartHint,
  valueLengthText,
  type PendingSecret,
} from './pending-secrets.js';
import type { Secret } from '../shared/types.js';

function approved(key: string, extra: Partial<Secret> = {}): Secret {
  return {
    id: `s-${key}`,
    key,
    type: 'environment',
    scope: 'user',
    scopeId: 'u-1',
    injectionMode: 'always',
    version: 2,
    created: '2026-11-01T10:00:00Z',
    updated: '2026-11-02T10:00:00Z',
    ...extra,
  };
}

function pending(key: string, extra: Partial<PendingSecret> = {}): PendingSecret {
  return {
    id: `p-${key}`,
    key,
    type: 'environment',
    target: key,
    scope: 'user',
    allowProgeny: false,
    valueLen: 12,
    agentId: 'agent-1',
    agentSlug: 'helper',
    projectId: 'proj-1',
    projectName: 'alpha',
    created: '2026-11-09T10:00:00Z',
    updated: '2026-11-09T10:00:00Z',
    expiresAt: '2026-11-16T10:00:00Z',
    replacesExisting: false,
    state: 'pending',
    overridesProjectSecrets: [],
    ...extra,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('secretEntries', () => {
  it('lists pending keys first, then approved keys, each sorted by key', () => {
    const entries = secretEntries({
      items: [
        { state: 'approved', key: 'A_KEY', secret: approved('A_KEY') },
        { state: 'pending', key: 'B_KEY', pending: pending('B_KEY') },
        { state: 'approved', key: 'C_KEY', secret: approved('C_KEY') },
        { state: 'pending', key: 'Z_KEY', pending: pending('Z_KEY') },
      ],
    });
    expect(entries.map((e) => e.key)).toEqual(['B_KEY', 'Z_KEY', 'A_KEY', 'C_KEY']);
  });

  it('joins an approved and a pending secret of one key into one entry', () => {
    const entries = secretEntries({
      items: [
        { state: 'approved', key: 'TOKEN', secret: approved('TOKEN') },
        { state: 'pending', key: 'TOKEN', pending: pending('TOKEN', { replacesExisting: true }) },
      ],
    });
    expect(entries).toHaveLength(1);
    expect(entries[0]?.approved?.key).toBe('TOKEN');
    expect(entries[0]?.pending?.id).toBe('p-TOKEN');
    expect(entryStateLabel(entries[0]!)).toBe('Approved, replacement pending');
    expect(pendingCount(entries)).toBe(1);
  });

  it('reads the approved secrets of a response without items', () => {
    expect(secretEntries({ secrets: [approved('X')] }).map((e) => e.key)).toEqual(['X']);
    expect(secretEntries([approved('Y')]).map((e) => e.key)).toEqual(['Y']);
  });

  it('labels approved-only and pending-only entries', () => {
    expect(entryStateLabel({ key: 'K', approved: approved('K') })).toBe('Approved');
    expect(entryStateLabel({ key: 'K', pending: pending('K') })).toBe('Pending');
  });
});

describe('pending secret text', () => {
  it('names the writing agent and project with their ids', () => {
    expect(pendingWriterText(pending('K'))).toBe(
      'Written by agent helper (agent-1) in project alpha (proj-1)'
    );
  });

  it('adds agents they create to the effect only with progeny access', () => {
    expect(pendingEffectText(pending('K'))).toBe('Delivered to your agents in all projects');
    expect(pendingEffectText(pending('K', { allowProgeny: true }))).toBe(
      'Delivered to your agents in all projects and to agents they create'
    );
    expect(pendingEffectText(pending('K', { allowProgeny: true }), false)).toBe(
      'Delivered to your agents in all projects'
    );
  });

  it('lists the projects whose project secret it overrides', () => {
    expect(pendingOverridesText(pending('K'))).toBe('');
    expect(
      pendingOverridesText(
        pending('K', { overridesProjectSecrets: [{ projectId: 'p1', projectName: 'P' }] })
      )
    ).toBe('Overrides project secret K in project P');
    expect(
      pendingOverridesText(
        pending('K', {
          overridesProjectSecrets: [
            { projectId: 'p1', projectName: 'P' },
            { projectId: 'p2', projectName: 'Q' },
          ],
        })
      )
    ).toBe('Overrides project secret K in projects P, Q');
  });

  it('describes the secret it replaces, with who last set it and when', () => {
    const fmt = (iso: string): string => `<${iso}>`;
    expect(pendingReplacesText(pending('K'), approved('K'), fmt)).toBe('');
    expect(
      pendingReplacesText(
        pending('K', { replacesExisting: true }),
        approved('K', { updatedBy: 'agent:agent-9' }),
        fmt
      )
    ).toBe('Replaces your existing secret (last set by agent:agent-9, <2026-11-02T10:00:00Z>)');
    expect(pendingReplacesText(pending('K', { replacesExisting: true }), undefined, fmt)).toBe(
      'Replaces your existing secret'
    );
  });

  it('reports a replacement only when updated differs from created', () => {
    expect(pendingWasReplaced(pending('K'))).toBe(false);
    expect(pendingWasReplaced(pending('K', { updated: '2026-11-10T10:00:00Z' }))).toBe(true);
  });

  it('formats the value length and the start hint', () => {
    expect(valueLengthText(1)).toBe('1 byte');
    expect(valueLengthText(40)).toBe('40 bytes');
    expect(pendingSecretsHintText(1)).toBe('1 secret awaits approval in Profile > Secrets.');
    expect(pendingSecretsHintText(3)).toBe('3 secrets await approval in Profile > Secrets.');
  });
});

describe('decodeBase64Text', () => {
  it('decodes UTF-8 text', () => {
    const b64 = btoa(
      Array.from(new TextEncoder().encode('test-value-1 ✓'), (b) => String.fromCharCode(b)).join('')
    );
    expect(decodeBase64Text(b64)).toBe('test-value-1 ✓');
  });

  it('returns null for bytes that are not UTF-8 and for invalid base64', () => {
    expect(decodeBase64Text(btoa('\xff\xfe\xfd'))).toBeNull();
    expect(decodeBase64Text('%%%')).toBeNull();
  });
});

describe('fetchPendingSecretCount and the start hint', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.mocked(showToast).mockClear();
  });

  it('counts the pending secrets from the owner list', async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(jsonResponse({ pendingSecrets: [pending('A'), pending('B')] }))
    );
    vi.stubGlobal('fetch', fetchMock);
    expect(await fetchPendingSecretCount()).toBe(2);
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/pending-secrets',
      expect.objectContaining({ credentials: 'include' })
    );
  });

  it('returns null when the list cannot be read', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ error: { code: 'forbidden' } }, 403)))
    );
    expect(await fetchPendingSecretCount()).toBeNull();
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('offline')))
    );
    expect(await fetchPendingSecretCount()).toBeNull();
  });

  it('shows the hint only when secrets are pending', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ pendingSecrets: [] })))
    );
    await showPendingSecretsStartHint();
    expect(showToast).not.toHaveBeenCalled();

    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ pendingSecrets: [pending('A'), pending('B')] })))
    );
    await showPendingSecretsStartHint();
    expect(showToast).toHaveBeenCalledWith(
      '2 secrets await approval in Profile > Secrets.',
      'primary'
    );
  });
});
