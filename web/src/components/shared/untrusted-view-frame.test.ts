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
 * The shared untrusted view frame (ptone/scion#4580): the one definition
 * the artifact page and the chat preview both render.
 */

import { describe, it, expect, afterEach } from 'vitest';
import { render } from 'lit';

import { UNTRUSTED_VIEW_SANDBOX, renderUntrustedViewFrame } from './untrusted-view-frame.js';

const URL_ = 'https://view.example.test/v/tok/index.html';

afterEach(() => {
  document.body.innerHTML = '';
});

describe('renderUntrustedViewFrame', () => {
  it('renders the view in a sandboxed frame without allow-same-origin, under the untrusted bar', () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    render(renderUntrustedViewFrame({ url: URL_, title: 'Site', owner: 'agent-1' }), host);

    expect(UNTRUSTED_VIEW_SANDBOX).toBe('allow-scripts');
    const frames = host.querySelectorAll('.untrusted iframe');
    expect(frames).toHaveLength(1);
    const frame = frames[0];
    expect(frame.getAttribute('sandbox')).toBe('allow-scripts');
    expect(frame.getAttribute('sandbox')).not.toContain('allow-same-origin');
    expect(frame.getAttribute('referrerpolicy')).toBe('no-referrer');
    expect(frame.getAttribute('src')).toBe(URL_);
    expect(frame.hasAttribute('srcdoc')).toBe(false);
    expect(frame.getAttribute('title')).toBe('Site (sandboxed)');
    const bar = (host.querySelector('.untrusted-bar')?.textContent ?? '').replace(/\s+/g, ' ').trim();
    expect(bar).toBe('Content published by agent-1 · runs sandboxed, isolated from Scion');
    expect(host.querySelector('.untrusted-bar sl-icon')?.getAttribute('name')).toBe('shield-lock');
  });
});
