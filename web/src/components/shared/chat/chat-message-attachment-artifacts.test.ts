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
 * Tests for the attachment chip menu on <scion-chat-message> and attachment
 * footer actions on <scion-chat-file-preview> (ptone/scion#4598, #4599):
 * experiment gating (hub.attachment_artifacts + hub.artifacts), publish rights,
 * Copy reference (scion://attachment/<id>), and Save as artifact… dispatch.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { ARTIFACTS_FLAG, ATTACHMENT_ARTIFACTS_FLAG } from '../../../client/artifacts.js';
import { resetServerFlagStateForTests, setFeatureFlag } from '../../../utils/feature-flags.js';
import type { AttachmentRefInfo, ScionChatMessage } from './chat-message.js';
import type { ScionChatFilePreview } from './chat-file-preview.js';

vi.mock('../../../utils/markdown.js', () => ({
  getMarkdownRenderer: () =>
    Promise.resolve({
      render: (md: string) => `<p>${md}</p>`,
      renderFragment: (md: string) => {
        const tpl = document.createElement('template');
        tpl.innerHTML = `<p>${md}</p>`;
        return tpl.content;
      },
    }),
}));

vi.mock('../code-editor.js', () => ({
  getLanguageFromPath: () => 'plaintext',
}));

const copyMock = vi.fn(() => Promise.resolve(true));
vi.mock('../../../utils/clipboard.js', () => ({
  copyToClipboard: (text: string) => copyMock(text),
  showCopyFailedToast: vi.fn(),
}));

const toastMock = vi.fn();
vi.mock('../../../utils/toast.js', () => ({
  showToast: (...args: unknown[]) => toastMock(...args),
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('hello', { status: 200 }))),
  extractApiError: () => Promise.resolve('error'),
}));

const FILE: AttachmentRefInfo = {
  id: '5f1c2d3e-0000-4000-8000-0000000000aa',
  name: 'archive.bin',
  mime: 'application/octet-stream',
  size: 2048,
};

async function mountMessage(
  opts: {
    canPublishArtifacts?: boolean;
    attachmentRefs?: AttachmentRefInfo[];
  } = {}
): Promise<ScionChatMessage> {
  const el = document.createElement('scion-chat-message') as ScionChatMessage;
  el.body = 'See attached.';
  el.attachmentRefs = opts.attachmentRefs ?? [FILE];
  if (opts.canPublishArtifacts !== undefined) {
    el.canPublishArtifacts = opts.canPublishArtifacts;
  }
  document.body.appendChild(el);
  await el.updateComplete;
  await Promise.resolve();
  await el.updateComplete;
  return el;
}

