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
 * Self-test for `assertNoHorizontalOverflow`: proves the helper actually
 * detects an overflowing element several plain-DOM levels below the active
 * panel (not just the panel's own rect), so the helper cannot silently stop
 * checking most of the panel again without a test failing here first.
 */

import { test, expect } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { assertNoHorizontalOverflow } from './helpers.js';

test('@static assertNoHorizontalOverflow detects an element that overflows deep inside the active panel', async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name === 'desktop-1440',
    'on desktop all three panels are on screen, so the active panel left === 0 check already ' +
      'throws regardless of the probe — this self-test is only meaningful on mobile'
  );
  await openChatRail(page);
  await openGeneralThread(page);

  // Inject into the thread header — several plain-DOM levels below the
  // active panel element itself, in the same shadow root, which is exactly
  // the kind of content a shallow "check the panel's own rect only" scan
  // would miss.
  await page.evaluate(() => {
    const pageEl = document.querySelector('scion-page-chat') as
      | (HTMLElement & { shadowRoot: ShadowRoot })
      | null;
    const header = pageEl?.shadowRoot?.querySelector('.v2-thread-header');
    if (!header) throw new Error('test setup: .v2-thread-header not found');
    const probe = document.createElement('span');
    probe.setAttribute('data-overflow-probe', '');
    // position:absolute takes it out of the header's flex layout, so a flex
    // child's automatic shrink-to-fit can't defeat the fixed width.
    probe.style.cssText = 'position:absolute;top:0;left:0;width:2000px;height:1px;';
    header.appendChild(probe);
  });

  // Matches the specific assertion message so a different, unrelated check
  // inside the helper failing first can't also satisfy this expectation.
  await expect(assertNoHorizontalOverflow(page)).rejects.toThrow(
    /no visible element right edge past innerWidth/
  );
});
