// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import type { Page } from '@playwright/test';
import {
  ALPHA_NOTES_CONTENT,
  ATTACHMENT_MESSAGE,
  BETA_NOTES_CONTENT,
  BINARY_ATTACHMENT_ID,
  IMAGE_ATTACHMENT_ID,
  PATH_IMAGE_MESSAGE,
  PATH_MESSAGE_A,
  PATH_MESSAGE_B,
  PATH_OVERSIZE_MESSAGE,
  PROJECT_A,
  PROJECT_B,
  TEXT_ATTACHMENT_BODY,
  TEXT_ATTACHMENT_ID,
} from './data.js';

export { ALPHA_NOTES_CONTENT, BETA_NOTES_CONTENT, TEXT_ATTACHMENT_BODY };

/** A minimal but valid 1x1 red PNG, so the browser actually decodes an <img>. */
const PNG_1X1_RED_BASE64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=';

/** Reported size only — the extracted viewer never reads the body once size exceeds the limit. */
export const OVERSIZE_BYTES = 600 * 1024;

export interface TrackedRequest {
  method: string;
  url: string;
}

/** Same technique as e2e/chat-palette/mock-api.ts: replace the real app bootstrap module at the network layer. */
async function stubMainClientModule(page: Page): Promise<void> {
  await page.route('**/src/client/main.ts', (route) =>
    route.fulfill({
      contentType: 'text/javascript',
      body: `
        class FixtureStateManager extends EventTarget {
          currentScope = null;
          isConnected() { return false; }
          setScope() {}
          setCurrentUserId() {}
          hydrate() {}
          getAgent() { return undefined; }
          getAgents() { return new Map(); }
          getDeletedAgentIds() { return new Set(); }
          removeAgent() {}
          seedAgents() {}
        }
        export const stateManager = new FixtureStateManager();
        export function navigateTo(path) {
          const url = new URL(path, location.origin);
          history.pushState({}, '', url.pathname + url.search + url.hash);
          window.dispatchEvent(new PopStateEvent('popstate'));
        }
      `,
    })
  );
}

export async function setupApiMocks(page: Page): Promise<TrackedRequest[]> {
  const requests: TrackedRequest[] = [];
  await stubMainClientModule(page);

  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();
    requests.push({ method, url: request.url() });

    // The conversation key contains colons, which the client percent-encodes
    // into the URL (`url.pathname` preserves that encoding rather than
    // decoding it) — match by suffix/method instead of the exact decoded key,
    // since this fixture only ever mounts one conversation.
    if (path.endsWith('/messages') && method === 'GET') {
      return route.fulfill({
        json: {
          items: [
            PATH_MESSAGE_A,
            PATH_MESSAGE_B,
            PATH_IMAGE_MESSAGE,
            PATH_OVERSIZE_MESSAGE,
            ATTACHMENT_MESSAGE,
          ],
          messageAttachments: {
            [ATTACHMENT_MESSAGE.id]: [
              { id: IMAGE_ATTACHMENT_ID, name: 'shot.png', mime: 'image/png', size: 95 },
              {
                id: TEXT_ATTACHMENT_ID,
                name: 'plan.md',
                mime: 'text/markdown',
                size: TEXT_ATTACHMENT_BODY.length,
              },
              {
                id: BINARY_ATTACHMENT_ID,
                name: 'archive.zip',
                mime: 'application/zip',
                size: 2048,
              },
            ],
          },
        },
      });
    }

    if (
      path === `/api/v1/projects/${PROJECT_A}/workspace/files/notes.md` &&
      url.searchParams.get('format') === 'json'
    ) {
      return route.fulfill({
        json: { content: ALPHA_NOTES_CONTENT, size: ALPHA_NOTES_CONTENT.length },
      });
    }
    if (
      path === `/api/v1/projects/${PROJECT_B}/workspace/files/notes.md` &&
      url.searchParams.get('format') === 'json'
    ) {
      return route.fulfill({
        json: { content: BETA_NOTES_CONTENT, size: BETA_NOTES_CONTENT.length },
      });
    }
    if (
      path === `/api/v1/projects/${PROJECT_A}/workspace/files/diagram.png` &&
      url.searchParams.get('view') === 'true'
    ) {
      return route.fulfill({
        contentType: 'image/png',
        body: Buffer.from(PNG_1X1_RED_BASE64, 'base64'),
      });
    }
    if (
      path === `/api/v1/projects/${PROJECT_A}/workspace/files/huge.log` &&
      url.searchParams.get('format') === 'json'
    ) {
      return route.fulfill({ json: { content: '', size: OVERSIZE_BYTES } });
    }

    if (
      path === `/api/v1/chat/attachments/${IMAGE_ATTACHMENT_ID}` &&
      url.searchParams.get('view') === 'true'
    ) {
      return route.fulfill({
        contentType: 'image/png',
        body: Buffer.from(PNG_1X1_RED_BASE64, 'base64'),
      });
    }
    if (path === `/api/v1/chat/attachments/${TEXT_ATTACHMENT_ID}`) {
      return route.fulfill({ contentType: 'text/markdown', body: TEXT_ATTACHMENT_BODY });
    }
    if (path === '/api/v1/chat/attachments/att-missing') {
      return route.fulfill({ status: 404, json: { error: 'attachment not found' } });
    }

    if (path === '/api/v1/auth/me') {
      return route.fulfill({
        json: { id: 'self-user', email: 'self@example.com', displayName: 'Self User' },
      });
    }
    if (path.endsWith('/read') || path.endsWith('/typing') || path === '/api/v1/chat/presence') {
      return route.fulfill({ json: {} });
    }
    // Unnamed endpoint: empty object keeps the real component's defensive
    // parsing harmless without asserting it is called.
    return route.fulfill({ json: {} });
  });

  return requests;
}
