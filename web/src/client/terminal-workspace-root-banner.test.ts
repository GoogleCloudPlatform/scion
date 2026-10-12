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

// @vitest-environment happy-dom

/**
 * Test-hub banner in the terminal workspace (ptone/scion#4240, phase W):
 * the workspace replaces the app shell with its own header, so it renders
 * the banner above that header itself.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import { TEST_INFRA_STATUS_URL, resetTestInfraStatusForTests } from './test-infra-status.js';
import { TEST_HUB_MEMBER_TEXT } from '../components/shared/test-hub-banner.js';

vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    dispose = vi.fn();
    reset = vi.fn();
    write = vi.fn();
    focus = vi.fn();
    blur = vi.fn();
    refresh = vi.fn();
    parser = { registerOscHandler: vi.fn() };
    loadAddon = vi.fn();
    open = vi.fn();
    onData = vi.fn();
    onBinary = vi.fn();
    attachCustomKeyEventHandler = vi.fn();
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/xterm/css/xterm.css?inline', () => ({ default: '' }));

let WorkspaceRoot: typeof TerminalWorkspaceRoot;
let root: TerminalWorkspaceRoot | null = null;

beforeAll(async () => {
  WorkspaceRoot = (await import('./terminal-workspace-root.js')).TerminalWorkspaceRoot;
}, 30_000);

afterEach(() => {
  root?.dispose();
  root?.element.remove();
  root = null;
  resetTestInfraStatusForTests();
  vi.unstubAllGlobals();
});

/** The URL a fetch mock was called with, whatever form it took. */
function urlOf(input: unknown): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.href;
  if (input instanceof Request) return input.url;
  return '';
}

/** Stubs fetch: the status endpoint answers `body`, everything else `{}`. */
function stubStatus(body: unknown): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    return Promise.resolve(
      new Response(JSON.stringify(url.endsWith(TEST_INFRA_STATUS_URL) ? body : {}), {
        status: 200,
      })
    );
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function banner(): HTMLElement | null {
  return root?.element.querySelector<HTMLElement>('sl-alert.test-hub-banner') ?? null;
}

describe('terminal workspace test-hub banner', () => {
  it('renders the banner above the workspace header, and again after removal and navigation', async () => {
    stubStatus({ testIdentities: true, testHubAdmin: false, testSuperAdmin: false });
    root = new WorkspaceRoot(null);
    document.body.appendChild(root.element);
    await vi.waitFor(() => expect(banner()).not.toBeNull());

    const first = banner()!;
    expect(first.textContent?.trim()).toBe(TEST_HUB_MEMBER_TEXT);
    expect(first.hasAttribute('closable')).toBe(false);
    const container = root.element.querySelector(':scope > .terminal-test-hub-banner')!;
    expect(container.contains(first)).toBe(true);
    expect(container.nextElementSibling?.tagName.toLowerCase()).toBe('scion-header');

    // Removing the banner, or even its container, then navigating brings it back.
    container.remove();
    root.setCurrentPath('/terminals/agent-1');
    const second = banner();
    expect(second).not.toBeNull();
    expect(second).not.toBe(first);
    const restored = root.element.querySelector(':scope > .terminal-test-hub-banner')!;
    expect(restored.contains(second)).toBe(true);
    expect(restored.nextElementSibling?.tagName.toLowerCase()).toBe('scion-header');
  });

  it('renders no banner element when every gate is off', async () => {
    const fetchMock = stubStatus({
      testIdentities: false,
      testHubAdmin: false,
      testSuperAdmin: false,
    });
    root = new WorkspaceRoot(null);
    document.body.appendChild(root.element);
    await vi.waitFor(() =>
      expect(fetchMock.mock.calls.some(([u]) => urlOf(u).endsWith(TEST_INFRA_STATUS_URL))).toBe(
        true
      )
    );
    await new Promise((r) => setTimeout(r, 0));
    expect(root.element.querySelector('sl-alert')).toBeNull();
  });
});
