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
import * as featureFlags from './feature-flags.js';
import {
  DECISIONS_FLAG,
  decisionsEnabled,
  isFeatureEnabled,
  setFeatureFlag,
  setServerFlags,
  resetServerFlagStateForTests,
} from './feature-flags.js';

const featureFlagExports: Record<string, unknown> = featureFlags;

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
    localStorage.removeItem('scion:feature:web.fixture_flag');
    localStorage.removeItem('scion:feature:test.flag');
  } catch {
    // ignore in environments without localStorage
  }
  resetServerFlagStateForTests();
});

// ---------------------------------------------------------------------------
// Removed access boundary flags
// ---------------------------------------------------------------------------

describe('feature-flags: web.agent_edit', () => {
  it('defaults ON (in DEFAULT_ON_FLAGS) with no server value or override', () => {
    expect(isFeatureEnabled('web.agent_edit')).toBe(true);
  });

  it('the server can still turn it off', () => {
    setServerFlags({ 'web.agent_edit': false });
    expect(isFeatureEnabled('web.agent_edit')).toBe(false);
  });
});

describe('feature-flags: web.chat_starred_messages', () => {
  it('defaults ON (in DEFAULT_ON_FLAGS) with no server value or override', () => {
    expect(isFeatureEnabled('web.chat_starred_messages')).toBe(true);
  });

  it('the server can still turn it off', () => {
    setServerFlags({ 'web.chat_starred_messages': false });
    expect(isFeatureEnabled('web.chat_starred_messages')).toBe(false);
  });
});

