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
 * The web's copies of the hub's launch predicates (ptone/scion#4664):
 * isAgentLaunching mirrors store.Agent.IsInFlight and isIncompleteCreate
 * mirrors store.Agent.IsIncompleteCreate (pkg/store/models.go).
 */

import { describe, it, expect } from 'vitest';

import type { AgentLaunch, AgentPhase } from './types.js';
import { isAgentLaunching, isIncompleteCreate } from './types.js';

function launch(overrides: Partial<AgentLaunch> = {}): AgentLaunch {
  return { id: 'l-1', state: 'active', active: true, kind: 'create', ...overrides };
}

const ended = launch({ state: 'ended', active: false });

describe('isAgentLaunching', () => {
  it.each<AgentPhase>(['created', 'provisioning', 'cloning', 'starting'])(
    'is true for an active launch in phase %s',
    (phase) => {
      expect(isAgentLaunching({ phase, launch: launch() })).toBe(true);
    }
  );

  it.each<AgentPhase>(['running', 'error', 'stopped'])(
    'is false for an active launch in phase %s',
    (phase) => {
      expect(isAgentLaunching({ phase, launch: launch() })).toBe(false);
    }
  );

  it('is false for an ended launch', () => {
    expect(isAgentLaunching({ phase: 'provisioning', launch: ended })).toBe(false);
  });

  it('is false without a launch', () => {
    expect(isAgentLaunching({ phase: 'provisioning' })).toBe(false);
  });
});

describe('isIncompleteCreate', () => {
  it.each<AgentPhase>(['suspended', 'stopping', 'stopped', 'error'])(
    'is true for a failed create launch in phase %s',
    (phase) => {
      expect(isIncompleteCreate({ phase, launch: { ...ended, error: 'runtime_error' } })).toBe(
        true
      );
    }
  );

  it('is true for a create launch still winding down on error', () => {
    expect(isIncompleteCreate({ phase: 'error', launch: launch() })).toBe(true);
  });

  it('is false for an ended create launch with no error', () => {
    expect(isIncompleteCreate({ phase: 'stopped', launch: { ...ended, endReason: 'succeeded' } })).toBe(
      false
    );
  });

  it('is false in a phase the agent ran or is launching in', () => {
    expect(
      isIncompleteCreate({ phase: 'running', launch: { ...ended, error: 'runtime_error' } })
    ).toBe(false);
    expect(isIncompleteCreate({ phase: 'provisioning', launch: launch() })).toBe(false);
  });

  it('is false for a launch that is not a create', () => {
    expect(
      isIncompleteCreate({ phase: 'error', launch: { ...ended, kind: 'start', error: 'x' } })
    ).toBe(false);
  });

  it('is false without a launch', () => {
    expect(isIncompleteCreate({ phase: 'error' })).toBe(false);
  });
});
