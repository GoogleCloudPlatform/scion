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
  GCS_MESSAGES,
  GCS_BUCKET,
  GCS_XPROJECT_BUCKET,
  GCS_XPROJECT_OBJECT,
  GCS_MARKDOWN_BODY,
  GCS_JSON_BODY,
  GCS_SVG_BODY,
  GCS_HTML_BODY,
  GCS_TOO_LARGE_INLINE_BYTES,
} from './data.js';

export { ALPHA_NOTES_CONTENT, BETA_NOTES_CONTENT, TEXT_ATTACHMENT_BODY };

/** A minimal but valid 1x1 red PNG, so the browser actually decodes an <img>. */
const PNG_1X1_RED_BASE64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=';

/** A minimal but valid 1x1 JPEG, so the browser actually decodes an <img>. */
const JPEG_1X1_BASE64 =
  '/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBAQEBAgGBgUGCQgKCgkICQkKDA8MCgsOCwkJDRENDg8QEBEQCgwSExIQEw8QEBD/2wBDAQMDAwQDBAgEBAgQCwkLEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBD/wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAj/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/8QAFQEBAQAAAAAAAAAAAAAAAAAAAAX/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIRAxEAPwCdABmX/9k=';

/** A minimal but valid 1x1 transparent GIF, so the browser actually decodes an <img>. */
const GIF_1X1_BASE64 = 'R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==';

