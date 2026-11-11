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
 * Sanity checks that the test harness itself is emulating a touch device
 * correctly, plus the viewport meta content.
 *
 * No gestures — runs on every project, including webkit-iphone @static.
 */

import { test, expect } from '@playwright/test';
import { openChatRail } from './fixture.js';

test('@static mobile projects report pointer:coarse and hover:none', async ({ page }, testInfo) => {
  await openChatRail(page);
  const media = await page.evaluate(() => ({
    coarse: window.matchMedia('(pointer: coarse)').matches,
    hoverNone: window.matchMedia('(hover: none)').matches,
  }));

  const isMobileProject = testInfo.project.name !== 'desktop-1440';
  expect(media.coarse, `${testInfo.project.name}: pointer:coarse`).toBe(isMobileProject);
  expect(media.hoverNone, `${testInfo.project.name}: hover:none`).toBe(isMobileProject);
});

test('@static viewport meta has viewport-fit=cover, interactive-widget and no zoom-disabling attributes', async ({
  page,
}) => {
  await openChatRail(page);
  const content = await page.evaluate(
    () => document.querySelector('meta[name="viewport"]')?.getAttribute('content') || ''
  );

  expect(content).toContain('width=device-width');
  expect(content).toContain('viewport-fit=cover');
  expect(content).toContain('interactive-widget=resizes-content');
  expect(content).not.toContain('maximum-scale');
  expect(content).not.toContain('user-scalable');
});