describe('feature-flags: access boundary flags removed', () => {
  it('does not export ACCESS_BOUNDARIES_READ_FLAG', () => {
    expect(featureFlagExports.ACCESS_BOUNDARIES_READ_FLAG).toBeUndefined();
  });

  it('does not export ACCESS_BOUNDARIES_AUTHORING_FLAG', () => {
    expect(featureFlagExports.ACCESS_BOUNDARIES_AUTHORING_FLAG).toBeUndefined();
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
    expect(featureFlagExports.NATIVE_CHAT_PALETTE_FLAG).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Removed v2 flag
// ---------------------------------------------------------------------------

describe('feature-flags: v2 flag removed', () => {
  it('does not export NATIVE_CHAT_V2_FLAG', () => {
    expect(featureFlagExports.NATIVE_CHAT_V2_FLAG).toBeUndefined();
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
// Removed terminal workspace flag (graduated, ptone/scion#4917)
// ---------------------------------------------------------------------------

describe('feature-flags: terminal workspace flag removed', () => {
  it('does not export TERMINAL_WORKSPACE_FLAG', () => {
    expect(featureFlagExports.TERMINAL_WORKSPACE_FLAG).toBeUndefined();
  });

  // The one allowed reference to the retired name outside compiledRetired
  // (see the experiment-removal checklist).
  it('the retired name resolves OFF by default (not in DEFAULT_ON_FLAGS)', () => {
    expect(isFeatureEnabled('web.terminal_workspace')).toBe(false);
  });

  it('unrelated flags not in DEFAULT_ON_FLAGS still default to false', () => {
    expect(isFeatureEnabled('test.flag')).toBe(false);
    expect(isFeatureEnabled('web.access_boundaries_read')).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Removed project thread groups flag (graduated, ptone/scion#5546)
// ---------------------------------------------------------------------------

describe('feature-flags: project thread groups flag removed', () => {
  // The one allowed reference to the retired name outside compiledRetired
  // (see the experiment-removal checklist).
  it('the retired name resolves OFF by default (not in DEFAULT_ON_FLAGS)', () => {
    expect(isFeatureEnabled('hub.chat_thread_groups')).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Consolidated Decisions flag (ptone/scion#8223)
// ---------------------------------------------------------------------------

describe('feature-flags: web.decisions consolidation', () => {
  it('does not export CHAT_CHOICES_FLAG or PROJECT_DECISIONS_FLAG', () => {
    expect((featureFlags as Record<string, unknown>).CHAT_CHOICES_FLAG).toBeUndefined();
    expect((featureFlags as Record<string, unknown>).PROJECT_DECISIONS_FLAG).toBeUndefined();
    expect((featureFlags as Record<string, unknown>).chatChoicesEnabled).toBeUndefined();
    expect((featureFlags as Record<string, unknown>).projectDecisionsEnabled).toBeUndefined();
  });

  // The one allowed reference to each retired name outside compiledRetired
  // (see the experiment-removal checklist).
  it('retired names resolve OFF by default and do not turn on web.decisions', () => {
    expect(isFeatureEnabled('web.chat_choices')).toBe(false);
    expect(isFeatureEnabled('web.project_decisions')).toBe(false);
    expect(decisionsEnabled()).toBe(false);
  });

  it('DECISIONS_FLAG is web.decisions, defaults OFF, and toggles decisionsEnabled()', () => {
    expect(DECISIONS_FLAG).toBe('web.decisions');
    expect(decisionsEnabled()).toBe(false);
    setFeatureFlag(DECISIONS_FLAG, true);
    expect(decisionsEnabled()).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Removed unified filter query dialect flag (graduated, ptone/scion#9034)
// ---------------------------------------------------------------------------

describe('feature-flags: unified filter query dialect flag removed', () => {
  it('does not export FILTERS_QUERY_DIALECT_FLAG or filtersQueryDialectEnabled', () => {
    // Type-level check: neither symbol is exported from feature-flags.ts.
    type HasFlag = 'FILTERS_QUERY_DIALECT_FLAG' extends keyof typeof featureFlags ? true : false;
    type HasHelper = 'filtersQueryDialectEnabled' extends keyof typeof featureFlags ? true : false;
    const hasFlag: HasFlag = false;
    const hasHelper: HasHelper = false;
    expect(hasFlag).toBe(false);
    expect(hasHelper).toBe(false);
    expect((featureFlags as Record<string, unknown>).FILTERS_QUERY_DIALECT_FLAG).toBeUndefined();
    expect((featureFlags as Record<string, unknown>).filtersQueryDialectEnabled).toBeUndefined();
  });

  // The one allowed reference to the retired name outside compiledRetired
  // (see the experiment-removal checklist).
  it('the retired name resolves OFF by default (not in DEFAULT_ON_FLAGS)', () => {
    expect(isFeatureEnabled('filters.query_dialect')).toBe(false);
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
// setServerFlags / resetServerFlagStateForTests — precedence matrix
// (ptone/scion#2217)
// ---------------------------------------------------------------------------

describe('setServerFlags: precedence', () => {
  it('a server value beats localStorage for a registered experiment', () => {
    localStorage.setItem('scion:feature:web.fixture_flag', 'true');
    setServerFlags({ 'web.fixture_flag': false });
    expect(isFeatureEnabled('web.fixture_flag')).toBe(false);
  });

  it('localStorage applies to names the server did not send', () => {
    localStorage.setItem('scion:feature:test.flag', 'true');
    setServerFlags({ 'web.fixture_flag': false });
    expect(isFeatureEnabled('test.flag')).toBe(true);
  });

  it('values in the bag before the first setServerFlags() call beat server values ("pinned")', () => {
    window.__SCION_FEATURES__ = { 'web.fixture_flag': true };
    setServerFlags({ 'web.fixture_flag': false });
    expect(isFeatureEnabled('web.fixture_flag')).toBe(true);
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
    window.__SCION_FEATURES__ = { 'web.fixture_flag': true };
    setServerFlags({ 'web.fixture_flag': false });
    expect(isFeatureEnabled('web.fixture_flag')).toBe(true);

    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    setServerFlags({ 'web.fixture_flag': false });
    expect(isFeatureEnabled('web.fixture_flag')).toBe(false);
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
    setServerFlags(['web.fixture_flag']);
    expect(window.__SCION_FEATURES__ ?? {}).not.toHaveProperty('0');
  });
});

// ---------------------------------------------------------------------------
// Shadowed-override logging — one console.info per flag per page load
// ---------------------------------------------------------------------------

describe('isFeatureEnabled: shadowed-override logging', () => {
  it('logs once when a localStorage override is shadowed by a server value, not once per call', () => {
    const infoSpy = vi.spyOn(console, 'info').mockImplementation(() => {});
    localStorage.setItem('scion:feature:web.fixture_flag', 'false');
    setServerFlags({ 'web.fixture_flag': true });

    isFeatureEnabled('web.fixture_flag');
    isFeatureEnabled('web.fixture_flag');
    isFeatureEnabled('web.fixture_flag');

    expect(infoSpy).toHaveBeenCalledTimes(1);
    infoSpy.mockRestore();
  });

  it('does not log when there is no localStorage override to shadow', () => {
    const infoSpy = vi.spyOn(console, 'info').mockImplementation(() => {});
    setServerFlags({ 'web.fixture_flag': true });

    isFeatureEnabled('web.fixture_flag');

    expect(infoSpy).not.toHaveBeenCalled();
    infoSpy.mockRestore();
  });

  it('logs independently per flag name', () => {
    const infoSpy = vi.spyOn(console, 'info').mockImplementation(() => {});
    localStorage.setItem('scion:feature:web.fixture_flag', 'false');
    localStorage.setItem('scion:feature:web.native_chat', 'false');
    setServerFlags({ 'web.fixture_flag': true, 'web.native_chat': true });

    isFeatureEnabled('web.fixture_flag');
    isFeatureEnabled('web.native_chat');

    expect(infoSpy).toHaveBeenCalledTimes(2);
    infoSpy.mockRestore();
  });
});

describe('feature-flags: change event', () => {
  it('announces server flags and a set flag on the window', async () => {
    const { FEATURE_FLAGS_CHANGED_EVENT } = await import('./feature-flags.js');
    const seen = vi.fn();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, seen);
    try {
      setServerFlags({ 'test.flag': true });
      setFeatureFlag('test.flag', false);
      expect(seen).toHaveBeenCalledTimes(2);
      expect(isFeatureEnabled('test.flag')).toBe(false);
    } finally {
      window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, seen);
    }
  });
});

describe('execSchedulesEnabled (hub.exec_schedules)', () => {
  it('defaults to false and follows server/explicit flags', async () => {
    const { EXEC_SCHEDULES_FLAG, execSchedulesEnabled } = await import('./feature-flags.js');
    expect(EXEC_SCHEDULES_FLAG).toBe('hub.exec_schedules');
    expect(execSchedulesEnabled()).toBe(false);

    setServerFlags({ 'hub.exec_schedules': true });
    expect(execSchedulesEnabled()).toBe(true);

    setFeatureFlag('hub.exec_schedules', false);
    expect(execSchedulesEnabled()).toBe(false);
  });
});

describe('k8sWorkloadIdentitySetupEnabled (hub.k8s_workload_identity_setup)', () => {
  it('defaults to false and follows server/explicit flags', async () => {
    const { K8S_WORKLOAD_IDENTITY_SETUP_FLAG, k8sWorkloadIdentitySetupEnabled } =
      await import('./feature-flags.js');
    expect(K8S_WORKLOAD_IDENTITY_SETUP_FLAG).toBe('hub.k8s_workload_identity_setup');
    expect(k8sWorkloadIdentitySetupEnabled()).toBe(false);

    setServerFlags({ 'hub.k8s_workload_identity_setup': true });
    expect(k8sWorkloadIdentitySetupEnabled()).toBe(true);

    setFeatureFlag('hub.k8s_workload_identity_setup', false);
    expect(k8sWorkloadIdentitySetupEnabled()).toBe(false);
  });
});

describe('idleSuspendEnabled (lifecycle.idle_suspend)', () => {
  it('defaults to false and follows server/explicit flags', async () => {
    const { IDLE_SUSPEND_FLAG, idleSuspendEnabled } = await import('./feature-flags.js');
    expect(IDLE_SUSPEND_FLAG).toBe('lifecycle.idle_suspend');
    expect(idleSuspendEnabled()).toBe(false);

    setServerFlags({ 'lifecycle.idle_suspend': true });
    expect(idleSuspendEnabled()).toBe(true);

    setFeatureFlag('lifecycle.idle_suspend', false);
    expect(idleSuspendEnabled()).toBe(false);
  });
});
