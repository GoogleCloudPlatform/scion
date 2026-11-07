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

import { render } from 'lit';
import { describe, expect, it } from 'vitest';

import { renderReleaseNotesLink } from './release-notes-link.js';

// ptone/scion#6134: a release URL comes from the release manifest or the
// GitHub catalog; only an https URL becomes a link, anything else is shown
// as plain text. (Moved from the server-config page tests when that page's
// update banner was removed, hub-update-channels Phase 6.)
describe('renderReleaseNotesLink', () => {
  it.each([
    ['https', 'https://github.com/GoogleCloudPlatform/scion/releases/tag/v1.1.0', true],
    ['javascript:', 'javascript:alert(document.cookie)', false],
    ['data:', 'data:text/html,<script>alert(1)</script>', false],
    ['http:', 'http://example.com/releases/1.1.0', false],
    ['relative', '/releases/1.1.0', false],
    ['malformed', 'https://', false],
  ])('renders a %s URL safely', (_name, url, linked) => {
    const host = document.createElement('div');
    render(renderReleaseNotesLink(url), host);

    const link = host.querySelector<HTMLAnchorElement>('a.release-notes-link');
    const text = host.querySelector<HTMLElement>('.release-notes-text');
    if (linked) {
      expect(link?.getAttribute('href')).toBe(url);
      expect(link?.getAttribute('rel')).toBe('noopener noreferrer');
      expect(text).toBeNull();
    } else {
      expect(link).toBeNull();
      expect(text?.textContent).toBe(url);
      // No anchor anywhere carries the unsafe value.
      expect(host.querySelectorAll('a').length).toBe(0);
    }
  });
});
