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
 * The diagnostics page navigates to the health dashboard through the shared
 * navigation helper (ptone/scion#3118).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

const apiFetch = vi.fn((path: string) =>
  Promise.resolve(
    path === '/healthz'
      ? new Response(JSON.stringify({ status: 'ok' }), { status: 200 })
      : new Response('{}', { status: 501 })
  )
);
vi.mock('../../client/api.js', () => ({ apiFetch }));
// The log viewer is not rendered without Cloud Logging; keep it out of the test.
vi.mock('../shared/unified-log-viewer.js', () => ({}));

type DiagnosticsEl = HTMLElement & { updateComplete: Promise<boolean> };

describe('scion-page-diagnostics', () => {
  beforeAll(async () => {
    await import('./diagnostics.js');
  }, 60_000);

  afterEach(() => {
    document.body.replaceChildren();
  });

  it('"View Health" dispatches nav-click to /health on document', async () => {
    const el = document.createElement('scion-page-diagnostics') as DiagnosticsEl;
    document.body.appendChild(el);
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalledWith('/healthz'));
    await el.updateComplete;

    const paths: string[] = [];
    const onNav = (e: Event): void => {
      paths.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    try {
      const link = el.shadowRoot?.querySelector<HTMLElement>('.health-link');
      expect(link).not.toBeNull();
      link!.click();
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
    expect(paths).toEqual(['/health']);
  });
});
