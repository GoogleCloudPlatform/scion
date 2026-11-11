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
 * The boot preload may fetch the /chat chunk before the feature flags are
 * known (ptone/scion#5307), but a disabled native chat still never renders.
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt, json } from './__fixtures__/main-boot.js';

const record = vi.hoisted(() => ({ chatRendered: false }));

vi.mock('../components/pages/home.js', () => {
  if (!customElements.get('scion-page-home')) {
    customElements.define('scion-page-home', class extends HTMLElement {});
  }
  return {};
});
vi.mock('../components/pages/chat.js', () => {
  if (!customElements.get('scion-page-chat')) {
    customElements.define(
      'scion-page-chat',
      class extends HTMLElement {
        connectedCallback(): void {
          record.chatRendered = true;
        }
      }
    );
  }
  return {};
});
vi.mock('../components/chat/chat-shell.js', () => {
  if (!customElements.get('scion-chat-shell')) {
    customElements.define(
      'scion-chat-shell',
      class extends HTMLElement {
        connectedCallback(): void {
          record.chatRendered = true;
        }
      }
    );
  }
  return {};
});

describe('main.ts chat flag gate', () => {
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('chat route still blocked when native_chat disabled', async () => {
    await bootMainAt('/chat', (p) =>
      p === '/api/v1/settings/public'
        ? Promise.resolve(json({ nativeChatEnabled: false }))
        : undefined
    );

    await vi.waitFor(
      () => expect(document.querySelector('scion-page-home')).not.toBeNull(),
      BOOT_WAIT
    );
    expect(window.location.pathname).toBe('/');
    expect(document.querySelector('scion-page-chat')).toBeNull();
    expect(document.querySelector('scion-chat-shell')).toBeNull();
    expect(record.chatRendered).toBe(false);
  }, 15_000);
});
