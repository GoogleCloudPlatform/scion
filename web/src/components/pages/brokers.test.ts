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

/**
 * Tests for brokers.ts's "Agents / Cap" rendering (ptone/scion#2061 P2.2,
 * design.md Amendment A1).
 *
 * The backend does not produce agentLimitSource: "not_enforced" yet (P1b,
 * ptone/scion#2270, is a separate in-progress PR) — these tests stub the
 * runtime-brokers list response with that source to pin the rendering
 * contract ahead of the backend wiring: a broker whose cap is not currently
 * enforced must show a visible "not enforced" marker next to the value, in
 * both the grid view (the page's default) and the table view — not only in
 * the tooltip, which a caller could miss entirely.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

/** happy-dom has no EventSource; the page's stateManager.setScope opens one. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const NOT_ENFORCED_BROKER = {
  id: 'broker-not-enforced',
  name: 'not-enforced-broker',
  slug: 'not-enforced-broker',
  version: '1.0.0',
  status: 'online',
  connectionState: 'connected',
  lastHeartbeat: new Date().toISOString(),
  autoProvide: false,
  createdAt: new Date().toISOString(),
  updatedAt: new Date().toISOString(),
  // Stubbed: the backend doesn't produce this source yet (P1b in progress).
  agentLimit: 30,
  agentCount: 7,
  agentLimitSource: 'not_enforced',
};

const ENFORCED_BROKER = {
  ...NOT_ENFORCED_BROKER,
  id: 'broker-enforced',
  name: 'enforced-broker',
  slug: 'enforced-broker',
  agentLimitSource: 'hub_default',
};

async function mountBrokersPage(): Promise<
  HTMLElement & { updateComplete: Promise<boolean> }
> {
  const el = document.createElement('scion-page-brokers') as HTMLElement & {
    updateComplete: Promise<boolean>;
  };
  document.body.appendChild(el);
  await el.updateComplete;
  // Let the async loadBrokers() fetch resolve and trigger a re-render.
  await new Promise((resolve) => setTimeout(resolve, 20));
  await el.updateComplete;
  return el;
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

describe('scion-page-brokers — not_enforced marker (design.md Amendment A1)', () => {
  let element: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;

  beforeAll(async () => {
    await import('./brokers.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    localStorage.removeItem('scion-view-brokers');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('grid view (default): shows a visible "not enforced" marker, not just in a tooltip', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ brokers: [NOT_ENFORCED_BROKER] })))
    );
    element = await mountBrokersPage();

    const text = shadowText(element);
    expect(text).toContain('7 / 30');
    expect(text.toLowerCase()).toContain('not enforced');

    // Not tooltip-only: the marker must be present as visible text content,
    // which textContent already proves (title attributes are not part of
    // textContent) — but assert it explicitly on the marker element too.
    const marker = element.shadowRoot?.querySelector('.not-enforced-marker');
    expect(marker).not.toBeNull();
    expect(marker?.textContent?.toLowerCase()).toContain('not enforced');
  });

  it('table view: shows the same visible "not enforced" marker', async () => {
    localStorage.setItem('scion-view-brokers', 'list');
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ brokers: [NOT_ENFORCED_BROKER] })))
    );
    element = await mountBrokersPage();

    const text = shadowText(element);
    expect(text).toContain('7 / 30');
    expect(text.toLowerCase()).toContain('not enforced');
    expect(element.shadowRoot?.querySelector('.not-enforced-marker')).not.toBeNull();
  });

  it('does not show the marker for an enforced source', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ brokers: [ENFORCED_BROKER] })))
    );
    element = await mountBrokersPage();

    const text = shadowText(element);
    expect(text).toContain('7 / 30');
    expect(text.toLowerCase()).not.toContain('not enforced');
    expect(element.shadowRoot?.querySelector('.not-enforced-marker')).toBeNull();
  });
});
