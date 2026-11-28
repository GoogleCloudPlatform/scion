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
  settingsTier: 'db' | 'file';
  loading: boolean;
  activeTab: string;
  artifactsSettings: Record<string, string>;
  layer1Baseline: Record<string, unknown> | null;
  sectionKoanfPaths: Record<string, string[]> | null;
  loadedRevisions: Record<string, number>;
};

const ARTIFACT_FIELD_KEYS = [
  'enabled',
  'default_retention_days',
  'gc_grace_hours',
  'link_default_ttl_hours',
  'link_max_ttl_hours',
  'max_file_bytes',
  'max_bundle_bytes',
  'max_files',
  'remote_images_enabled',
  'remote_image_max_count',
  'remote_image_max_bytes',
  'remote_image_fetch_timeout_s',
  'remote_image_total_budget_s',
];

describe('admin server config Artifacts panel', () => {
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
    artifacts?: Record<string, unknown>,
    revisions: Record<string, number> = { artifacts: 3 },
    settingsTier: 'db' | 'file' = 'db'
  ): Promise<ConfigHost> {
    const el = document.createElement('scion-page-admin-server-config') as ConfigHost;
    document.body.appendChild(el);
    el.loading = false;
    el.activeTab = 'hub-server';
    el.settingsTier = settingsTier;
    el.populateForm({
      settings_tier: settingsTier,
      ...(artifacts !== undefined ? { artifacts } : {}),
    });
    el.sectionKoanfPaths = {};
    el.layer1Baseline = el.buildLayer1Payload();
    el.loadedRevisions = revisions;
    await el.updateComplete;
    return el;
  }

  it('hides the panel in file-only mode', async () => {
    const el = await mount(undefined, {}, 'file');
    expect(el.shadowRoot!.querySelector('[data-section="artifacts"]')).toBeNull();
  });

  it('renders all 13 artifact settings fields and forward-only retention notice', async () => {
    const el = await mount({
      enabled: true,
      default_retention_days: 30,
      gc_grace_hours: 48,
      link_default_ttl_hours: 24,
      link_max_ttl_hours: 168,
      max_file_bytes: 10485760,
      max_bundle_bytes: 104857600,
      max_files: 50,
      remote_images_enabled: false,
      remote_image_max_count: 10,
      remote_image_max_bytes: 1048576,
      remote_image_fetch_timeout_s: 5,
      remote_image_total_budget_s: 20,
    });

    const section = el.shadowRoot!.querySelector('[data-section="artifacts"]');
    expect(section).not.toBeNull();

    const text = section!.textContent!.replace(/\s+/g, ' ');
    expect(text).toContain('apply going forward only');
    expect(text).toContain('existing artifacts with no expiry are unaffected');

    for (const key of ARTIFACT_FIELD_KEYS) {
      expect(section!.querySelector(`[data-field="artifacts.${key}"]`)).not.toBeNull();
    }

    const retention = section!.querySelector('[data-field="artifacts.default_retention_days"]')!;
    expect(retention.getAttribute('label')).toBe('Default Retention (days)');
    expect(retention.getAttribute('value')).toBe('30');
    expect(retention.getAttribute('min')).toBe('0');

    const remoteEnabled = section!.querySelector('[data-field="artifacts.remote_images_enabled"]')!;
    expect(remoteEnabled.getAttribute('value')).toBe('false');
  });

  it('omits artifacts from buildSavePayload when unchanged', async () => {
    const el = await mount({ default_retention_days: 30 }, { artifacts: 3 });
    const body = el.buildSavePayload();
    expect(body).not.toHaveProperty('artifacts');
    expect(body).not.toHaveProperty('expected_revisions');
  });

  it('sends only changed fields, preserves explicit 0/false, and sends null for cleared fields', async () => {
    const el = await mount(
      {
        default_retention_days: 30,
        gc_grace_hours: 48,
        enabled: true,
      },
      { artifacts: 5 }
    );

    el.artifactsSettings = {
      ...el.artifactsSettings,
      default_retention_days: '0',
      gc_grace_hours: '',
      enabled: 'false',
      max_files: '100',
    };

    const body = el.buildSavePayload() as {
      artifacts?: Record<string, unknown>;
      expected_revisions?: Record<string, number>;
    };
    expect(body.artifacts).toEqual({
      default_retention_days: 0,
      gc_grace_hours: null,
      enabled: false,
      max_files: 100,
    });
    expect(body.expected_revisions).toEqual({ artifacts: 5 });
  });

  it('defaults expected_revisions.artifacts to 0 when unstored', async () => {
    const el = await mount(undefined, {});
    el.artifactsSettings = {
      ...el.artifactsSettings,
      default_retention_days: '14',
    };

    const body = el.buildSavePayload() as {
      artifacts?: Record<string, unknown>;
      expected_revisions?: Record<string, number>;
    };
    expect(body.artifacts).toEqual({ default_retention_days: 14 });
    expect(body.expected_revisions).toEqual({ artifacts: 0 });
  });

  it('shows validation errors and disables Save on invalid range or cross-field values', async () => {
    const el = await mount({});
    el.artifactsSettings = {
      ...el.artifactsSettings,
      default_retention_days: '-1',
      gc_grace_hours: '12',
      link_default_ttl_hours: '200',
      link_max_ttl_hours: '100',
      remote_image_fetch_timeout_s: '40',
      remote_image_total_budget_s: '20',
    };
    await el.updateComplete;

    const alert = el.shadowRoot!.querySelector('[data-artifacts-validation-error]');
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('Default Retention (days) must be at least 0');
    expect(alert!.textContent).toContain('Blob GC Grace Period (hours) must be at least 24');
    expect(alert!.textContent).toContain('Default Share Link TTL cannot exceed Max Share Link TTL');
    expect(alert!.textContent).toContain(
      'Remote Image Total Budget cannot be less than Remote Image Fetch Timeout'
    );

    const blockedBanner = el.shadowRoot!.querySelector('.artifacts-blocked');
    expect(blockedBanner).not.toBeNull();
  });
});
