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
 * Tests for the best-effort resume lifecycle action (#1868): the shared
 * request-body builder used by the agent-detail, agents, and project-detail
 * pages so that a "Resume (best effort)" click on an error-phase agent asks
 * the Hub's /start endpoint to resume the harness session instead of
 * starting fresh.
 */

import { describe, it, expect } from 'vitest';
import { lifecycleActionRequestInit, RESUME_BEST_EFFORT_CONFIRM_MESSAGE } from './types.js';

describe('lifecycleActionRequestInit', () => {
  it('sends forceResume:true in a JSON body for force-resume', () => {
    const init = lifecycleActionRequestInit('force-resume');
    expect(init.method).toBe('POST');
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' });
    expect(init.body).toBe(JSON.stringify({ forceResume: true }));
  });

  it.each(['start', 'stop', 'suspend', 'resume', 'delete'] as const)(
    'posts with no body for %s',
    (action) => {
      const init = lifecycleActionRequestInit(action);
      expect(init).toEqual({ method: 'POST' });
    }
  );
});

describe('RESUME_BEST_EFFORT_CONFIRM_MESSAGE', () => {
  it('warns that resume is best-effort and mentions the fresh-start alternative', () => {
    expect(RESUME_BEST_EFFORT_CONFIRM_MESSAGE).toContain('Resume');
    expect(RESUME_BEST_EFFORT_CONFIRM_MESSAGE).toContain('Start');
  });
});
