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
import {
  TOKENOMICS_TRAJECTORY_CAPTURE_FLAG,
  setFeatureFlag,
  resetServerFlagStateForTests,
} from '../../utils/feature-flags.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('Trajectory capture policy UI (P5.2b)', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
    resetServerFlagStateForTests();
    document.body.innerHTML = '';
  });

  describe('project-settings trajectory capture control', () => {
    beforeEach(async () => {
      await import('./project-settings.js');
    });

    it('hides trajectory capture control and omits trajectoryCapture from PUT when experiment is off', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
      const putBodies: Array<Record<string, unknown>> = [];
      vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const method = (init?.method || 'GET').toUpperCase();
        if (url.includes('/settings/resolved')) {
          return Promise.resolve(
            jsonResponse({
              project: { defaultTemplate: 'claude' },
              settings: {},
            })
          );
        }
        if (url.includes('/settings') && method === 'PUT') {
          const rawBody = typeof init?.body === 'string' ? init.body : '{}';
          putBodies.push(JSON.parse(rawBody) as Record<string, unknown>);
          return Promise.resolve(jsonResponse({ defaultTemplate: 'claude' }));
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
        expect(
          el.shadowRoot?.querySelector('[data-testid="project-telemetry-select"]')
        ).not.toBeNull();
      });

      expect(
        el.shadowRoot!.querySelector('[data-testid="project-trajectory-capture-select"]')
      ).toBeNull();

      const saveBtn = el.shadowRoot!.querySelector(
        'sl-button[variant="primary"]'
      ) as HTMLElement | null;
      expect(saveBtn).not.toBeNull();
      saveBtn!.click();
      await vi.waitFor(() => {
        expect(putBodies.length).toBeGreaterThan(0);
      });
      expect(putBodies[0]).not.toHaveProperty('trajectoryCapture');
    });

    it('renders editable select when experiment is on and unlocked, and sends trajectoryCapture on PUT', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
      const putBodies: Array<Record<string, unknown>> = [];
      vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const method = (init?.method || 'GET').toUpperCase();
        if (url.includes('/settings/resolved')) {
          return Promise.resolve(
            jsonResponse({
              project: { trajectoryCapture: 'metadata_only', defaultTemplate: 'claude' },
              settings: {},
            })
          );
        }
        if (url.includes('/settings') && method === 'PUT') {
          const rawBody = typeof init?.body === 'string' ? init.body : '{}';
          putBodies.push(JSON.parse(rawBody) as Record<string, unknown>);
          return Promise.resolve(
            jsonResponse({
              trajectoryCapture: 'off',
              trajectory_capture: { policy: 'off', enforced_by_hub: false },
            })
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
        const select = el.shadowRoot?.querySelector(
          '[data-testid="project-trajectory-capture-select"]'
        );
        expect(select).not.toBeNull();
      });

      const select = el.shadowRoot!.querySelector(
        '[data-testid="project-trajectory-capture-select"]'
      ) as HTMLElement & { value: string; disabled: boolean };
      expect(select.hasAttribute('disabled')).toBe(false);
      expect(select.getAttribute('value') || select.value).toBe('metadata_only');

      select.value = 'off';
      select.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
      await el.updateComplete;

      const saveBtn = el.shadowRoot!.querySelector(
        'sl-button[variant="primary"]'
      ) as HTMLElement | null;
      expect(saveBtn).not.toBeNull();
      saveBtn!.click();
      await vi.waitFor(() => {
        expect(putBodies.length).toBeGreaterThan(0);
      });
      expect(putBodies[0].trajectoryCapture).toBe('off');
    });

    it('disables select, displays full and "Enforced by Hub policy", and omits trajectoryCapture from PUT when locked', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
      const putBodies: Array<Record<string, unknown>> = [];
      vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const method = (init?.method || 'GET').toUpperCase();
        if (url.includes('/settings/resolved')) {
          return Promise.resolve(
            jsonResponse({
              project: { trajectoryCapture: 'off', defaultTemplate: 'claude' },
              settings: {
                'scion.io/trajectory-capture': {
                  projectSet: true,
                  projectValue: 'off',
                  hubDefault: 'absent',
                  hubValue: null,
                  lockedBy: 'hub',
                  effective: 'full',
                  storedValue: 'off',
                },
              },
            })
          );
        }
        if (url.includes('/settings') && method === 'PUT') {
          const rawBody = typeof init?.body === 'string' ? init.body : '{}';
          putBodies.push(JSON.parse(rawBody) as Record<string, unknown>);
          return Promise.resolve(
            jsonResponse({
              defaultTemplate: 'claude',
              trajectory_capture: { policy: 'full', enforced_by_hub: true },
            })
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
        const select = el.shadowRoot?.querySelector(
          '[data-testid="project-trajectory-capture-select"]'
        );
        expect(select?.getAttribute('help-text')).toContain('Enforced by Hub policy');
      });

      const select = el.shadowRoot!.querySelector(
        '[data-testid="project-trajectory-capture-select"]'
      ) as HTMLElement & { value: string; disabled: boolean };
      expect(select.hasAttribute('disabled') || select.disabled).toBe(true);
      expect(select.getAttribute('value') || select.value).toBe('full');
      expect(select.getAttribute('help-text')).toContain('Enforced by Hub policy');

      const saveBtn = el.shadowRoot!.querySelector(
        'sl-button[variant="primary"]'
      ) as HTMLElement | null;
      expect(saveBtn).not.toBeNull();
      saveBtn!.click();
      await vi.waitFor(() => {
        expect(putBodies.length).toBeGreaterThan(0);
      });
      expect(putBodies[0]).not.toHaveProperty('trajectoryCapture');
    });
  });

  describe('admin-server-config trajectory opt-out lock toggle', () => {
    beforeEach(async () => {
      await import('./admin-server-config.js');
    });

    it('hides opt-out lock switch when experiment is off and default is true', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
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

      expect(
        el.shadowRoot!.querySelector('[data-testid="telemetry-trajectory-allow-opt-out-switch"]')
      ).toBeNull();
      expect(
        el.shadowRoot!.querySelector('[data-testid="telemetry-trajectory-opt-out-mismatch"]')
      ).toBeNull();
    });

    it('renders switch checked by default when experiment is on and sends allow_project_opt_out=false on save', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
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
              telemetry: { enabled: true, trajectory: { allow_project_opt_out: false } },
            })
          );
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
        telemetryTrajectoryAllowProjectOptOut: boolean;
        handleSave: () => Promise<void>;
      };
      document.body.appendChild(el);

      await vi.waitFor(() => {
        expect(
          el.shadowRoot?.querySelector('[data-testid="telemetry-trajectory-allow-opt-out-switch"]')
        ).not.toBeNull();
      });

      const sw = el.shadowRoot!.querySelector(
        '[data-testid="telemetry-trajectory-allow-opt-out-switch"]'
      ) as HTMLElement & { checked: boolean };
      expect(sw.hasAttribute('checked') || sw.checked).toBe(true);

      el.telemetryTrajectoryAllowProjectOptOut = false;
      await el.updateComplete;
      await el.handleSave();

      expect(putBodies.length).toBe(1);
      const tel = putBodies[0].telemetry as Record<string, unknown>;
      expect(tel.trajectory).toEqual({ allow_project_opt_out: false });
    });
  });
});
