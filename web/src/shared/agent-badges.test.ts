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
  MODEL_SIZE_CHIPS,
  UNKNOWN_VENDOR_MARK,
  VENDOR_MARKS,
  agentBadgeView,
  vendorMark,
} from './agent-badges.js';

describe('agent badge view', () => {
  it('has a mark for every vendor pkg/harness Vendor returns', () => {
    // pkg/harness/vendor.go harnessVendors values.
    for (const v of ['anthropic', 'google', 'openai', 'xai', 'meta']) {
      expect(VENDOR_MARKS[v]?.multiVendor ?? false, v).toBe(false);
    }
    for (const v of ['opencode', 'copilot', 'hermes']) {
      expect(VENDOR_MARKS[v]?.multiVendor, v).toBe(true);
    }
  });

  it('points every mark at an image file named after its vendor id', () => {
    for (const [vendor, mark] of Object.entries(VENDOR_MARKS)) {
      expect(mark.src, vendor).toMatch(new RegExp(`vendor-marks/${vendor}\\.png`));
    }
    expect(UNKNOWN_VENDOR_MARK.src).toMatch(/vendor-marks\/unknown\.png/);
    // Never inlined as a data URI (?no-inline): each is a cacheable file.
    for (const mark of [...Object.values(VENDOR_MARKS), UNKNOWN_VENDOR_MARK]) {
      expect(mark.src.startsWith('data:')).toBe(false);
    }
  });

  it('maps unknown, empty and differently cased vendors', () => {
    expect(vendorMark(undefined)).toBe(UNKNOWN_VENDOR_MARK);
    expect(vendorMark('')).toBe(UNKNOWN_VENDOR_MARK);
    expect(vendorMark('acme')).toBe(UNKNOWN_VENDOR_MARK);
    expect(vendorMark('Anthropic')).toBe(VENDOR_MARKS.anthropic);
  });

  it('shows a chip for each size alias and none for an unknown or empty size', () => {
    expect(Object.keys(MODEL_SIZE_CHIPS)).toEqual(['small', 'medium', 'large', 'extra-large']);
    expect(agentBadgeView('google', 'medium').size?.chip).toBe('M');
    expect(agentBadgeView('google', '').size).toBeNull();
    expect(agentBadgeView('google', undefined).size).toBeNull();
    expect(agentBadgeView('google', 'huge').size).toBeNull();
  });

  it('drops the size for a multi-vendor harness', () => {
    const view = agentBadgeView('hermes', 'large', 'hermes');
    expect(view.size).toBeNull();
    expect(view.title).toBe('Harness: Hermes');
  });
});
