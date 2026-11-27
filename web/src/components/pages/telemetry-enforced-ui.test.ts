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
import {
  TELEMETRY_ENFORCEMENT_FLAG,
  setFeatureFlag,
  resetServerFlagStateForTests,
} from '../../utils/feature-flags.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('Telemetry enforcement UI (P3b)', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    setFeatureFlag(TELEMETRY_ENFORCEMENT_FLAG, false);
    resetServerFlagStateForTests();
    document.body.innerHTML = '';
  });

  describe('agent-config-form when telemetryLockedByHub is true', () => {
    beforeEach(async () => {
      await import('../shared/agent-config-form.js');
    });

    it('disables the telemetry select, hides Disabled, links hub lock reason via help-text (O4), and emits no telemetry patch', async () => {
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
      expect(select.getAttribute('help-text')).toBe('Required by hub operator');

      const options = Array.from(select.querySelectorAll('sl-option')).map((o) =>
        o.getAttribute('value')
      );
      expect(options).toEqual(['true']);

      expect(form.collectConfigPatch()).toEqual({});
    });
  });

  describe('agent-create shares one /settings/resolved fetch (O2)', () => {
    beforeEach(async () => {
      await import('./agent-create.js');
    });

    it('fetches /settings/resolved once per project and propagates telemetryLockedByHub to the form', async () => {
      let resolvedFetches = 0;
      vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (url.includes('/api/v1/projects?')) {
          return Promise.resolve(jsonResponse({ projects: [{ id: 'p1', name: 'P1' }] }));
        }
        if (url.endsWith('/api/v1/projects/p1/settings/resolved')) {
          resolvedFetches += 1;
          return Promise.resolve(
            jsonResponse({
              project: { defaultTemplate: 'claude' },
              settings: {
                'scion.io/telemetry-enabled': {
                  projectSet: false,
                  projectValue: null,
                  hubDefault: 'present',
                  hubValue: true,
                  lockedBy: 'hub',
                  effective: true,
                  storedValue: null,
                },
                'scion.io/default-max-turns': {
                  projectSet: false,
                  projectValue: null,
                  hubDefault: 'absent',
                  hubValue: null,
                  ceiling: 50,
                },
              },
            })
          );
        }
        return Promise.resolve(jsonResponse({}));
      });

      const el = document.createElement('scion-page-agent-create') as HTMLElement & {
        updateComplete: Promise<boolean>;
      };
      document.body.appendChild(el);

      await vi.waitFor(() => {
        const form = el.shadowRoot?.querySelector(
          'scion-agent-config-form'
        ) as ScionAgentConfigForm | null;
        expect(form?.telemetryLockedByHub).toBe(true);
      });

      expect(resolvedFetches).toBe(1);
    });
  });

  describe('project-settings when telemetry is lockedBy hub', () => {
    beforeEach(async () => {
      await import('./project-settings.js');
    });

    it('renders disabled dropdown with linked help-text (O4) and omits telemetryEnabled from PUT', async () => {
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
      };
      el.projectId = 'proj-1';
      document.body.appendChild(el);

      await vi.waitFor(() => {
        const select = el.shadowRoot?.querySelector('[data-testid="project-telemetry-select"]');
        expect(select?.getAttribute('help-text')).toContain('Required by hub operator');
      });

      const select = el.shadowRoot!.querySelector(
        '[data-testid="project-telemetry-select"]'
      ) as HTMLElement & { value: string; disabled: boolean };
      expect(select.hasAttribute('disabled') || select.disabled).toBe(true);
      expect(select.getAttribute('value') || select.value).toBe('enabled');
      expect(select.getAttribute('help-text')).toContain('Required by hub operator');
      expect(select.getAttribute('help-text')).toContain(
        'Project setting (Disabled) is overridden'
      );

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

  describe('admin-server-config enforce telemetry gating (R1) and L1/O1 warning', () => {
    beforeEach(async () => {
      await import('./admin-server-config.js');
    });

    it('R1: hides enforce switch when experiment is off and enforced is not stored', async () => {
      setFeatureFlag(TELEMETRY_ENFORCEMENT_FLAG, false);
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
              telemetry: { enabled: true },
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
        expect(el.shadowRoot?.querySelector('sl-tab-group')).not.toBeNull();
      });

      expect(el.shadowRoot!.querySelector('[data-testid="telemetry-enforced-switch"]')).toBeNull();
      expect(
        el.shadowRoot!.querySelector('[data-testid="telemetry-enforcement-mismatch"]')
      ).toBeNull();
    });

    it('R1: shows mismatch alert (not checked switch) when experiment is off and enforced:true is stored', async () => {
      setFeatureFlag(TELEMETRY_ENFORCEMENT_FLAG, false);
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
              telemetry: { enabled: true, enforced: true },
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
        expect(
          el.shadowRoot?.querySelector('[data-testid="telemetry-enforcement-mismatch"]')
        ).not.toBeNull();
      });

      expect(el.shadowRoot!.querySelector('[data-testid="telemetry-enforced-switch"]')).toBeNull();
      const alert = el.shadowRoot!.querySelector('[data-testid="telemetry-enforcement-mismatch"]');
      expect(alert?.textContent).toContain('telemetry.enforced');
      expect(alert?.textContent).toContain(TELEMETRY_ENFORCEMENT_FLAG);
    });

    it('R1 + O1: with experiment on, disables enforce switch unless enabled=true, shows L1 warning, and sends enforced:false when enabling telemetry with an invalid stored row', async () => {
      setFeatureFlag(TELEMETRY_ENFORCEMENT_FLAG, true);
      const putBodies: Array<Record<string, unknown>> = [];
      vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const method = (init?.method || 'GET').toUpperCase();
        if (url.includes('/api/v1/admin/server-config/schema')) {
          return Promise.resolve(jsonResponse({ sections: {} }));
        }
        if (url.includes('/api/v1/admin/server-config') && method === 'PUT') {
          const rawBody = typeof init?.body === 'string' ? init.body : '{}';
          putBodies.push(JSON.parse(rawBody) as Record<string, unknown>);
          return Promise.resolve(
            jsonResponse({
              settings_tier: 'db',
              schema_version: '1',
              telemetry: { enabled: true, enforced: false },
            })
          );
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
        telemetryEnabled: boolean | null;
        handleSave: () => Promise<void>;
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

      // O1: Admin enables telemetry without touching the enforce switch, then saves.
      // Payload MUST explicitly send enforced: false so the stray stored enforced=true
      // is cleared rather than silently armed.
      el.telemetryEnabled = true;
      await el.updateComplete;
      await el.handleSave();

      expect(putBodies.length).toBe(1);
      const tel = putBodies[0].telemetry as Record<string, unknown>;
      expect(tel.enabled).toBe(true);
      expect(tel.enforced).toBe(false);
    });
  });
});
