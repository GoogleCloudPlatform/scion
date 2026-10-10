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

/**
 * Inline image attachments.
 *
 * The images load lazily and the server sends no dimensions, so each
 * thumbnail reserves its box before it loads. A thread with images still
 * opens at its newest message, and a thumbnail never runs past its bubble.
 */

import { test, expect, type Page } from '@playwright/test';
import { deflateSync } from 'node:zlib';
import { openChatRail, openGeneralThread } from './fixture.js';
import { GENERAL_THREAD_ID, PROJECT_A, buildMessages } from './mock-api.js';

/** The fixture image: wider than the thumbnail box, so it scales down. */
const IMAGE = { width: 1200, height: 500 };
/** How long the attachment takes to arrive, so it lands after the thread has rendered. */
const IMAGE_DELAY_MS = 700;

const CRC_TABLE = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});

function crc32(buf: Buffer): number {
  let c = 0xffffffff;
  for (const byte of buf) c = CRC_TABLE[(c ^ byte) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}

/** A flat-colour RGB PNG of the given size. */
function solidPng(width: number, height: number): Buffer {
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  header[8] = 8; // bit depth
  header[9] = 2; // RGB
  const row = Buffer.alloc(1 + width * 3, 0x60);
  row[0] = 0; // no filter
  const raw = Buffer.concat(Array.from({ length: height }, () => row));
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', header),
    pngChunk('IDAT', deflateSync(raw)),
    pngChunk('IEND', Buffer.alloc(0)),
  ]);
}

const PNG = solidPng(IMAGE.width, IMAGE.height);

/**
 * Give the general thread's last two messages an image each, served late.
 * With `hold`, the attachment requests wait until that promise settles.
 */
async function withImages(page: Page, hold?: Promise<void>): Promise<void> {
  await page.route(
    new RegExp(`/api/v1/chat/conversations/${GENERAL_THREAD_ID}/messages`),
    (route) => {
      if (route.request().method() !== 'GET') {
        void route.fallback();
        return;
      }
      const messages = buildMessages(GENERAL_THREAD_ID, PROJECT_A.id);
      const ids = messages.slice(-2).map((m) => String(m.id));
      const messageAttachments = Object.fromEntries(
        ids.map((id, i) => [
          id,
          [{ id: `img-${i}`, name: `diagram-${i}.png`, mime: 'image/png', size: PNG.length }],
        ])
      );
      void route.fulfill({ json: { items: messages, messages, messageAttachments } });
    }
  );
  await page.route(/\/api\/v1\/chat\/attachments\//, async (route) => {
    await (hold ?? new Promise((resolve) => setTimeout(resolve, IMAGE_DELAY_MS)));
    await route.fulfill({ body: PNG, contentType: 'image/png' });
  });
}

/** Every inline thumbnail in the thread, searched through the shadow roots. */
async function thumbnails(page: Page) {
  return page.evaluate(() => {
    const all: Element[] = [];
    const walk = (root: ParentNode) => {
      for (const el of root.querySelectorAll('*')) {
        if (el.matches('img.attachment-image')) all.push(el);
        if (el.shadowRoot) walk(el.shadowRoot);
      }
    };
    walk(document);
    return all.map((el) => {
      const img = el as HTMLImageElement;
      const box = img.getBoundingClientRect();
      const host = (img.getRootNode() as ShadowRoot).host;
      const bubble = host.shadowRoot!.querySelector('.bubble-content, .bubble');
      const bubbleBox = bubble ? bubble.getBoundingClientRect() : box;
      return {
        loaded: img.complete && img.naturalWidth > 0,
        left: box.left,
        right: box.right,
        width: box.width,
        height: box.height,
        bubbleLeft: bubbleBox.left,
        bubbleRight: bubbleBox.right,
        innerWidth: window.innerWidth,
      };
    });
  });
}

/**
 * Bring each thumbnail that has not loaded into view, as a reader scrolling
 * up to it would, and wait until all have loaded. A lazy image clipped by
 * the message scroller is not fetched until it comes near the view.
 */
async function loadAll(page: Page): Promise<void> {
  await expect
    .poll(
      async () => {
        await page.evaluate(() => {
          const walk = (root: ParentNode) => {
            for (const el of root.querySelectorAll('*')) {
              if (el.matches('img.attachment-image') && !(el as HTMLImageElement).complete) {
                el.scrollIntoView({ block: 'nearest' });
              }
              if (el.shadowRoot) walk(el.shadowRoot);
            }
          };
          walk(document);
        });
        return (await thumbnails(page)).filter((t) => t.loaded).length;
      },
      { timeout: 10_000 }
    )
    .toBe(2);
}

