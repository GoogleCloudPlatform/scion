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
 * Real-Chromium verification for ptone/scion#6676: chat surfaces load only
 * hub-served (same-origin / data:) images by default and never request
 * external image hosts across:
 *   1. Chat message bodies (<scion-chat-message>)
 *   2. Inter-agent message popover (<scion-chat-interagent-marker>)
 *   3. Markdown attachment inline preview (<scion-chat-message> -> <scion-markdown-preview>)
 *   4. Markdown file/attachment full preview (<scion-chat-file-preview> -> <scion-markdown-preview>)
 */

import { test, expect } from '@playwright/test';

const EXTERNAL_HOST = 'elsewhere.example';
const TINY_PNG_DATA =
  'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';

test('chat surfaces block external image requests and keep hub-served images', async ({ page }) => {
  const externalRequests: string[] = [];

  await page.route(`**://${EXTERNAL_HOST}/**`, (route) => {
    externalRequests.push(route.request().url());
    return route.abort();
  });

  // Register catch-all first (Playwright evaluates page.route in LIFO order).
  await page.route('**/api/**', (route) => route.abort());

  // Serve a 1x1 PNG for same-origin test images and mock markdown attachment/file endpoints.
  const pngBytes = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==',
    'base64'
  );
  await page.route('**/api/v1/chat/attachments/hub-img', (route) =>
    route.fulfill({ status: 200, contentType: 'image/png', body: pngBytes })
  );
  await page.route('**/api/v1/chat/attachments/md-att**', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'text/markdown',
      body: `![ext-att](https://${EXTERNAL_HOST}/att.png) ![hub-att](/api/v1/chat/attachments/hub-img)`,
    })
  );
  await page.route('**/api/v1/chat/conversations/**/interagent**', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        messages: [
          {
            id: 'ia-1',
            projectId: 'proj-1',
            sender: 'agent:alpha',
            senderId: 'alpha-id',
            recipient: 'agent:beta',
            recipientId: 'beta-id',
            msg: `![ext-ia](https://${EXTERNAL_HOST}/ia.png) ![hub-ia](/api/v1/chat/attachments/hub-img)`,
            msgTruncated: false,
            type: 'agent-message',
            agentId: 'alpha-id',
            createdAt: new Date().toISOString(),
          },
        ],
      }),
    })
  );

  await page.goto('/e2e/chat-file-preview/fixture.html', { waitUntil: 'domcontentloaded' });

  // 1. Message body + inline markdown attachment preview
  const messageResult = await page.evaluate(
    async ({ host, dataUri }) => {
      await customElements.whenDefined('scion-chat-message');
      const el = document.createElement('scion-chat-message') as HTMLElement & {
        body: string;
        attachmentRefs: Array<{ id: string; name: string; mime: string; size: number }>;
        updateComplete: Promise<boolean>;
      };
      el.body = `![ext-body](https://${host}/body.png) ![hub-body](/api/v1/chat/attachments/hub-img) ![data-body](${dataUri})`;
      el.attachmentRefs = [{ id: 'md-att', name: 'notes.md', mime: 'text/markdown', size: 80 }];
      document.body.appendChild(el);

      const waitFor = async (check: () => boolean): Promise<void> => {
        const deadline = Date.now() + 5000;
        while (Date.now() < deadline) {
          if (check()) return;
          await new Promise((r) => setTimeout(r, 25));
        }
        throw new Error('Timed out waiting for chat message render');
      };

      await waitFor(() => !!el.shadowRoot?.querySelector('.md-content'));
      await waitFor(() => {
        const preview = el.shadowRoot?.querySelector('scion-markdown-preview');
        return !!preview?.shadowRoot?.querySelector('.preview-container p');
      });

      const bodyContent = el.shadowRoot!.querySelector('.md-content')!;
      const bodyImgSrcs = Array.from(bodyContent.querySelectorAll('img')).map((img) =>
        img.getAttribute('src')
      );

      const inlinePreview = el.shadowRoot!.querySelector('scion-markdown-preview')!;
      const inlineContainer = inlinePreview.shadowRoot!.querySelector('.preview-container')!;
      const inlineImgSrcs = Array.from(inlineContainer.querySelectorAll('img')).map((img) =>
        img.getAttribute('src')
      );

      return {
        bodyImgSrcs,
        bodyText: bodyContent.textContent ?? '',
        inlineHasAttr: inlinePreview.hasAttribute('same-origin-images'),
        inlineImgSrcs,
        inlineText: inlineContainer.textContent ?? '',
      };
    },
    { host: EXTERNAL_HOST, dataUri: TINY_PNG_DATA }
  );

  expect(messageResult.bodyImgSrcs).toEqual(['/api/v1/chat/attachments/hub-img', TINY_PNG_DATA]);
  expect(messageResult.bodyText).toContain('ext-body');
  expect(messageResult.inlineHasAttr).toBe(true);
  expect(messageResult.inlineImgSrcs).toEqual(['/api/v1/chat/attachments/hub-img']);
  expect(messageResult.inlineText).toContain('ext-att');

  // 2. Inter-agent message popover
  const interagentResult = await page.evaluate(async () => {
    await customElements.whenDefined('scion-chat-interagent-marker');
    const el = document.createElement('scion-chat-interagent-marker') as HTMLElement & {
      messageCount: number;
      expanded: boolean;
      messages: unknown[];
      updateComplete: Promise<boolean>;
    };
    el.setAttribute('conversation-key', 'dm:agent:alpha-id:user:user-1');
    el.messageCount = 1;
    el.expanded = true;
    el.messages = [
      {
        id: 'ia-1',
        projectId: 'proj-1',
        sender: 'agent:alpha',
        senderId: 'alpha-id',
        recipient: 'agent:beta',
        recipientId: 'beta-id',
        msg: 'Preview...',
        msgTruncated: true,
        type: 'agent-message',
        agentId: 'alpha-id',
        createdAt: new Date().toISOString(),
      },
    ];
    document.body.appendChild(el);
    await el.updateComplete;

    const btn = el.shadowRoot?.querySelector('.ia-expand sl-icon-button') as HTMLElement | null;
    if (!btn) throw new Error('Expand button not found');
    btn.click();

    const deadline = Date.now() + 5000;
    while (Date.now() < deadline) {
      if (el.shadowRoot?.querySelector('.ia-full-body')) break;
      await new Promise((r) => setTimeout(r, 25));
    }
    const body = el.shadowRoot!.querySelector('.ia-full-body')!;
    return {
      imgSrcs: Array.from(body.querySelectorAll('img')).map((img) => img.getAttribute('src')),
      text: body.textContent ?? '',
    };
  });

  expect(interagentResult.imgSrcs).toEqual(['/api/v1/chat/attachments/hub-img']);
  expect(interagentResult.text).toContain('ext-ia');

  // 3. Full markdown file/attachment preview (<scion-chat-file-preview>)
  const filePreviewResult = await page.evaluate(async () => {
    await customElements.whenDefined('scion-chat-file-preview');
    const el = document.createElement('scion-chat-file-preview') as HTMLElement & {
      target: unknown;
      updateComplete: Promise<boolean>;
    };
    el.target = {
      kind: 'attachment',
      id: 'md-att',
      name: 'notes.md',
      mime: 'text/markdown',
      size: 80,
    };
    document.body.appendChild(el);

    const deadline = Date.now() + 5000;
    while (Date.now() < deadline) {
      const preview = el.shadowRoot?.querySelector('scion-markdown-preview');
      if (preview?.shadowRoot?.querySelector('.preview-container p')) break;
      await new Promise((r) => setTimeout(r, 25));
    }

    const preview = el.shadowRoot!.querySelector('scion-markdown-preview')!;
    const container = preview.shadowRoot!.querySelector('.preview-container')!;
    return {
      hasAttr: preview.hasAttribute('same-origin-images'),
      imgSrcs: Array.from(container.querySelectorAll('img')).map((img) => img.getAttribute('src')),
      text: container.textContent ?? '',
    };
  });

  expect(filePreviewResult.hasAttr).toBe(true);
  expect(filePreviewResult.imgSrcs).toEqual(['/api/v1/chat/attachments/hub-img']);
  expect(filePreviewResult.text).toContain('ext-att');

  // Confirm the browser never initiated any request to the external host.
  expect(externalRequests).toEqual([]);
});
