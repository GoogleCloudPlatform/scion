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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { withTimeout } from './with-timeout.js';

describe('withTimeout (review R3-1)', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('resolves with the promise value when it settles before the budget (late-arrival-is-fine path)', async () => {
    const inner = new Promise<string>((resolve) => setTimeout(() => resolve('ok'), 500));
    const result = withTimeout(inner, 1500);

    await vi.advanceTimersByTimeAsync(500);

    await expect(result).resolves.toBe('ok');
    // Review R4-2: the budget timer must be cleared once `inner` settles,
    // not left pending until it would have fired on its own — pins
    // `clearTimeout(timer)` on the fulfil path.
    expect(vi.getTimerCount()).toBe(0);
  });

  it('resolves to undefined when the promise hangs past the budget', async () => {
    const hang = new Promise<string>(() => {
      /* never settles */
    });
    const result = withTimeout(hang, 1500);

    await vi.advanceTimersByTimeAsync(1500);

    await expect(result).resolves.toBeUndefined();
  });

  it('applies the hung promise\'s side effect once it eventually lands (late-arrival path)', async () => {
    let sideEffect = '';
    const hang = new Promise<void>((resolve) => {
      setTimeout(() => {
        sideEffect = 'applied';
        resolve();
      }, 5000); // well past the budget
    });
    const result = withTimeout(hang, 1500);

    await vi.advanceTimersByTimeAsync(1500);
    await expect(result).resolves.toBeUndefined();
    expect(sideEffect).toBe(''); // not yet — still in flight

    await vi.advanceTimersByTimeAsync(3500);
    expect(sideEffect).toBe('applied'); // the original promise was never abandoned
  });

  it('propagates a rejection that happens before the budget', async () => {
    const failing = new Promise<string>((_resolve, reject) =>
      setTimeout(() => reject(new Error('boom')), 100)
    );
    const result = withTimeout(failing, 1500);
    // Attach the assertion's rejection handler synchronously, before
    // advancing timers — otherwise Node sees `result` rejected with no
    // handler yet attached and logs a (harmless, but noisy) "handled
    // asynchronously" warning.
    const assertion = expect(result).rejects.toThrow('boom');

    await vi.advanceTimersByTimeAsync(100);
    await assertion;
    // Review R4-2: pins `clearTimeout(timer)` on the reject path too.
    expect(vi.getTimerCount()).toBe(0);
  });

  it('does not reject from a late failure after the budget already resolved undefined', async () => {
    const lateFailure = new Promise<string>((_resolve, reject) =>
      setTimeout(() => reject(new Error('too late to matter')), 5000)
    );
    const result = withTimeout(lateFailure, 1500);
    const assertion = expect(result).resolves.toBeUndefined();

    await vi.advanceTimersByTimeAsync(1500);
    await assertion;

    // The late rejection must not become an unhandled rejection or retroactively
    // change the already-settled `result` promise.
    await vi.advanceTimersByTimeAsync(3500);
  });
});
