// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Shared by the chat and terminal palette e2e suites: the quick palette's
 * computed text sizes, and the dense scale both hosts are expected to show.
 */

import type { Page } from '@playwright/test';

/** The palette's dense type scale, in computed pixels (16px root). */
export const DENSE_PALETTE_FONT_SIZES = {
  input: '15px',
  groupHeading: '11px',
  secondary: '12px',
  help: '11px',
  kbd: '10px',
} as const;

export type PaletteFontSizes = Record<keyof typeof DENSE_PALETTE_FONT_SIZES, string | null>;

/** Reads the open palette's computed font sizes (null for an element that is not rendered). */
export function paletteFontSizes(page: Page): Promise<PaletteFontSizes> {
  return page.locator('scion-quick-palette').evaluate((el) => {
    const root = el.shadowRoot;
    const size = (selector: string): string | null => {
      const node = root?.querySelector(selector);
      return node ? getComputedStyle(node).fontSize : null;
    };
    return {
      input: size('#palette-query-input'),
      groupHeading: size('.palette-group-heading'),
      secondary: size('.palette-secondary'),
      help: size('.palette-help'),
      kbd: size('.palette-help kbd'),
    };
  });
}
