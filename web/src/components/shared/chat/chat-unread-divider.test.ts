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
import { hasMessagesAfter, unreadDividerAfter } from './chat-unread-divider.js';

describe('unreadDividerAfter', () => {
  const ids = ['m1', 'm2', 'm3'];

  it('follows a loaded last-read message', () => {
    expect(unreadDividerAfter(ids, 'm1', false)).toBe(0);
    expect(unreadDividerAfter(ids, 'm2', true)).toBe(1);
  });

  it('falls at the top when the watermark was purged by retention', () => {
    expect(unreadDividerAfter(ids, 'purged', true)).toBe(-1);
    expect(hasMessagesAfter(-1, ids.length)).toBe(true);
  });

  it('shows none for a watermark that may be on an older page', () => {
    expect(unreadDividerAfter(ids, 'older', false)).toBeNull();
  });

  it('shows none without a watermark or messages', () => {
    expect(unreadDividerAfter(ids, '', true)).toBeNull();
    expect(unreadDividerAfter([], 'purged', true)).toBeNull();
  });

  it('has nothing below the newest message', () => {
    expect(hasMessagesAfter(2, ids.length)).toBe(false);
    expect(hasMessagesAfter(null, ids.length)).toBe(false);
  });
});
