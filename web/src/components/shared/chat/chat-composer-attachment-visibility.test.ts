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
 * Tests for the composer attachment visibility line under pending attachment
 * chips (ptone/scion#4598, #4599): wording per conversation kind (project
 * thread, DM, group without a project).
 */

import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import type { ScionChatComposer, UploadedAttachment } from './chat-composer.js';

const PENDING_FILE: UploadedAttachment = {
  id: 'att-1',
  name: 'report.txt',
  mime: 'text/plain',
  size: 128,
  url: '/api/v1/chat/attachments/att-1',
};

type ComposerWithPending = ScionChatComposer & { pendingFiles: UploadedAttachment[] };

async function mountComposer(opts: {
  conversationMode: 'thread' | 'dm' | '';
  conversationKey?: string;
  projectId?: string;
  projectName?: string;
  peerName?: string;
  pendingFiles?: UploadedAttachment[];
}): Promise<ComposerWithPending> {
  const el = document.createElement('scion-chat-composer') as ComposerWithPending;
  el.conversationMode = opts.conversationMode;
  el.conversationKey = opts.conversationKey ?? '';
  el.projectId = opts.projectId ?? '';
  el.projectName = opts.projectName ?? '';
  el.peerName = opts.peerName ?? '';
  document.body.appendChild(el);
  await el.updateComplete;
  if (opts.pendingFiles) {
    el.pendingFiles = opts.pendingFiles;
    await el.updateComplete;
  }
  return el;
}

describe('scion-chat-composer attachment visibility line (#4598, #4599)', () => {
  beforeAll(async () => {
    await import('./chat-composer.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    localStorage.clear();
  });

  it('does not render when there are no pending attachment chips', async () => {
    const el = await mountComposer({
      conversationMode: 'thread',
      projectId: 'p-1',
      projectName: 'Project X',
    });
    expect(el.shadowRoot?.querySelector('.attachment-visibility')).toBeNull();
  });

  it('shows project wording for a project thread', async () => {
    const el = await mountComposer({
      conversationMode: 'thread',
      conversationKey: 'topic-1',
      projectId: 'p-1',
      projectName: 'Project X',
      pendingFiles: [PENDING_FILE],
    });
    expect(el.shadowRoot?.querySelector('.attachment-visibility')?.textContent?.trim()).toBe(
      'Visible to everyone in Project X'
    );
  });

  it('shows DM wording for a direct message', async () => {
    const el = await mountComposer({
      conversationMode: 'dm',
      conversationKey: 'dm:user:u1:user:u2',
      peerName: 'Alice',
      pendingFiles: [PENDING_FILE],
    });
    expect(el.shadowRoot?.querySelector('.attachment-visibility')?.textContent?.trim()).toBe(
      'Visible to you and Alice'
    );
  });

  it('shows conversation members wording for a group without a project', async () => {
    const el = await mountComposer({
      conversationMode: 'thread',
      conversationKey: 'group-1',
      projectId: '',
      projectName: '',
      pendingFiles: [PENDING_FILE],
    });
    expect(el.shadowRoot?.querySelector('.attachment-visibility')?.textContent?.trim()).toBe(
      'Visible to members of this conversation'
    );
  });
});
