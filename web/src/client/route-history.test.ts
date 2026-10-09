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

import { describe, it, expect, afterEach } from 'vitest';
import {
  IN_PAGE_STATE_KEY,
  hasInPageState,
  isInPagePop,
  pushRouteEntry,
  type RouteShell,
} from './route-history.js';

describe('pushRouteEntry', () => {
  afterEach(() => {
    window.history.replaceState({}, '', '/');
  });

  it("pushes the browser URL and records the app path as the shell's rendered path", async () => {
    let resolveUpdate!: () => void;
    const shell = {
      currentPath: '/chat/alpha/topic-1',
      updateComplete: new Promise<void>((resolve) => (resolveUpdate = resolve)),
    } as unknown as RouteShell;
    const before = window.history.length;

    let settled = false;
    const done = pushRouteEntry(shell, '/chat/alpha/topic-2', '/base/chat/alpha/topic-2').then(
      () => (settled = true)
    );

    expect(window.location.pathname).toBe('/base/chat/alpha/topic-2');
    expect(window.history.length).toBe(before + 1);
    // The shell records the app-relative path, which the header's mode
    // switch returns to and the terminal-return page reuse compares.
    expect(shell.currentPath).toBe('/chat/alpha/topic-2');
    await Promise.resolve();
    expect(settled).toBe(false);
    resolveUpdate();
    await done;
    expect(settled).toBe(true);
  });

  it('still pushes the URL when there is no shell yet', async () => {
    await pushRouteEntry(undefined, '/chat', '/chat');
    expect(window.location.pathname).toBe('/chat');
  });

  it('records the given history state on the entry', async () => {
    await pushRouteEntry(undefined, '/chat/a/t', '/chat/a/t', { [IN_PAGE_STATE_KEY]: { x: 1 } });
    expect(window.history.state).toEqual({ [IN_PAGE_STATE_KEY]: { x: 1 } });
  });
});

describe('isInPagePop', () => {
  const marked = { [IN_PAGE_STATE_KEY]: { panel: 'left', below: [] } };
  const shell = (currentPath: string): RouteShell => ({ currentPath }) as unknown as RouteShell;

  it('leaves an in-page entry of the shown path to the page', () => {
    expect(isInPagePop(marked, '/chat/a/t', shell('/chat/a/t'), false)).toBe(true);
    // The fragment is not part of which page is showing.
    expect(isInPagePop(marked, '/chat/a/t', shell('/chat/a/t#msg-1'), false)).toBe(true);
    expect(isInPagePop(marked, '/chat/a/t?x=1', shell('/chat/a/t?x=1'), false)).toBe(true);
  });

  it('renders any other path, or a state without the marker', () => {
    expect(isInPagePop(marked, '/chat/a/other', shell('/chat/a/t'), false)).toBe(false);
    expect(isInPagePop(marked, '/chat/a/t?x=1', shell('/chat/a/t'), false)).toBe(false);
    expect(isInPagePop({}, '/chat/a/t', shell('/chat/a/t'), false)).toBe(false);
    expect(isInPagePop(null, '/chat/a/t', shell('/chat/a/t'), false)).toBe(false);
    expect(isInPagePop(marked, '/chat/a/t', undefined, false)).toBe(false);
  });

  it('renders on the way back from the terminal workspace, which hides the outlet', () => {
    expect(isInPagePop(marked, '/chat/a/t', shell('/chat/a/t'), true)).toBe(false);
  });

  it('recognises the marker only as an object', () => {
    expect(hasInPageState(marked)).toBe(true);
    expect(hasInPageState({ [IN_PAGE_STATE_KEY]: null })).toBe(false);
    expect(hasInPageState({ [IN_PAGE_STATE_KEY]: 'x' })).toBe(false);
  });
});
