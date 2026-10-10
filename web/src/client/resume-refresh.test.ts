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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { ResumeRefresh, RESUME_REFRESH_AFTER_MS } from './resume-refresh.js';

let visibility: DocumentVisibilityState = 'visible';
let clock = 0;

function setVisibility(state: DocumentVisibilityState): void {
  visibility = state;
  document.dispatchEvent(new Event('visibilitychange'));
}

function pageshow(persisted: boolean): void {
  const e = new Event('pageshow');
  Object.defineProperty(e, 'persisted', { value: persisted });
  window.dispatchEvent(e);
}

describe('ResumeRefresh', () => {
  let onResume: ReturnType<typeof vi.fn>;
  let resume: ResumeRefresh;

  beforeEach(() => {
    visibility = 'visible';
    clock = 1_000_000;
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visibility);
    onResume = vi.fn();
    resume = new ResumeRefresh(onResume, { now: () => clock });
    resume.start();
  });

  afterEach(() => {
    resume.stop();
    vi.restoreAllMocks();
  });

  it('fires when the page is shown after a long time hidden', () => {
    setVisibility('hidden');
    clock += RESUME_REFRESH_AFTER_MS;
    setVisibility('visible');
    expect(onResume).toHaveBeenCalledTimes(1);
  });

  it('does not fire after a short time hidden', () => {
    setVisibility('hidden');
    clock += RESUME_REFRESH_AFTER_MS - 1;
    setVisibility('visible');
    expect(onResume).not.toHaveBeenCalled();
  });

  it('measures from the first hidden event, and fires once per return', () => {
    setVisibility('hidden');
    clock += RESUME_REFRESH_AFTER_MS / 2;
    setVisibility('hidden');
    clock += RESUME_REFRESH_AFTER_MS / 2;
    setVisibility('visible');
    setVisibility('visible');
    expect(onResume).toHaveBeenCalledTimes(1);
  });

  it('does not fire on a visible event with no hidden one before it', () => {
    clock += RESUME_REFRESH_AFTER_MS * 2;
    setVisibility('visible');
    expect(onResume).not.toHaveBeenCalled();
  });

  it('counts time hidden from start() when the page starts hidden', () => {
    resume.stop();
    visibility = 'hidden';
    resume.start();
    clock += RESUME_REFRESH_AFTER_MS;
    setVisibility('visible');
    expect(onResume).toHaveBeenCalledTimes(1);
  });

  it('fires on a back/forward cache restore, not on a normal pageshow', () => {
    pageshow(false);
    expect(onResume).not.toHaveBeenCalled();
    pageshow(true);
    expect(onResume).toHaveBeenCalledTimes(1);
  });

  it('stops listening once stopped', () => {
    resume.stop();
    setVisibility('hidden');
    clock += RESUME_REFRESH_AFTER_MS;
    setVisibility('visible');
    pageshow(true);
    expect(onResume).not.toHaveBeenCalled();
  });
});
