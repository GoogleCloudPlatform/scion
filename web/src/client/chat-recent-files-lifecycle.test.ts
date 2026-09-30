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

import { describe, it, expect } from 'vitest';
import {
  buildRecentFilesScope,
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
