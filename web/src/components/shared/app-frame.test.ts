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

import { describe, it, expect, afterEach } from 'vitest';
import { enterAppFrame, exitAppFrame, _appFrameRefCountForTests } from './app-frame.js';

function resetFrameState(): void {
  while (_appFrameRefCountForTests() > 0) {
    exitAppFrame();
  }
  document.documentElement.classList.remove('scion-app-frame');
}

describe('app-frame ref counting', () => {
  afterEach(() => {
    resetFrameState();
  });

  it('adds the class on first entry and removes it on last exit', () => {
    expect(document.documentElement.classList.contains('scion-app-frame')).toBe(false);
    enterAppFrame();
    expect(document.documentElement.classList.contains('scion-app-frame')).toBe(true);
    exitAppFrame();
    expect(document.documentElement.classList.contains('scion-app-frame')).toBe(false);
  });

  it('keeps the class while any shell still holds it (route-swap overlap)', () => {
    enterAppFrame(); // old shell connects
    enterAppFrame(); // new shell connects before the old one disconnects
    exitAppFrame(); // old shell disconnects
    expect(document.documentElement.classList.contains('scion-app-frame')).toBe(true);
    exitAppFrame(); // new shell disconnects
    expect(document.documentElement.classList.contains('scion-app-frame')).toBe(false);
  });

  it('never goes negative on an unbalanced exit', () => {
    exitAppFrame();
    expect(_appFrameRefCountForTests()).toBe(0);
    enterAppFrame();
    expect(_appFrameRefCountForTests()).toBe(1);
    exitAppFrame();
    expect(_appFrameRefCountForTests()).toBe(0);
  });
});
