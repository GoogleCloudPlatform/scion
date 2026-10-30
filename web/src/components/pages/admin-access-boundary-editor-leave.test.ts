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
 * The access-boundary editor asks through the router's leave guard
 * (ptone/scion#3460): with unsaved changes, leaving needs the user's
 * confirmation; a clean draft leaves freely; the guard goes with the page.
 */

import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { mayLeave } from '../../client/navigation.js';

type Editor = HTMLElement & { updateComplete: Promise<boolean>; isDirty: boolean };

const confirmSpy = vi.fn(() => false);

beforeAll(async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.reject(new Error('unexpected fetch')))
  );
  vi.stubGlobal('confirm', confirmSpy);
  await import('./admin-access-boundary-editor.js');
}, 30_000);

afterEach(() => {
  document.body.replaceChildren();
  window.history.replaceState({}, '', '/');
  confirmSpy.mockReset();
});

afterAll(() => {
  vi.unstubAllGlobals();
});

async function mountEditor(): Promise<Editor> {
  window.history.replaceState({}, '', '/admin/access-boundaries/new');
  const el = document.createElement('scion-page-admin-access-boundary-editor') as Editor;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('access-boundary editor leave guard', () => {
  it('leaves a clean draft without asking', async () => {
    await mountEditor();
    expect(mayLeave()).toBe(true);
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it('asks with unsaved changes, and stays or leaves as the user chooses', async () => {
    const el = await mountEditor();
    el.isDirty = true;

    confirmSpy.mockReturnValueOnce(false);
    expect(mayLeave()).toBe(false);
    confirmSpy.mockReturnValueOnce(true);
    expect(mayLeave()).toBe(true);
    expect(confirmSpy).toHaveBeenCalledTimes(2);
  });

  it('removes its guard when the page is removed', async () => {
    const el = await mountEditor();
    el.isDirty = true;
    el.remove();

    expect(mayLeave()).toBe(true);
    expect(confirmSpy).not.toHaveBeenCalled();
  });
});