/**
 * Each message's top and bottom within the message list, in px:
 * independent of where the list is scrolled to.
 */
async function listLayout(page: Page): Promise<Array<[number, number]>> {
  return page.evaluate(() => {
    const thread = document
      .querySelector('scion-page-chat')!
      .shadowRoot!.querySelector('scion-chat-thread')!;
    const scroller = thread.shadowRoot!.querySelector('.messages-scroll')!;
    const origin = scroller.getBoundingClientRect().top - scroller.scrollTop;
    return [...thread.shadowRoot!.querySelectorAll('scion-chat-message')].map((m) => {
      const box = m.getBoundingClientRect();
      return [Math.round(box.top - origin), Math.round(box.bottom - origin)] as [number, number];
    });
  });
}

/** How far the message list sits above its bottom, in px. */
async function distanceFromBottom(page: Page): Promise<number> {
  return page.evaluate(() => {
    const thread = document
      .querySelector('scion-page-chat')!
      .shadowRoot!.querySelector('scion-chat-thread')!;
    const el = thread.shadowRoot!.querySelector('.messages-scroll')!;
    return el.scrollHeight - el.scrollTop - el.clientHeight;
  });
}

test.describe('inline image attachments', () => {
  test('a thread with images opens at its newest message', async ({ page }) => {
    await openChatRail(page, withImages);
    await openGeneralThread(page);
    // The newest image is in view; an older one off screen may wait for
    // the reader to scroll to it, as lazy images do.
    await expect
      .poll(async () => (await thumbnails(page)).at(-1)?.loaded, { timeout: 10_000 })
      .toBe(true);
    // Let the load's layout and any scroll correction settle.
    await page.waitForTimeout(300);
    expect(await distanceFromBottom(page), 'still at the bottom').toBeLessThanOrEqual(2);
  });

  test('before the image arrives, its box is reserved at full size', async ({ page }) => {
    let release = (): void => {};
    const hold = new Promise<void>((resolve) => (release = resolve));
    try {
      await openChatRail(page, (p) => withImages(p, hold));
      await openGeneralThread(page);
      await expect.poll(async () => (await thumbnails(page)).length, { timeout: 10_000 }).toBe(2);
      for (const thumb of await thumbnails(page)) {
        expect(thumb.loaded, 'the request is still held back').toBe(false);
        const bubbleWidth = thumb.bubbleRight - thumb.bubbleLeft;
        // The full 320px box, or the bubble's width where that is narrower.
        expect(thumb.width, 'a full-size box').toBeGreaterThanOrEqual(
          Math.min(320, bubbleWidth) - 1
        );
        expect(thumb.width, 'no wider than the box').toBeLessThanOrEqual(320.5);
        expect(thumb.right, 'inside the bubble').toBeLessThanOrEqual(thumb.bubbleRight + 0.5);
        expect(thumb.right, 'inside the screen').toBeLessThanOrEqual(thumb.innerWidth);
        expect(Math.abs(thumb.height - (thumb.width * 3) / 4), 'a 4:3 box').toBeLessThan(1);
      }
    } finally {
      release();
    }
  });

  test('an image of another shape loads without moving what is below it', async ({ page }) => {
    let release = (): void => {};
    const hold = new Promise<void>((resolve) => (release = resolve));
    try {
      await openChatRail(page, (p) => withImages(p, hold));
      await openGeneralThread(page);
      await expect.poll(async () => (await thumbnails(page)).length, { timeout: 10_000 }).toBe(2);
      // Let the reserved layout settle before taking the reference.
      await page.waitForTimeout(300);
      const before = await listLayout(page);
      release();
      await loadAll(page);
      await page.waitForTimeout(300);
      expect(await listLayout(page), 'every message where it was').toEqual(before);
    } finally {
      release();
    }
  });

  test('the thumbnail stays inside its bubble', async ({ page }) => {
    await openChatRail(page, withImages);
    await openGeneralThread(page);
    await loadAll(page);
    for (const thumb of await thumbnails(page)) {
      expect(thumb.right, 'inside the bubble').toBeLessThanOrEqual(thumb.bubbleRight + 0.5);
      expect(thumb.right, 'inside the screen').toBeLessThanOrEqual(thumb.innerWidth);
    }
    // The image shows inside its reserved box, which keeps its shape.
    for (const thumb of await thumbnails(page)) {
      expect(Math.abs(thumb.height - (thumb.width * 3) / 4), 'still the 4:3 box').toBeLessThan(1);
    }
  });
});
