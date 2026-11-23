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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetServerFlagStateForTests, setFeatureFlag } from '../../utils/feature-flags.js';
import { FILTERS_QUERY_DIALECT_FLAG } from '../../shared/filter-query/index.js';
import './agents.js';
import type { ScionPageAgents } from './agents.js';

describe('<scion-page-agents> unified filter dialect (P1)', () => {
  let fetchUrls: string[];

  beforeEach(() => {
    fetchUrls = [];
    localStorage.clear();
    window.history.replaceState(null, '', '/agents');
    resetServerFlagStateForTests();
    vi.stubGlobal(
      'EventSource',
      class {
        addEventListener(): void {}
        removeEventListener(): void {}
        close(): void {}
      }
    );
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        fetchUrls.push(url);
        return Promise.resolve(
          new Response(JSON.stringify({ agents: [], complete: true, totalCount: 0 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    localStorage.clear();
    window.history.replaceState(null, '', '/agents');
    resetServerFlagStateForTests();
  });

  it('renders legacy label input when filters.query_dialect is off', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, false);
    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('scion-filter-box')).toBeNull();
    expect(el.shadowRoot?.querySelector('sl-input[placeholder*="label"]')).not.toBeNull();
  });

  it('migrates localStorage phase once to ?q= and renders <scion-filter-box> when on', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    localStorage.setItem('scion-filter-agents-phase', 'running');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 10));

    expect(localStorage.getItem('scion-filter-agents-phase')).toBeNull();
    expect(new URLSearchParams(window.location.search).get('q')).toBe('phase:running');

    const box = el.shadowRoot?.querySelector('scion-filter-box');
    expect(box).not.toBeNull();
    expect(box?.value).toBe('phase:running');
    expect(fetchUrls.some((u) => u.includes('q=phase%3Arunning'))).toBe(true);
  });

  it('edits query text when clicking phase chips and updates ?q= in URL', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    window.history.replaceState(null, '', '/agents?q=label%3Ateam%3Dui');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    const buttons = Array.from(
      el.shadowRoot?.querySelectorAll('.filter-bar .scope-toggle button') ?? []
    );
    const errorBtn = buttons.find((b) => b.textContent?.trim() === 'Error') as HTMLButtonElement;
    expect(errorBtn).toBeDefined();
    errorBtn.click();
    await el.updateComplete;

    expect(new URLSearchParams(window.location.search).get('q')).toBe('phase:error label:team=ui');

    const allBtn = buttons.find((b) => b.textContent?.trim() === 'All') as HTMLButtonElement;
    allBtn.click();
    await el.updateComplete;
    expect(new URLSearchParams(window.location.search).get('q')).toBe('label:team=ui');
  });

  it('shows "No Matching Agents" instead of "No Agents Found" when a filter query matches zero rows', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    window.history.replaceState(null, '', '/agents?q=phase%3Aerror');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    const heading = el.shadowRoot?.querySelector('.empty-state h2')?.textContent?.trim();
    expect(heading).toBe('No Matching Agents');
  });
});