/** A minimal but valid 1x1 lossless WebP, so the browser actually decodes an <img>. */
const WEBP_1X1_BASE64 = 'UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==';

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
        export function replaceRoute(path) {
          history.replaceState(history.state, '', path + location.search + location.hash);
          return Promise.resolve();
        }
        export function pushRoute(path) {
          history.pushState({}, '', path);
          return Promise.resolve();
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

/**
 * Self-contained mocks for the gs:// link spec: its own message list
 * (GCS_MESSAGES), the gcs object fetch endpoint, /api/v1/settings/public,
 * and /api/v1/experiments. Deliberately independent of {@link setupApiMocks}
 * (a different message count/shape) so this suite's fixtures never perturb
 * the extraction suite's `toHaveCount(5)` assertions.
 *
 * @param opts.gcsLinksExperimentEnabled - the `web.gcs_links` value served
 * from `/api/v1/experiments` (ptone/scion#2545). Defaults to `true`, since
 * most specs in this file exercise the linkifier; pass `false` for a case
 * that drives the real experiments-fetch path instead of an
 * `__SCION_FEATURES__` init-script pin.
 */
export async function setupGcsApiMocks(
  page: Page,
  opts: { gcsLinksExperimentEnabled?: boolean } = {}
): Promise<TrackedRequest[]> {
  const { gcsLinksExperimentEnabled = true } = opts;
  const requests: TrackedRequest[] = [];
  await stubMainClientModule(page);

  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();
    requests.push({ method, url: request.url() });

    if (path.endsWith('/messages') && method === 'GET') {
      return route.fulfill({ json: { items: GCS_MESSAGES, messageAttachments: {} } });
    }

    if (path === '/api/v1/gcs/object' && method === 'GET') {
      const bucket = url.searchParams.get('bucket');
      const object = url.searchParams.get('object');

      if (bucket === GCS_BUCKET && object === 'missing.txt') {
        return route.fulfill({
          status: 404,
          contentType: 'application/json',
          body: '{"error":{"code":"not_found","message":"Object not found"}}',
        });
      }
      if (bucket === GCS_BUCKET && object === 'denied.txt') {
        return route.fulfill({
          status: 403,
          contentType: 'application/json',
          body: '{"error":{"code":"not_found","message":"Object not found"}}',
        });
      }
      if (bucket === GCS_BUCKET && object === 'report.pdf') {
        return route.fulfill({
          status: 404,
          contentType: 'application/json',
          body: '{"error":{"code":"not_found","message":"Object not found"}}',
        });
      }
      if (bucket === GCS_XPROJECT_BUCKET && object === GCS_XPROJECT_OBJECT) {
        return route.fulfill({
          status: 200,
          contentType: 'text/plain; charset=utf-8',
          headers: { 'Content-Disposition': `attachment; filename*=UTF-8''dev-brief.md` },
          body: GCS_MARKDOWN_BODY,
        });
      }
      if (bucket === GCS_BUCKET && object === 'data.json') {
        return route.fulfill({
          status: 200,
          contentType: 'text/plain; charset=utf-8',
          body: GCS_JSON_BODY,
        });
      }

      // Image sniffing, SVG-as-source, octet-stream, the
      // Content-Length preview-size abort, 413 and an HTML-bodied object.
      // Content-Type here stands in for the hub's own http.DetectContentType
      // sniff result (covered directly against the real sniffer in
      // pkg/hub/gcs_link_test.go) — this mock only needs to prove the
      // client reacts correctly to whatever Content-Type the hub decided.
      if (bucket === GCS_BUCKET && object === 'photo.png') {
        return route.fulfill({
          status: 200,
          contentType: 'image/png',
          body: Buffer.from(PNG_1X1_RED_BASE64, 'base64'),
        });
      }
      if (bucket === GCS_BUCKET && object === 'photo.jpg') {
        return route.fulfill({
          status: 200,
          contentType: 'image/jpeg',
          body: Buffer.from(JPEG_1X1_BASE64, 'base64'),
        });
      }
      if (bucket === GCS_BUCKET && object === 'photo.gif') {
        return route.fulfill({
          status: 200,
          contentType: 'image/gif',
          body: Buffer.from(GIF_1X1_BASE64, 'base64'),
        });
      }
      if (bucket === GCS_BUCKET && object === 'photo.webp') {
        return route.fulfill({
          status: 200,
          contentType: 'image/webp',
          body: Buffer.from(WEBP_1X1_BASE64, 'base64'),
        });
      }
      if (bucket === GCS_BUCKET && object === 'diagram.svg') {
        // The hub never serves image/svg+xml: an SVG object's real sniffed
        // type is text/plain, same as any other text body.
        return route.fulfill({
          status: 200,
          contentType: 'text/plain; charset=utf-8',
          body: GCS_SVG_BODY,
        });
      }
      if (bucket === GCS_BUCKET && object === 'fake.png') {
        // A .png name whose actual bytes are HTML: the hub's sniff ignores
        // the extension and metadata alike, so this is text/plain, never
        // image/png and never text/html.
        return route.fulfill({
          status: 200,
          contentType: 'text/plain; charset=utf-8',
          headers: { 'Content-Disposition': `attachment; filename*=UTF-8''fake.png` },
          body: GCS_HTML_BODY,
        });
      }
      if (bucket === GCS_BUCKET && object === 'archive.blob') {
        return route.fulfill({
          status: 200,
          contentType: 'application/octet-stream',
          body: Buffer.from([0x00, 0x01, 0x02, 0x03]),
        });
      }
      if (bucket === GCS_BUCKET && object === 'huge.txt') {
        // A real body whose UTF-8 byte length (the Content-Length) is over
        // GCS_TOO_LARGE_INLINE_BYTES but whose character count is under it, so
        // only the Content-Length check — not the post-read length cap — can
        // classify it as too large.
        return route.fulfill({
          status: 200,
          contentType: 'text/plain; charset=utf-8',
          body: 'é'.repeat(Math.ceil(GCS_TOO_LARGE_INLINE_BYTES / 2)),
        });
      }
      if (bucket === GCS_BUCKET && object === 'giant.bin') {
        return route.fulfill({
          status: 413,
          contentType: 'application/json',
          body: JSON.stringify({
            error: {
              code: 'too_large',
              details: { size: 12 * 1024 * 1024, limit: 10 * 1024 * 1024 },
            },
          }),
        });
      }
      // Every other in-fixture object (dir/file.md, start.txt, after.txt,
      // workspace/collision.md, the hostile-name objects, o/r) resolves to
      // generic text content — only the cases above need distinct bodies
      // for their assertions.
      return route.fulfill({
        status: 200,
        contentType: 'text/plain; charset=utf-8',
        body: `content of gs://${bucket}/${object}`,
      });
    }

    if (path === '/api/v1/settings/public') {
      return route.fulfill({ json: { nativeChatEnabled: true } });
    }

    if (path === '/api/v1/experiments') {
      return route.fulfill({
        json: { experiments: { 'web.gcs_links': gcsLinksExperimentEnabled } },
      });
    }

    if (path === '/api/v1/auth/me') {
      return route.fulfill({
        json: { id: 'self-user', email: 'self@example.com', displayName: 'Self User' },
      });
    }
    if (path.endsWith('/read') || path.endsWith('/typing') || path === '/api/v1/chat/presence') {
      return route.fulfill({ json: {} });
    }
    return route.fulfill({ json: {} });
  });

  return requests;
}
