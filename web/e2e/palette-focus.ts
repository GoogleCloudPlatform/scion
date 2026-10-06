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
 * Shared by the chat, terminal and graph palette e2e suites: whether the
 * palette's query input holds keyboard focus, and a slow palette module to
 * widen the window between an open and that focus.
 */

import type { Page } from '@playwright/test';

/**
 * Whether the deepest focused element, through open shadow roots, is the
 * query input of a `<scion-quick-palette>`.
 */
export function paletteInputHasFocus(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
    const root = el?.getRootNode();
    return (
      el?.id === 'palette-query-input' &&
      root instanceof ShadowRoot &&
      root.host.tagName === 'SCION-QUICK-PALETTE'
    );
  });
}

/**
 * Delays the palette component module by `ms`, as a first open over a slow
 * network would see it. Keys typed meanwhile must still become the query.
 */
export async function slowPaletteModule(page: Page, ms = 400): Promise<void> {
  await page.route(/\/quick-palette\.ts(\?|$)/, async (route) => {
    await new Promise((resolve) => setTimeout(resolve, ms));
    await route.continue();
  });
}
