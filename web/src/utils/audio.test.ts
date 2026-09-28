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
 * audio — unit tests.
 *
 * Covers the localStorage-backed chime preferences (global + per-project),
 * the shouldChime() combinator, and the playChimeThrottled() cooldown. A
 * fake AudioContext stands in for the real Web Audio API (unavailable in
 * happy-dom) so the throttle can be asserted on by counting oscillators
 * actually created, not just by checking nothing threw.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import {
  isChimeEnabled,
  setChimeEnabled,
  isProjectChimeEnabled,
  setProjectChimeEnabled,
  shouldChime,
  playChime,
  playChimeThrottled,
} from './audio.js';

const PROJECT = 'proj-1';

/** Counts oscillators created across all chime plays in a test. */
let oscillatorsCreated = 0;

class FakeAudioParam {
  setValueAtTime(): void {}
  exponentialRampToValueAtTime(): void {}
}

class FakeOscillator {
  type = 'sine';
  frequency = new FakeAudioParam();
  connect(): void {}
  start(): void {}
  stop(): void {}
  constructor() {
    oscillatorsCreated++;
  }
}

class FakeGain {
  gain = new FakeAudioParam();
  connect(): void {}
}

class FakeAudioContext {
  state: 'running' | 'suspended' = 'running';
  currentTime = 0;
  destination = {};
  resume(): void {
    this.state = 'running';
  }
  createGain(): FakeGain {
    return new FakeGain();
  }
  createOscillator(): FakeOscillator {
    return new FakeOscillator();
  }
}

beforeEach(() => {
  localStorage.clear();
  oscillatorsCreated = 0;
  (window as unknown as { AudioContext: unknown }).AudioContext = FakeAudioContext;
});

describe('global chime preference', () => {
  it('defaults to on', () => {
    expect(isChimeEnabled()).toBe(true);
  });

  it('turns off and back on', () => {
    setChimeEnabled(false);
    expect(isChimeEnabled()).toBe(false);

    setChimeEnabled(true);
    expect(isChimeEnabled()).toBe(true);
  });
});

describe('per-project chime preference', () => {
  it('defaults to on for any project', () => {
    expect(isProjectChimeEnabled(PROJECT)).toBe(true);
  });

  it('turns off and back on independently per project', () => {
    setProjectChimeEnabled(PROJECT, false);
    expect(isProjectChimeEnabled(PROJECT)).toBe(false);
    expect(isProjectChimeEnabled('proj-2')).toBe(true);

    setProjectChimeEnabled(PROJECT, true);
    expect(isProjectChimeEnabled(PROJECT)).toBe(true);
  });
});

describe('shouldChime', () => {
  it('is true when both global and project prefs are on', () => {
    expect(shouldChime(PROJECT)).toBe(true);
  });

  it('is false when the global toggle is off', () => {
    setChimeEnabled(false);
    expect(shouldChime(PROJECT)).toBe(false);
  });

  it('is false when the project toggle is off', () => {
    setProjectChimeEnabled(PROJECT, false);
    expect(shouldChime(PROJECT)).toBe(false);
  });
});

describe('playChime', () => {
  // Runs before any other test in this file constructs the module-level
  // AudioContext singleton, so this genuinely exercises the "no AudioContext
  // available yet" path rather than reusing an already-cached instance.
  it('does not throw when AudioContext is unavailable', () => {
    delete (window as unknown as { AudioContext?: unknown }).AudioContext;
    expect(() => playChime()).not.toThrow();
  });

  it('plays two tones via the Web Audio API', () => {
    playChime();
    expect(oscillatorsCreated).toBe(2);
  });
});

describe('playChimeThrottled', () => {
  let now: number;
  // playChimeThrottled's cooldown state is a module-level singleton, so each
  // test must start far enough ahead of the previous test's last chime that
  // the cooldown from one test can never bleed into the next.
  let testIndex = 0;

  beforeEach(() => {
    testIndex += 1;
    now = testIndex * 1_000_000;
    vi.spyOn(Date, 'now').mockImplementation(() => now);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('does not play when chime is disabled', () => {
    setChimeEnabled(false);
    playChimeThrottled(PROJECT);
    expect(oscillatorsCreated).toBe(0);
  });

  it('plays on the first call', () => {
    playChimeThrottled(PROJECT);
    expect(oscillatorsCreated).toBe(2);
  });

  it('suppresses a second chime within the cooldown window', () => {
    playChimeThrottled(PROJECT);
    now += 500; // well within the 2s cooldown
    playChimeThrottled(PROJECT);
    expect(oscillatorsCreated).toBe(2); // only the first call played
  });

  it('plays again once the cooldown has elapsed', () => {
    playChimeThrottled(PROJECT);
    now += 2001;
    playChimeThrottled(PROJECT);
    expect(oscillatorsCreated).toBe(4); // both calls played
  });
});
