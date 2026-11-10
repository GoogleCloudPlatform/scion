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
 * The router's link handling under a non-root base path
 * (ptone/scion#4196): a plain click navigates client-side to the prefixed
 * URL, and a Ctrl, Cmd, Shift or middle click opens the prefixed URL in a
 * new tab instead of the raw app-relative href, with no doubled prefix.
 * Served at the root, a modifier click is left to the browser.
 */

import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';

vi.mock('../components/pages/projects.js', () => {
  if (!customElements.get('scion-page-projects')) {
    customElements.define('scion-page-projects', class extends HTMLElement {});
  }
  return {};
});
vi.mock('../components/pages/agents.js', () => {
  if (!customElements.get('scion-page-agents')) {
    customElements.define('scion-page-agents', class extends HTMLElement {});
  }
  return {};
});

/** Whether the router prevented the browser's default for the last event. */
let prevented: boolean | null = null;
const record = (e: Event): void => {
  // Window listeners run after the router's document listener. Prevent the
  // default here so happy-dom never follows the link itself.
  prevented = e.defaultPrevented;
  e.preventDefault();
};

function link(href: string): HTMLAnchorElement {
  const a = document.createElement('a');
  a.setAttribute('href', href);
  a.textContent = href;
  document.body.appendChild(a);
  return a;
}

function press(a: HTMLAnchorElement, type: 'click' | 'auxclick', init: MouseEventInit): void {
  prevented = null;
  a.dispatchEvent(
    new MouseEvent(type, { bubbles: true, cancelable: true, composed: true, ...init })
  );
}

describe('main.ts link handling under a base path', () => {
  let openSpy: ReturnType<typeof vi.fn>;

  beforeAll(async () => {
    window.addEventListener('click', record);
    window.addEventListener('auxclick', record);
    vi.stubEnv('BASE_URL', '/scion/');
    await bootMainAt('/scion/projects');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-projects')).not.toBeNull(),
      BOOT_WAIT
    );
  }, 15_000);

  afterEach(() => {
    document.querySelectorAll('body > a').forEach((a) => a.remove());
    vi.stubEnv('BASE_URL', '/scion/');
  });

  afterAll(() => {
    window.removeEventListener('click', record);
    window.removeEventListener('auxclick', record);
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
  });

  function stubOpen(): void {
    openSpy = vi.fn(() => null);
    vi.stubGlobal('open', openSpy);
  }

  it.each([
    ['Ctrl', 'click', { button: 0, ctrlKey: true }],
    ['Cmd', 'click', { button: 0, metaKey: true }],
    ['Shift', 'click', { button: 0, shiftKey: true }],
    ['middle', 'auxclick', { button: 1 }],
  ] as const)('a %s click opens the prefixed URL in a new tab', (_name, type, init) => {
    stubOpen();
    press(link('/agents?phase=running'), type, init);
    expect(prevented).toBe(true);
    expect(openSpy).toHaveBeenCalledTimes(1);
    expect(openSpy).toHaveBeenCalledWith('/scion/agents?phase=running', '_blank', 'noopener');
    // The page on screen stays.
    expect(window.location.pathname).toBe('/scion/projects');
  });

  it('a middle click is not handled twice when the browser also fires click', () => {
    stubOpen();
    const a = link('/agents');
    press(a, 'click', { button: 1 });
    press(a, 'auxclick', { button: 1 });
    expect(openSpy).toHaveBeenCalledTimes(1);
  });

  it('leaves Alt clicks, server routes, external and relative links to the browser', () => {
    stubOpen();
    press(link('/agents'), 'click', { button: 0, altKey: true });
    expect(prevented).toBe(false);
    for (const href of ['/auth/logout', '/api/v1/x', 'https://example.com/a', 'agents']) {
      press(link(href), 'click', { button: 0, ctrlKey: true });
      expect(prevented, href).toBe(false);
    }
    expect(openSpy).not.toHaveBeenCalled();
  });

  it('served at the root, a modifier click is left to the browser', () => {
    stubOpen();
    vi.stubEnv('BASE_URL', '/');
    press(link('/agents'), 'click', { button: 0, ctrlKey: true });
    expect(prevented).toBe(false);
    expect(openSpy).not.toHaveBeenCalled();
  });

  it('a plain click navigates client-side to the prefixed URL, prefixed once', async () => {
    stubOpen();
    press(link('/agents'), 'click', { button: 0 });
    expect(prevented).toBe(true);
    expect(openSpy).not.toHaveBeenCalled();
    expect(window.location.pathname).toBe('/scion/agents');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-agents')).not.toBeNull(),
      BOOT_WAIT
    );
  });
});
