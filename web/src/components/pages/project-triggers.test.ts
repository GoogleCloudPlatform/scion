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
import { resetServerFlagStateForTests, setServerFlags } from '../../utils/feature-flags.js';
import { clearSourceExtensionsForTests } from '../triggers/source-extensions.js';
import './project-triggers.js';
import type { ScionPageProjectTriggers } from './project-triggers.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('scion-page-project-triggers', () => {
  beforeEach(() => {
    clearSourceExtensionsForTests();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    window.history.replaceState({}, '', '/projects/proj-1/triggers');
  });

  afterEach(() => {
    clearSourceExtensionsForTests();
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('renders 404 and makes zero API requests when hub.triggers flag is off', async () => {
    setServerFlags({ 'hub.triggers': false });
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-page-project-triggers') as ScionPageProjectTriggers;
    el.projectId = 'proj-1';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('scion-page-404')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('sl-tab-group')).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders all four tabs when hub.triggers flag is on and honors deep-link params', async () => {
    setServerFlags({ 'hub.triggers': true });
    window.history.replaceState(
      {},
      '',
      '/projects/proj-1/triggers?tab=rules&new=1&source=custom-src&preset=p-1'
    );

    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url.includes('/webhook-endpoints')) {
          return Promise.resolve(jsonResponse({ endpoints: [], totalCount: 0 }));
        }
        if (url.includes('/triggers')) {
          return Promise.resolve(jsonResponse({ triggers: [], totalCount: 0 }));
        }
        if (url.includes('/trigger-events')) {
          return Promise.resolve(jsonResponse({ events: [], totalCount: 0 }));
        }
        if (url.includes('/trigger-fires')) {
          return Promise.resolve(jsonResponse({ fires: [], totalCount: 0 }));
        }
        if (url.includes('/agents')) {
          return Promise.resolve(jsonResponse({ agents: [] }));
        }
        return Promise.resolve(jsonResponse({ id: 'proj-1', name: 'Alpha Project' }));
      })
    );

    const el = document.createElement('scion-page-project-triggers') as ScionPageProjectTriggers;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 30));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('scion-page-404')).toBeNull();
    expect(el.shadowRoot?.querySelector('scion-webhook-endpoint-list')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('scion-trigger-list')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('scion-trigger-event-list')).toBeTruthy();
    expect(el.shadowRoot?.querySelector('scion-trigger-fire-list')).toBeTruthy();

    const triggerList = el.shadowRoot?.querySelector('scion-trigger-list') as HTMLElement & {
      pendingCreateSource: string;
      pendingCreatePreset: string;
    };
    expect(triggerList.pendingCreateSource).toBe('custom-src');
    expect(triggerList.pendingCreatePreset).toBe('p-1');
  });
});
