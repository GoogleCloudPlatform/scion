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
import { stopAllNotices } from './stop-all.js';

describe('stopAllNotices', () => {
  it('shows nothing when every agent stopped', () => {
    expect(stopAllNotices({ stopped: 3, failed: 0 })).toEqual([]);
  });

  it('warns about failures', () => {
    expect(stopAllNotices({ stopped: 2, failed: 1 })).toEqual([
      { message: 'Stopped 2 agents, 1 failed.', variant: 'warning' },
    ]);
  });

  it('shows a neutral notice for agents whose start was in flight', () => {
    const notices = stopAllNotices({ stopped: 1, failed: 0, stopRecorded: 2 });
    expect(notices).toHaveLength(1);
    expect(notices[0].variant).toBe('neutral');
    expect(notices[0].message).toContain('Stop recorded for 2 agents still starting');
  });

  it('shows both notices in order', () => {
    const notices = stopAllNotices({ stopped: 0, failed: 1, stopRecorded: 1 });
    expect(notices.map((n) => n.variant)).toEqual(['warning', 'neutral']);
    expect(notices[1].message).toContain('Stop recorded for 1 agent still starting');
  });
});
