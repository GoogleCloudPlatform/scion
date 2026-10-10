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
import { isMacPlatform } from './platform.js';

describe('isMacPlatform', () => {
  const originalPlatform = Object.getOwnPropertyDescriptor(window.navigator, 'platform');
  const originalUAData = Object.getOwnPropertyDescriptor(window.navigator, 'userAgentData');

  afterEach(() => {
    if (originalPlatform) {
      Object.defineProperty(window.navigator, 'platform', originalPlatform);
    }
    if (originalUAData) {
      Object.defineProperty(window.navigator, 'userAgentData', originalUAData);
    } else {
      delete (window.navigator as unknown as Record<string, unknown>).userAgentData;
    }
  });

  function setPlatform(platform: string): void {
    Object.defineProperty(window.navigator, 'platform', { value: platform, configurable: true });
  }

  function setUserAgentDataPlatform(platform: string | undefined): void {
    Object.defineProperty(window.navigator, 'userAgentData', {
      value: platform === undefined ? undefined : { platform },
      configurable: true,
    });
  }

  it('without Client Hints, falls back to the navigator.platform regex', () => {
    setUserAgentDataPlatform(undefined);
    setPlatform('MacIntel');
    expect(isMacPlatform()).toBe(true);

    setPlatform('Linux x86_64');
    expect(isMacPlatform()).toBe(false);
  });

  it('prefers navigator.userAgentData.platform over navigator.platform when both are present', () => {
    // navigator.platform disagrees on purpose, to prove Client Hints wins.
    setPlatform('Linux x86_64');
    setUserAgentDataPlatform('macOS');
    expect(isMacPlatform()).toBe(true);

    setPlatform('MacIntel');
    setUserAgentDataPlatform('Windows');
    expect(isMacPlatform()).toBe(false);
  });

  it('returns false, not throw, when navigator is unavailable', () => {
    vi.stubGlobal('navigator', undefined);
    expect(isMacPlatform()).toBe(false);
    vi.unstubAllGlobals();
  });
});