describe('scion-chat-message attachment chip menu (#4598, #4599)', () => {
  beforeAll(async () => {
    await import('./chat-message.js');
  });

  beforeEach(() => {
    resetServerFlagStateForTests();
    localStorage.clear();
    copyMock.mockClear();
    toastMock.mockClear();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    resetServerFlagStateForTests();
    localStorage.clear();
  });

  it('hides the attachment menu when hub.attachment_artifacts is off', async () => {
    setFeatureFlag(ARTIFACTS_FLAG, true);
    const el = await mountMessage();
    expect(el.shadowRoot?.querySelector('.attachment-menu')).toBeNull();
  });

  it('shows Copy reference and Download, but hides Save as artifact… when hub.artifacts is off', async () => {
    setFeatureFlag(ATTACHMENT_ARTIFACTS_FLAG, true);
    setFeatureFlag(ARTIFACTS_FLAG, false);
    const el = await mountMessage();
    const menu = el.shadowRoot?.querySelector('.attachment-menu');
    expect(menu).not.toBeNull();
    expect(menu?.querySelector('.attachment-save-artifact-item')).toBeNull();
    expect(menu?.querySelector('.attachment-copy-ref-item')).not.toBeNull();
    expect(menu?.querySelector('.attachment-download-item')).not.toBeNull();
  });

  it('shows Save as artifact… enabled when both experiments are on and caller has publish rights', async () => {
    setFeatureFlag(ATTACHMENT_ARTIFACTS_FLAG, true);
    setFeatureFlag(ARTIFACTS_FLAG, true);
    const el = await mountMessage({ canPublishArtifacts: true });
    const item = el.shadowRoot?.querySelector('.attachment-save-artifact-item');
    expect(item).not.toBeNull();
    expect(item?.hasAttribute('disabled')).toBe(false);

    const events: AttachmentRefInfo[] = [];
    el.addEventListener('save-attachment-as-artifact', (e: Event) => {
      events.push((e as CustomEvent<{ attachment: AttachmentRefInfo }>).detail.attachment);
    });

    const slMenu = el.shadowRoot?.querySelector('.attachment-menu sl-menu');
    slMenu?.dispatchEvent(
      new CustomEvent('sl-select', { detail: { item: { value: 'save-as-artifact' } } })
    );
    expect(events).toEqual([FILE]);
  });

  it('disables Save as artifact… and ignores selection when caller lacks publish rights', async () => {
    setFeatureFlag(ATTACHMENT_ARTIFACTS_FLAG, true);
    setFeatureFlag(ARTIFACTS_FLAG, true);
    const el = await mountMessage({ canPublishArtifacts: false });
    const item = el.shadowRoot?.querySelector('.attachment-save-artifact-item');
    expect(item).not.toBeNull();
    expect(item?.hasAttribute('disabled')).toBe(true);

    const events: unknown[] = [];
    el.addEventListener('save-attachment-as-artifact', (e: Event) => events.push(e));

    const slMenu = el.shadowRoot?.querySelector('.attachment-menu sl-menu');
    slMenu?.dispatchEvent(
      new CustomEvent('sl-select', { detail: { item: { value: 'save-as-artifact' } } })
    );
    expect(events).toHaveLength(0);
  });

  it('copies scion://attachment/<id> when Copy reference is chosen', async () => {
    setFeatureFlag(ATTACHMENT_ARTIFACTS_FLAG, true);
    const el = await mountMessage();
    const slMenu = el.shadowRoot?.querySelector('.attachment-menu sl-menu');
    slMenu?.dispatchEvent(
      new CustomEvent('sl-select', { detail: { item: { value: 'copy-ref' } } })
    );
    await vi.waitFor(() => {
      expect(copyMock).toHaveBeenCalledWith(`scion://attachment/${FILE.id}`);
    });
  });
});

describe('scion-chat-file-preview attachment actions (#4598, #4599)', () => {
  beforeAll(async () => {
    await import('./chat-file-preview.js');
  });

  beforeEach(() => {
    resetServerFlagStateForTests();
    localStorage.clear();
    copyMock.mockClear();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    resetServerFlagStateForTests();
    localStorage.clear();
  });

  it('gates Save as artifact… on both flags and publish rights in preview footer', async () => {
    setFeatureFlag(ATTACHMENT_ARTIFACTS_FLAG, true);
    setFeatureFlag(ARTIFACTS_FLAG, false);

    const el = document.createElement('scion-chat-file-preview') as ScionChatFilePreview;
    el.target = { kind: 'attachment', ...FILE };
    el.canPublishArtifacts = false;
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.copy-attachment-ref-btn')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.save-as-artifact-btn')).toBeNull();

    setFeatureFlag(ARTIFACTS_FLAG, true);
    el.requestUpdate();
    await el.updateComplete;

    const btn = el.shadowRoot?.querySelector('.save-as-artifact-btn');
    expect(btn).not.toBeNull();
    expect(btn?.hasAttribute('disabled')).toBe(true);

    el.canPublishArtifacts = true;
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.save-as-artifact-btn')?.hasAttribute('disabled')).toBe(
      false
    );
  });
});
