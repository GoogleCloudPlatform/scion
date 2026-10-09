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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageHarnessConfigDetail } from './harness-config-detail.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function createElement(sourceUrl: string): Promise<ScionPageHarnessConfigDetail> {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string) => {
      if (url.endsWith('/api/v1/harness-configs/hc-1')) {
        return Promise.resolve(
          jsonResponse({
            id: 'hc-1',
            name: 'my-config',
            slug: 'my-config',
            harness: 'claude',
            status: 'active',
            scope: 'global',
            sourceUrl,
          })
        );
      }
      return Promise.resolve(jsonResponse({}));
    })
  );
  window.history.pushState({}, '', '/settings/harness-configs/hc-1');
  const el = document.createElement(
    'scion-page-harness-config-detail'
  ) as ScionPageHarnessConfigDetail;
  document.body.appendChild(el);
  for (let i = 0; i < 4; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
  return el;
}

describe('harness config detail: source display', () => {
  let element: ScionPageHarnessConfigDetail | null = null;

  beforeAll(async () => {
    await import('./harness-config-detail.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('links an https source without credentials', async () => {
    element = await createElement('git+https://user:secret@github.com/acme/repo/tree/main/hc');
    const link = element.shadowRoot?.querySelector('.source-url a') as HTMLAnchorElement | null;
    expect(link?.getAttribute('href')).toBe('https://github.com/acme/repo/tree/main/hc');
    expect(element.shadowRoot?.textContent ?? '').not.toContain('secret');
  });

  // The display never shows credentials embedded in a source string.
  it('never shows credentials embedded in a non-https source', async () => {
    element = await createElement(':s3,access_key_id=AKIA,secret_access_key=secret:bucket/hc');
    const text = element.shadowRoot?.querySelector('.source-url')?.textContent ?? '';
    expect(text).toContain('non-web source');
    expect(element.shadowRoot?.textContent ?? '').not.toContain('secret');
    expect(element.shadowRoot?.querySelector('.source-url a')).toBeNull();
  });
});
