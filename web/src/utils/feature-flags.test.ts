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

import { describe, it, expect, beforeEach, vi } from 'vitest';
import {
  isFeatureEnabled,
  setFeatureFlag,
  setServerFlags,
  resetServerFlagStateForTests,
  TERMINAL_WORKSPACE_FLAG,
} from './feature-flags.js';

// Verify removed exports at the type level — these should not exist.
// @ts-expect-error ACCESS_BOUNDARIES_READ_FLAG was removed
import { ACCESS_BOUNDARIES_READ_FLAG } from './feature-flags.js';
// @ts-expect-error ACCESS_BOUNDARIES_AUTHORING_FLAG was removed
import { ACCESS_BOUNDARIES_AUTHORING_FLAG } from './feature-flags.js';
// @ts-expect-error NATIVE_CHAT_PALETTE_FLAG was removed (the palette is always on)
import { NATIVE_CHAT_PALETTE_FLAG } from './feature-flags.js';
// @ts-expect-error NATIVE_CHAT_V2_FLAG was removed (v2 is always on)
import { NATIVE_CHAT_V2_FLAG } from './feature-flags.js';

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

beforeEach(() => {
  // Clear server-injected features
  delete window.__SCION_FEATURES__;
  // Clear any localStorage overrides
  try {
    localStorage.removeItem('scion:feature:web.native_chat');
    localStorage.removeItem('scion:feature:web.access_boundaries_read');
    localStorage.removeItem('scion:feature:web.access_boundaries_authoring');
    localStorage.removeItem('scion:feature:web.terminal_workspace');
    localStorage.removeItem('scion:feature:test.flag');
  } catch {
    // ignore in environments without localStorage
  }
  resetServerFlagStateForTests();
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
// Removed palette flag
// ---------------------------------------------------------------------------

describe('feature-flags: palette flag removed', () => {
  it('does not export NATIVE_CHAT_PALETTE_FLAG', () => {
    expect(NATIVE_CHAT_PALETTE_FLAG).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Removed v2 flag
// ---------------------------------------------------------------------------

describe('feature-flags: v2 flag removed', () => {
  it('does not export NATIVE_CHAT_V2_FLAG', () => {
    expect(NATIVE_CHAT_V2_FLAG).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Retained native_chat flags
// ---------------------------------------------------------------------------

describe('feature-flags: native_chat flags retained', () => {
  it('web.native_chat defaults to ON', () => {
    expect(isFeatureEnabled('web.native_chat')).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// terminal_workspace default-on
// ---------------------------------------------------------------------------

describe('feature-flags: terminal_workspace default-on', () => {
  it('web.terminal_workspace defaults to ON (absent override → enabled)', () => {
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(true);
  });

  it('server-injected false overrides default-on for terminal_workspace', () => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': false };
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(false);
  });

  it('localStorage false overrides default-on for terminal_workspace', () => {
    localStorage.setItem('scion:feature:web.terminal_workspace', 'false');
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(false);
  });

  it('other default-on flags are unaffected', () => {
    expect(isFeatureEnabled('web.native_chat')).toBe(true);
  });

  it('unrelated flags not in DEFAULT_ON_FLAGS still default to false', () => {
    expect(isFeatureEnabled('test.flag')).toBe(false);
    expect(isFeatureEnabled('web.access_boundaries_read')).toBe(false);
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

// ---------------------------------------------------------------------------
// TERMINAL_WORKSPACE_FLAG (ptone/scion#2217)
// ---------------------------------------------------------------------------

describe('TERMINAL_WORKSPACE_FLAG', () => {
  it('exports the expected name', () => {
    expect(TERMINAL_WORKSPACE_FLAG).toBe('web.terminal_workspace');
  });
});

// ---------------------------------------------------------------------------
// setServerFlags / resetServerFlagStateForTests — precedence matrix
// (ptone/scion#2217)
// ---------------------------------------------------------------------------

describe('setServerFlags: precedence', () => {
  it('a server value beats localStorage for a registered experiment', () => {
    localStorage.setItem('scion:feature:web.terminal_workspace', 'true');
    setServerFlags({ 'web.terminal_workspace': false });
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(false);
  });

  it('localStorage applies to names the server did not send', () => {
    localStorage.setItem('scion:feature:test.flag', 'true');
    setServerFlags({ 'web.terminal_workspace': false });
    expect(isFeatureEnabled('test.flag')).toBe(true);
  });

  it('values in the bag before the first setServerFlags() call beat server values ("pinned")', () => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': true };
    setServerFlags({ 'web.terminal_workspace': false });
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(true);
  });

  it('pinning only protects keys present at the first call, not later ones', () => {
    window.__SCION_FEATURES__ = { 'a.flag': true };
    setServerFlags({ 'a.flag': false });
    expect(isFeatureEnabled('a.flag')).toBe(true);
    // A second setServerFlags call still cannot override the pinned key...
    setServerFlags({ 'a.flag': false, 'b.flag': true });
    expect(isFeatureEnabled('a.flag')).toBe(true);
    // ...but a name that wasn't pinned is applied normally.
    expect(isFeatureEnabled('b.flag')).toBe(true);
  });

  it('resetServerFlagStateForTests() clears the pin so the next call re-pins from the current bag', () => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': true };
    setServerFlags({ 'web.terminal_workspace': false });
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(true);

    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    setServerFlags({ 'web.terminal_workspace': false });
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(false);
  });

  it('a server value applies to an unregistered/dev name the same way', () => {
    setServerFlags({ 'hub.future_thing': true });
    expect(isFeatureEnabled('hub.future_thing')).toBe(true);
  });

  it('is a no-op (does not throw) when window is undefined, e.g. a non-browser context', () => {
    vi.stubGlobal('window', undefined);
    try {
      expect(() => setServerFlags({ 'test.flag': true })).not.toThrow();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('ignores a null or non-object flags argument', () => {
    // @ts-expect-error exercising a malformed runtime value
    expect(() => setServerFlags(null)).not.toThrow();
    // @ts-expect-error exercising a malformed runtime value
    expect(() => setServerFlags('not an object')).not.toThrow();
    // Without the guard, Object.entries() on a string iterates its
    // characters as numeric-index entries, polluting the bag — check the
    // bag directly, since `isFeatureEnabled` would hide that either way.
    expect(window.__SCION_FEATURES__ ?? {}).not.toHaveProperty('0');
  });

  it('ignores an array flags argument instead of writing its indices into the bag', () => {
    // typeof [] === 'object', so this needs its own Array.isArray check.
    // @ts-expect-error exercising a malformed runtime value
    setServerFlags(['web.terminal_workspace']);
    expect(window.__SCION_FEATURES__ ?? {}).not.toHaveProperty('0');
  });
});

// ---------------------------------------------------------------------------
// Shadowed-override logging — one console.info per flag per page load
// ---------------------------------------------------------------------------

describe('isFeatureEnabled: shadowed-override logging', () => {
  it('logs once when a localStorage override is shadowed by a server value, not once per call', () => {
    const infoSpy = vi.spyOn(console, 'info').mockImplementation(() => {});
    localStorage.setItem('scion:feature:web.terminal_workspace', 'false');
    setServerFlags({ 'web.terminal_workspace': true });

    isFeatureEnabled('web.terminal_workspace');
    isFeatureEnabled('web.terminal_workspace');
    isFeatureEnabled('web.terminal_workspace');

    expect(infoSpy).toHaveBeenCalledTimes(1);
    infoSpy.mockRestore();
  });

  it('does not log when there is no localStorage override to shadow', () => {
    const infoSpy = vi.spyOn(console, 'info').mockImplementation(() => {});
    setServerFlags({ 'web.terminal_workspace': true });

    isFeatureEnabled('web.terminal_workspace');

    expect(infoSpy).not.toHaveBeenCalled();
    infoSpy.mockRestore();
  });

  it('logs independently per flag name', () => {
    const infoSpy = vi.spyOn(console, 'info').mockImplementation(() => {});
    localStorage.setItem('scion:feature:web.terminal_workspace', 'false');
    localStorage.setItem('scion:feature:web.native_chat', 'false');
    setServerFlags({ 'web.terminal_workspace': true, 'web.native_chat': true });

    isFeatureEnabled('web.terminal_workspace');
    isFeatureEnabled('web.native_chat');

    expect(infoSpy).toHaveBeenCalledTimes(2);
    infoSpy.mockRestore();
  });
});
