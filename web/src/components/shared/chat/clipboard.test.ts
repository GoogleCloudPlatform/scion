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
import { copyToClipboard } from './clipboard.js';
import { copyToClipboard as sharedCopyToClipboard } from '../../../utils/clipboard.js';

// The helper's behaviour is tested in web/src/utils/clipboard.test.ts.
describe('chat clipboard re-export', () => {
  it('is the shared copyToClipboard helper', () => {
    expect(copyToClipboard).toBe(sharedCopyToClipboard);
  });
});
