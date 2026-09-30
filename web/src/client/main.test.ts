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
 * Covers `applyServerFeatureFlags()` (ptone/scion#2217 §3.4): the boot-time
 * parallel fetch of `/api/v1/settings/public` and `/api/v1/experiments`, and
 * its failure handling.
 *
 * `main.ts` calls `init()` itself when the module loads and
 * `document.readyState !== 'loading'` (see the bottom of that file), which
 * would otherwise run a full app boot as a side effect of importing it here.
 * Setting `readyState` to `'loading'` before the import makes `init()` wait
 * for a `DOMContentLoaded` event that this file never dispatches, so only
 * the re-exported `applyServerFeatureFlagsForTests` is exercised directly.
 */

Object.defineProperty(document, 'readyState', { value: 'loading', configurable: true });

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { resetServerFlagStateForTests, isFeatureEnabled } from '../utils/feature-flags.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

let applyServerFeatureFlagsForTests: () => Promise<void>;

beforeEach(async () => {
  resetServerFlagStateForTests();
  delete window.__SCION_FEATURES__;
  try {
    localStorage.clear();
  } catch {
    // ignore
  }
  const mod = await import('./main.js');
  applyServerFeatureFlagsForTests = mod.applyServerFeatureFlagsForTests;
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('applyServerFeatureFlags: parallel boot fetch', () => {
  it('issues both requests before either resolves — neither waits for the other', async () => {
    const calls: string[] = [];
    let resolveSettings!: (r: Response) => void;
    let resolveExperiments!: (r: Response) => void;
    const settingsPromise = new Promise<Response>((r) => {
      resolveSettings = r;
    });
    const experimentsPromise = new Promise<Response>((r) => {
      resolveExperiments = r;
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        calls.push(url);
        if (url.includes('/api/v1/experiments')) return experimentsPromise;
        if (url.includes('/api/v1/settings/public')) return settingsPromise;
        throw new Error(`unexpected fetch: ${url}`);
      })
    );

    const done = applyServerFeatureFlagsForTests();

    // Both fetch() calls must have been issued immediately — before either
    // response arrives — proving the requests race in parallel rather than
    // one being awaited before the other starts.
    await Promise.resolve();
    await Promise.resolve();
    expect(calls).toContain('/api/v1/settings/public');
    expect(calls).toContain('/api/v1/experiments');

    // Resolve experiments a full tick before settings/public. If the
    // implementation awaited settings/public before even starting the
    // experiments fetch, `calls` above would already have failed; this also
    // proves the overall call resolves once both settle, in either order.
    resolveExperiments(jsonResponse({ experiments: { 'web.terminal_workspace': false } }));
    await new Promise((r) => setTimeout(r, 0));
    resolveSettings(jsonResponse({ nativeChatEnabled: true }));
    await done;

    expect(isFeatureEnabled('web.terminal_workspace')).toBe(false);
    // nativeChatEnabled: true means no chat flags are turned off.
    expect(isFeatureEnabled('web.native_chat')).toBe(true);
  });

  it('a failed experiments fetch falls back to compiled defaults while the native-chat mapping from settings/public still applies', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url.includes('/api/v1/experiments')) return Promise.reject(new Error('network error'));
        if (url.includes('/api/v1/settings/public'))
          return Promise.resolve(jsonResponse({ nativeChatEnabled: false }));
        throw new Error(`unexpected fetch: ${url}`);
      })
    );

    await applyServerFeatureFlagsForTests();

    expect(isFeatureEnabled('web.native_chat')).toBe(false);
    // Compiled default for terminal_workspace (ON) still applies.
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(true);
  });

  it.each([401, 404])('a %i experiments response falls back to compiled defaults', async (status) => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url.includes('/api/v1/experiments')) return Promise.resolve(jsonResponse({}, status));
        return Promise.resolve(jsonResponse({ nativeChatEnabled: true }));
      })
    );

    await applyServerFeatureFlagsForTests();

    expect(isFeatureEnabled('web.terminal_workspace')).toBe(true);
  });

  it('a 200 response with a non-JSON body is treated as a failure', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url.includes('/api/v1/experiments')) {
          return Promise.resolve(
            new Response('not json', { status: 200, headers: { 'Content-Type': 'text/plain' } })
          );
        }
        return Promise.resolve(jsonResponse({ nativeChatEnabled: true }));
      })
    );

    await applyServerFeatureFlagsForTests();

    expect(isFeatureEnabled('web.terminal_workspace')).toBe(true);
  });

  it('a failed settings/public fetch does not prevent the experiments map from applying', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url.includes('/api/v1/settings/public')) return Promise.reject(new Error('down'));
        if (url.includes('/api/v1/experiments'))
          return Promise.resolve(jsonResponse({ experiments: { 'web.terminal_workspace': false } }));
        throw new Error(`unexpected fetch: ${url}`);
      })
    );

    await applyServerFeatureFlagsForTests();

    expect(isFeatureEnabled('web.terminal_workspace')).toBe(false);
    // Native chat flags are untouched by the failed settings/public fetch.
    expect(isFeatureEnabled('web.native_chat')).toBe(true);
  });
});
