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
 *
 * Every launch here has a shape the hub can send. `active` is the hub's
 * in-flight predicate, so it is true only in the pre-running phases; a create
 * that winds down after a failure or stop keeps `state: 'active'` with
 * `active: false` (store.ComputeAgentLaunch, pkg/store/launch_view.go).
 */

import { describe, it, expect } from 'vitest';

import type { AgentLaunch, AgentPhase } from './types.js';
import { isAgentLaunching, isIncompleteCreate } from './types.js';

const base = { id: 'l-1', kind: 'create' };

/** A create launch in flight: only in created, provisioning, cloning, starting. */
const inFlight: AgentLaunch = { ...base, state: 'active', active: true, step: 'launching' };

/** A create launch winding down after a failure or stop, before its error is recorded. */
const windingDown: AgentLaunch = { ...base, state: 'active', active: false, step: 'launching' };

/** A create launch that failed. */
const failed: AgentLaunch = {
  ...base,
  state: 'ended',
  active: false,
  error: 'runtime_error',
  endReason: 'failed',
};

/** A create launch that succeeded. */
const succeeded: AgentLaunch = { ...base, state: 'ended', active: false, endReason: 'succeeded' };

const inFlightPhases: AgentPhase[] = ['created', 'provisioning', 'cloning', 'starting'];
const incompletePhases: AgentPhase[] = ['suspended', 'stopping', 'stopped', 'error'];

describe('isAgentLaunching', () => {
  it.each(inFlightPhases)('is true for a launch in flight in phase %s', (phase) => {
    expect(isAgentLaunching({ phase, launch: inFlight })).toBe(true);
  });

  it.each(incompletePhases)('is false for a create winding down in phase %s', (phase) => {
    expect(isAgentLaunching({ phase, launch: windingDown })).toBe(false);
  });

  it('is false for a launch that succeeded', () => {
    expect(isAgentLaunching({ phase: 'running', launch: succeeded })).toBe(false);
  });

  it('is false for a launch that failed', () => {
    expect(isAgentLaunching({ phase: 'error', launch: failed })).toBe(false);
  });

  it('is false without a launch', () => {
    expect(isAgentLaunching({ phase: 'provisioning' })).toBe(false);
  });
});

describe('isIncompleteCreate', () => {
  it.each(incompletePhases)('is true for a failed create in phase %s', (phase) => {
    expect(isIncompleteCreate({ phase, launch: failed })).toBe(true);
  });

  it.each(incompletePhases)('is true for a create winding down in phase %s', (phase) => {
    expect(isIncompleteCreate({ phase, launch: windingDown })).toBe(true);
  });

  it('is false for an agent stopped after its create succeeded', () => {
    expect(isIncompleteCreate({ phase: 'stopped', launch: succeeded })).toBe(false);
  });

  it('is false for an agent that runs or is still launching', () => {
    expect(isIncompleteCreate({ phase: 'running', launch: succeeded })).toBe(false);
    expect(isIncompleteCreate({ phase: 'provisioning', launch: inFlight })).toBe(false);
  });

  it('is false for a launch that is not a create', () => {
    const start: AgentLaunch = { ...failed, kind: 'start' };
    expect(isIncompleteCreate({ phase: 'error', launch: start })).toBe(false);
  });

  it('is false without a launch', () => {
    expect(isIncompleteCreate({ phase: 'error' })).toBe(false);
  });
});
