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

import { afterEach, describe, expect, it } from 'vitest';
import { deepActiveElement } from './deep-active-element.js';

afterEach(() => {
  document.body.replaceChildren();
});

describe('deepActiveElement', () => {
  it('returns the focused light-DOM element', () => {
    const input = document.createElement('input');
    document.body.append(input);
    input.focus();
    expect(deepActiveElement()).toBe(input);
  });

  it('descends through nested open shadow roots', () => {
    const outer = document.createElement('div');
    const outerRoot = outer.attachShadow({ mode: 'open' });
    const inner = document.createElement('div');
    outerRoot.append(inner);
    const innerRoot = inner.attachShadow({ mode: 'open' });
    const button = document.createElement('button');
    innerRoot.append(button);
    document.body.append(outer);

    button.focus();

    expect(document.activeElement).toBe(outer);
    expect(deepActiveElement()).toBe(button);
  });

  it('returns the body when nothing is focused', () => {
    expect(deepActiveElement()).toBe(document.body);
  });
});
