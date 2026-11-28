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

import { describe, it, expect, beforeAll, afterEach, vi } from 'vitest';

type ConfigHost = HTMLElement & {
  updateComplete: Promise<unknown>;
  populateForm(data: Record<string, unknown>): void;
  buildLayer1Payload(): Record<string, unknown>;
  buildSavePayload(): Record<string, unknown>;
  handleSave(): Promise<void>;
  settingsTier: 'db' | 'file';
  loading: boolean;
  activeTab: string;
  hubUploads: Record<string, string>;
  layer1Baseline: Record<string, unknown> | null;
  sectionKoanfPaths: Record<string, string[]> | null;
  loadedRevisions: Record<string, number>;
};

const UPLOAD_KOANF_PATHS = [
  'server.hub.uploads.max_attachment_size',
  'server.hub.uploads.max_workspace_file_size',
  'server.hub.uploads.chunk_bytes',
  'server.hub.uploads.session_idle_ttl',
  'server.hub.uploads.session_max_lifetime',
  'server.hub.uploads.max_sessions_per_user',
  'server.hub.uploads.max_pending_bytes_per_user',
  'server.hub.uploads.max_pending_bytes_hub',
];

describe('admin server config Uploads panel', () => {
  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({}), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
    await import('./admin-server-config.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  async function mount(
    uploads: Record<string, unknown> = {},
    revisions: Record<string, number> = { uploads: 2 }
  ): Promise<ConfigHost> {
    const el = document.createElement('scion-page-admin-server-config') as ConfigHost;
    document.body.appendChild(el);
    el.loading = false;
    el.activeTab = 'hub';
    el.settingsTier = 'db';
    el.populateForm({
      settings_tier: 'db',
      server: { hub: { uploads } },
    });
    el.sectionKoanfPaths = { uploads: UPLOAD_KOANF_PATHS };
    el.layer1Baseline = el.buildLayer1Payload();
    el.loadedRevisions = revisions;
    await el.updateComplete;
    return el;
  }

  it('renders all 8 server.hub.uploads fields and leaves max_workspace_file_size max unpinned', async () => {
    const el = await mount({
      max_attachment_size: 20971520,
      max_workspace_file_size: 104857600,
      chunk_bytes: 4194304,
      session_idle_ttl: '30m',
      session_max_lifetime: '12h',
      max_sessions_per_user: 8,
      max_pending_bytes_per_user: 4294967296,
      max_pending_bytes_hub: 34359738368,
    });

    const section = el.shadowRoot!.querySelector('[data-section="uploads"]');
    expect(section).not.toBeNull();

    for (const fullKey of UPLOAD_KOANF_PATHS) {
      expect(section!.querySelector(`[data-field="${fullKey}"]`)).not.toBeNull();
    }

    const wsInput = section!.querySelector(
      '[data-field="server.hub.uploads.max_workspace_file_size"]'
    )!;
    expect(wsInput.getAttribute('value')).toBe('104857600');
    expect(wsInput.hasAttribute('max')).toBe(false);

    const chunkInput = section!.querySelector('[data-field="server.hub.uploads.chunk_bytes"]')!;
    expect(chunkInput.getAttribute('value')).toBe('4194304');
    expect(chunkInput.getAttribute('min')).toBe('1048576');
    expect(chunkInput.getAttribute('max')).toBe('16777216');
  });

  it('omits unchanged fields, sends edited values, and sends null for cleared fields', async () => {
    const el = await mount({
      chunk_bytes: 4194304,
      session_idle_ttl: '30m',
      max_sessions_per_user: 8,
    });

    // Unchanged: omitted from payload.
    expect(el.buildSavePayload()).not.toHaveProperty('server');

    // Edit chunk_bytes, clear session_idle_ttl, leave max_sessions_per_user untouched.
    el.hubUploads = {
      ...el.hubUploads,
      chunk_bytes: '8388608',
      session_idle_ttl: '',
      max_pending_bytes_per_user: '1073741824',
    };

    const payload = el.buildSavePayload() as {
      server?: { hub?: { uploads?: Record<string, unknown> } };
      expected_revisions?: Record<string, number>;
    };
    expect(payload.server?.hub?.uploads).toEqual({
      chunk_bytes: 8388608,
      session_idle_ttl: null,
      max_pending_bytes_per_user: 1073741824,
    });
    expect(payload.expected_revisions).toEqual({ uploads: 2 });
  });

  it('displays response.warnings returned by PUT /api/v1/admin/server-config', async () => {
    const el = await mount({ max_pending_bytes_per_user: 2147483648 });
    el.hubUploads = {
      ...el.hubUploads,
      max_pending_bytes_per_user: '1048576',
    };

    const warningText =
      'uploads.max_pending_bytes_per_user (1048576) is below the largest effective per-file limit';
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request, init?: RequestInit) => {
        const method = (init?.method ?? 'GET').toUpperCase();
        if (method === 'PUT') {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                status: 'ok',
                warnings: [warningText],
                server: { hub: { uploads: { max_pending_bytes_per_user: 1048576 } } },
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({
              settings_tier: 'db',
              server: { hub: { uploads: { max_pending_bytes_per_user: 1048576 } } },
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      })
    );

    await el.handleSave();
    await el.updateComplete;

    const notice = el.shadowRoot!.querySelector('.save-warnings-notice');
    expect(notice).not.toBeNull();
    expect(notice!.textContent).toContain(warningText);
  });
});
