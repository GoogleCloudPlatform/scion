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
import { ATTACHMENT_ONLY_LABEL, replyToContent } from './message-text.js';

describe('replyToContent', () => {
  it('labels a message without text as an attachment (ptone/scion#6146)', () => {
    expect(ATTACHMENT_ONLY_LABEL).toBe('Attachment');
    expect(replyToContent('')).toBe('Attachment');
    expect(replyToContent('  \n ')).toBe('Attachment');
    expect(replyToContent(undefined)).toBe('Attachment');
    expect(replyToContent(null)).toBe('Attachment');
  });

  it('keeps short text and shortens long text to 100 characters', () => {
    expect(replyToContent('hello')).toBe('hello');
    const long = replyToContent('x'.repeat(150));
    expect(long).toBe('x'.repeat(100) + '...');
  });
});
