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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { ScionAgentConfigForm } from '../shared/agent-config-form.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('Telemetry enforcement UI (P3b)', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  describe('agent-config-form when telemetryLockedByHub is true', () => {
    beforeEach(async () => {
      await import('../shared/agent-config-form.js');
    });

    it('disables the telemetry select, hides Disabled, shows hub lock note, and emits no telemetry patch', async () => {
      const form = document.createElement('scion-agent-config-form') as ScionAgentConfigForm;
      form.mode = 'create';
      form.telemetryLockedByHub = true;
      document.body.appendChild(form);
      await form.updateComplete;

      const field = form.shadowRoot!.querySelector('[data-key="config.telemetry"]');
      expect(field).not.toBeNull();
      const select = field!.querySelector('sl-select') as HTMLElement & {
        disabled: boolean;
        value: string;
      };
      expect(select).not.toBeNull();
      expect(select.hasAttribute('disabled') || select.disabled).toBe(true);
      expect(select.value).toBe('true');

      const options = Array.from(select.querySelectorAll('sl-option')).map((o) =>
        o.getAttribute('value')
      );
      expect(options).toEqual(['true']);

      const note = field!.querySelector('[data-testid="telemetry-locked-note"]');
      expect(note?.textContent).toContain('Required by hub operator');

      expect(form.collectConfigPatch()).toEqual({});
    });
  });

  describe('project-settings when telemetry is lockedBy hub', () => {
    beforeEach(async () => {
      await import('./project-settings.js');
    });

    it('renders disabled dropdown with override helper text and omits telemetryEnabled from PUT', async () => {
      const putBodies: Array<Record<string, unknown>> = [];
      vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const method = (init?.method || 'GET').toUpperCase();
        if (url.includes('/settings/resolved')) {
          return Promise.resolve(
            jsonResponse({
              project: { telemetryEnabled: false, defaultTemplate: 'claude' },
              settings: {
                'scion.io/telemetry-enabled': {
                  projectSet: true,
                  projectValue: 'false',
                  hubDefault: 'present',
                  hubValue: true,
                  lockedBy: 'hub',
                  effective: true,
                  storedValue: false,
                },
              },
            })
          );
        }
        if (url.includes('/settings') && method === 'PUT') {
          const rawBody = typeof init?.body === 'string' ? init.body : '{}';
          putBodies.push(JSON.parse(rawBody) as Record<string, unknown>);
          return Promise.resolve(
            jsonResponse({ telemetryEnabled: false, defaultTemplate: 'claude' })
          );
        }
        if (url.endsWith('/projects/proj-1')) {
          return Promise.resolve(
            jsonResponse({
              id: 'proj-1',
              name: 'Test Project',
              slug: 'test-project',
              _capabilities: { actions: ['update'] },
            })
          );
        }
        return Promise.resolve(jsonResponse({}));
      });

      const el = document.createElement('scion-page-project-settings') as HTMLElement & {
        projectId: string;
        updateComplete: Promise<boolean>;
        handleSaveConfig?: () => Promise<void>;
      };
      el.projectId = 'proj-1';
      document.body.appendChild(el);

      await vi.waitFor(() => {
        const help = el.shadowRoot?.querySelector('[data-testid="project-telemetry-locked-help"]');
        expect(help).not.toBeNull();
      });

      const select = el.shadowRoot!.querySelector(
        '[data-testid="project-telemetry-select"]'
      ) as HTMLElement & { value: string; disabled: boolean };
      expect(select).not.toBeNull();
      expect(select.hasAttribute('disabled') || select.disabled).toBe(true);
      expect(select.getAttribute('value') || select.value).toBe('enabled');

      const help = el.shadowRoot!.querySelector('[data-testid="project-telemetry-locked-help"]');
      const helpText = (help?.textContent ?? '').replace(/\s+/g, ' ').trim();
      expect(helpText).toContain('Required by hub operator');
      expect(helpText).toContain('Project setting (Disabled) is overridden');

      // Trigger save and verify telemetryEnabled is omitted from PUT payload.
      const saveBtn = el.shadowRoot!.querySelector(
        'sl-button[variant="primary"]'
      ) as HTMLElement | null;
      if (saveBtn) {
        saveBtn.click();
        await vi.waitFor(() => {
          expect(putBodies.length).toBeGreaterThan(0);
        });
        expect(putBodies[0]).not.toHaveProperty('telemetryEnabled');
      }
    });
  });

  describe('admin-server-config enforce telemetry toggle and L1 warning', () => {
    beforeEach(async () => {
      await import('./admin-server-config.js');
    });

    it('disables enforce toggle unless telemetry.enabled is true and surfaces telemetry_enforced_warning', async () => {
      vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (url.includes('/api/v1/admin/server-config/schema')) {
          return Promise.resolve(jsonResponse({ sections: {} }));
        }
        if (url.includes('/api/v1/admin/server-config')) {
          return Promise.resolve(
            jsonResponse({
              settings_tier: 'db',
              schema_version: '1',
              telemetry: { enabled: false, enforced: false },
              telemetry_enforced_warning:
                'stored telemetry settings have enforced=true but failed validation',
            })
          );
        }
        return Promise.resolve(jsonResponse({}));
      });

      const el = document.createElement('scion-page-admin-server-config') as HTMLElement & {
        updateComplete: Promise<boolean>;
      };
      document.body.appendChild(el);

      await vi.waitFor(() => {
        const sw = el.shadowRoot?.querySelector('[data-testid="telemetry-enforced-switch"]');
        expect(sw).not.toBeNull();
      });

      const sw = el.shadowRoot!.querySelector(
        '[data-testid="telemetry-enforced-switch"]'
      ) as HTMLElement & { disabled: boolean };
      expect(sw.hasAttribute('disabled') || sw.disabled).toBe(true);
      expect(sw.textContent).toContain('Enforce telemetry for all agents');

      const warn = el.shadowRoot!.querySelector('[data-testid="telemetry-enforced-warning"]');
      expect(warn).not.toBeNull();
      expect(warn?.textContent).toContain('failed validation');
    });
  });
});
