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
 * feature-flags — unit tests.
 *
 * Validates the feature-flag module after the access boundary
 * hard cutover: access boundary flags are removed; native_chat
 * flags are retained and default-on.
 */

import { describe, it, expect, beforeEach } from 'vitest';
import {
  isFeatureEnabled,
  setFeatureFlag,
  NATIVE_CHAT_V2_FLAG,
} from './feature-flags.js';

// Verify removed exports at the type level — these should not exist.
// @ts-expect-error ACCESS_BOUNDARIES_READ_FLAG was removed
import { ACCESS_BOUNDARIES_READ_FLAG } from './feature-flags.js';
// @ts-expect-error ACCESS_BOUNDARIES_AUTHORING_FLAG was removed
import { ACCESS_BOUNDARIES_AUTHORING_FLAG } from './feature-flags.js';

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

beforeEach(() => {
  // Clear server-injected features
  delete window.__SCION_FEATURES__;
  // Clear any localStorage overrides
  try {
    localStorage.removeItem('scion:feature:web.native_chat');
    localStorage.removeItem('scion:feature:web.native_chat_v2');
    localStorage.removeItem('scion:feature:web.access_boundaries_read');
    localStorage.removeItem('scion:feature:web.access_boundaries_authoring');
    localStorage.removeItem('scion:feature:test.flag');
  } catch {
    // ignore in environments without localStorage
  }
});

// ---------------------------------------------------------------------------
// Removed access boundary flags
// ---------------------------------------------------------------------------

describe('feature-flags: access boundary flags removed', () => {
  it('does not export ACCESS_BOUNDARIES_READ_FLAG', () => {
    expect(ACCESS_BOUNDARIES_READ_FLAG).toBeUndefined();
  });

  it('does not export ACCESS_BOUNDARIES_AUTHORING_FLAG', () => {
    expect(ACCESS_BOUNDARIES_AUTHORING_FLAG).toBeUndefined();
  });

  it('access_boundaries_read defaults to OFF (not in DEFAULT_ON_FLAGS)', () => {
    expect(isFeatureEnabled('web.access_boundaries_read')).toBe(false);
  });

  it('access_boundaries_authoring defaults to OFF', () => {
    expect(isFeatureEnabled('web.access_boundaries_authoring')).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Retained native_chat flags
// ---------------------------------------------------------------------------

describe('feature-flags: native_chat flags retained', () => {
  it('exports NATIVE_CHAT_V2_FLAG', () => {
    expect(NATIVE_CHAT_V2_FLAG).toBe('web.native_chat_v2');
  });

  it('web.native_chat defaults to ON', () => {
    expect(isFeatureEnabled('web.native_chat')).toBe(true);
  });

  it('web.native_chat_v2 defaults to ON', () => {
    expect(isFeatureEnabled('web.native_chat_v2')).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// isFeatureEnabled: resolution order
// ---------------------------------------------------------------------------

describe('isFeatureEnabled: resolution order', () => {
  it('returns false for unknown flags', () => {
    expect(isFeatureEnabled('test.flag')).toBe(false);
  });

  it('server-injected true overrides default-off', () => {
    window.__SCION_FEATURES__ = { 'test.flag': true };
    expect(isFeatureEnabled('test.flag')).toBe(true);
  });

  it('server-injected false overrides default-on', () => {
    window.__SCION_FEATURES__ = { 'web.native_chat': false };
    expect(isFeatureEnabled('web.native_chat')).toBe(false);
  });

  it('localStorage true overrides default-off', () => {
    localStorage.setItem('scion:feature:test.flag', 'true');
    expect(isFeatureEnabled('test.flag')).toBe(true);
  });

  it('localStorage false overrides default-on', () => {
    localStorage.setItem('scion:feature:web.native_chat', 'false');
    expect(isFeatureEnabled('web.native_chat')).toBe(false);
  });

  it('server-injected takes precedence over localStorage', () => {
    window.__SCION_FEATURES__ = { 'test.flag': false };
    localStorage.setItem('scion:feature:test.flag', 'true');
    expect(isFeatureEnabled('test.flag')).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// setFeatureFlag
// ---------------------------------------------------------------------------

describe('setFeatureFlag', () => {
  it('writes into window.__SCION_FEATURES__', () => {
    setFeatureFlag('test.flag', true);
    expect(window.__SCION_FEATURES__?.['test.flag']).toBe(true);
  });

  it('value set via setFeatureFlag is returned by isFeatureEnabled', () => {
    setFeatureFlag('test.flag', true);
    expect(isFeatureEnabled('test.flag')).toBe(true);
  });

  it('can disable a default-on flag', () => {
    setFeatureFlag('web.native_chat', false);
    expect(isFeatureEnabled('web.native_chat')).toBe(false);
  });
});
