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
 * Verifies that main.ts injects theme.css *before* an existing server-rendered
 * <style id="scion-branding-tokens"> element so custom branding tokens emitted
 * in <head> by pkg/hub/web.go win the equal-specificity CSS cascade on boot
 * instead of reverting to stock theme.css colors.
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';

vi.mock('../styles/theme.css?inline', () => ({
  default: 'html:root, :host, .sl-theme-light { --scion-primary: #3b82f6; }',
}));

vi.mock('../components/pages/home.js', () => {
  if (!customElements.get('scion-page-home')) {
    customElements.define('scion-page-home', class extends HTMLElement {});
  }
  return {};
});

describe('main.ts branding token stylesheet cascade order', () => {
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('inserts theme.css before #scion-branding-tokens so the branded palette wins', async () => {
    const brandingStyle = document.createElement('style');
    brandingStyle.id = 'scion-branding-tokens';
    brandingStyle.textContent = 'html:root, :host, .sl-theme-light { --scion-primary: #0f766e; }\n';
    document.head.appendChild(brandingStyle);

    await bootMainAt('/');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-home')).not.toBeNull(),
      BOOT_WAIT
    );

    const headStyles = Array.from(document.head.querySelectorAll('style'));
    const themeStyle = headStyles.find(
      (el) => el !== brandingStyle && (el.textContent ?? '').includes('--scion-primary: #3b82f6')
    );
    expect(themeStyle).toBeDefined();

    // theme.css must precede #scion-branding-tokens in document order so equal-specificity
    // html:root branding rules win the cascade instead of reverting to stock colors.
    const pos = themeStyle!.compareDocumentPosition(brandingStyle);
    expect(pos & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(
      getComputedStyle(document.documentElement).getPropertyValue('--scion-primary').trim()
    ).toBe('#0f766e');
  }, 15_000);
});
