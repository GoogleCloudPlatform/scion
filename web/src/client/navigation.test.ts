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

import { describe, it, expect, afterEach, vi } from 'vitest';
import { browserPath, navigateTo, pushUrl, replaceSearch, stripBasePath } from './navigation.js';

afterEach(() => {
  vi.unstubAllEnvs();
  window.history.replaceState({}, '', '/');
});

describe('browserPath', () => {
  it('returns the path unchanged when served at the root', () => {
    vi.stubEnv('BASE_URL', '/');
    expect(browserPath('/projects/p1')).toBe('/projects/p1');
  });

  it('prefixes the base path, without doubling the slash', () => {
    vi.stubEnv('BASE_URL', '/scion/');
    expect(browserPath('/projects/p1')).toBe('/scion/projects/p1');
  });
});

describe('stripBasePath', () => {
  it('is a no-op when served at the root', () => {
    vi.stubEnv('BASE_URL', '/');
    expect(stripBasePath('/projects/p1')).toBe('/projects/p1');
  });

  it('strips the base path, mapping the bare base to /', () => {
    vi.stubEnv('BASE_URL', '/scion/');
    expect(stripBasePath('/scion/projects/p1')).toBe('/projects/p1');
    expect(stripBasePath('/scion')).toBe('/');
    expect(stripBasePath('/scion/')).toBe('/');
    expect(stripBasePath('/other/x')).toBe('/other/x');
  });

  it('round-trips with browserPath', () => {
    vi.stubEnv('BASE_URL', '/scion/');
    expect(stripBasePath(browserPath('/skills/s1'))).toBe('/skills/s1');
  });
});

describe('navigateTo', () => {
  it('dispatches a composed nav-click event on document with the app path', () => {
    const seen: CustomEvent<{ path: string }>[] = [];
    const onNav = (e: Event) => seen.push(e as CustomEvent<{ path: string }>);
    document.addEventListener('nav-click', onNav);
    try {
      navigateTo('/skills/s1?tab=versions');
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
    expect(seen).toHaveLength(1);
    expect(seen[0].detail.path).toBe('/skills/s1?tab=versions');
    expect(seen[0].bubbles).toBe(true);
    expect(seen[0].composed).toBe(true);
  });

  it('does not touch history itself (the router does)', () => {
    const push = vi.spyOn(window.history, 'pushState');
    navigateTo('/projects');
    expect(push).not.toHaveBeenCalled();
    push.mockRestore();
  });
});

describe('pushUrl', () => {
  it('pushes the base-prefixed URL', () => {
    vi.stubEnv('BASE_URL', '/scion/');
    const before = window.history.length;
    pushUrl('/admin/access-boundaries/new');
    expect(window.location.pathname).toBe('/scion/admin/access-boundaries/new');
    expect(window.history.length).toBe(before + 1);
  });
});

describe('replaceSearch', () => {
  it('replaces the query, keeping path and hash, without a new entry', () => {
    window.history.replaceState({ keep: 1 }, '', '/scion/admin/groups?q=old#top');
    const before = window.history.length;
    replaceSearch(new URLSearchParams({ q: 'new', tab: 'mine' }));
    expect(window.location.pathname).toBe('/scion/admin/groups');
    expect(window.location.search).toBe('?q=new&tab=mine');
    expect(window.location.hash).toBe('#top');
    expect(window.history.length).toBe(before);
    expect(window.history.state).toEqual({ keep: 1 });
  });

  it('accepts a string with or without a leading ? and clears on empty', () => {
    window.history.replaceState({}, '', '/agents/graph?project=a');
    replaceSearch('?project=b');
    expect(window.location.search).toBe('?project=b');
    replaceSearch('');
    expect(window.location.search).toBe('');
    expect(window.location.pathname).toBe('/agents/graph');
  });
});
