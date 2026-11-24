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

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  buildRecentFilesScope,
  clearRecentFilesForLogout,
  recentFilesStorageKey,
  registerRecentFilesStore,
  setRecentFilesScope,
  shouldClearRecentFilesOnTeardown,
} from './chat-recent-files-lifecycle.js';

describe('shouldClearRecentFilesOnTeardown', () => {
  it('clears on an explicit logout', () => {
    expect(shouldClearRecentFilesOnTeardown('logout')).toBe(true);
  });

  it('does not clear on an auth-expiry teardown — the account may resume after re-auth', () => {
    expect(shouldClearRecentFilesOnTeardown('auth-expired')).toBe(false);
  });

  it('does not clear when the reason is missing', () => {
    expect(shouldClearRecentFilesOnTeardown(undefined)).toBe(false);
  });
});

describe('buildRecentFilesScope', () => {
  it('builds a scope from the user id, origin, and base URL', () => {
    expect(buildRecentFilesScope({ id: 'user-1' }, 'https://hub.example', '/app/')).toEqual({
      origin: 'https://hub.example',
      baseUrl: '/app/',
      userId: 'user-1',
    });
  });

  it('uses the given user id, not some other field', () => {
    const scope = buildRecentFilesScope({ id: 'user-2' }, 'https://hub.example', '/');
    expect(scope.userId).toBe('user-2');
  });
});

describe('lazy store lifecycle bridge', () => {
  const scope = buildRecentFilesScope({ id: 'user-1' }, 'https://hub.example', '/');

  afterEach(() => {
    registerRecentFilesStore(null);
    setRecentFilesScope(null);
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('applies a scope recorded before the chat store registers', () => {
    setRecentFilesScope(scope);
    const store = { setScope: vi.fn(), clearForLogout: vi.fn() };
    registerRecentFilesStore(store);
    expect(store.setScope).toHaveBeenCalledTimes(1);
    expect(store.setScope).toHaveBeenCalledWith(scope);
  });

  it('forwards setRecentFilesScope and clearRecentFilesForLogout when the store is registered', () => {
    const store = { setScope: vi.fn(), clearForLogout: vi.fn() };
    registerRecentFilesStore(store);

    setRecentFilesScope(scope);
    expect(store.setScope).toHaveBeenCalledWith(scope);

    clearRecentFilesForLogout();
    expect(store.clearForLogout).toHaveBeenCalledTimes(1);

    // Pending scope was cleared, so a later registration does not replay it.
    const nextStore = { setScope: vi.fn(), clearForLogout: vi.fn() };
    registerRecentFilesStore(nextStore);
    expect(nextStore.setScope).not.toHaveBeenCalled();
  });

  it('clears persisted localStorage directly on logout when the chat store has not loaded', () => {
    const key = recentFilesStorageKey(scope);
    localStorage.setItem(key, JSON.stringify({ version: 1, records: [] }));
    setRecentFilesScope(scope);

    clearRecentFilesForLogout();
    expect(localStorage.getItem(key)).toBeNull();

    // A store registering after logout must not receive the cleared scope.
    const store = { setScope: vi.fn(), clearForLogout: vi.fn() };
    registerRecentFilesStore(store);
    expect(store.setScope).not.toHaveBeenCalled();
  });
});
