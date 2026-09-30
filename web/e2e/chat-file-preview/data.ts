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
 * Pure fixture data shared by fixture.ts (loaded in the real browser via
 * Vite) and mock-api.ts (loaded by Playwright's Node test runner). Kept
 * import-free of anything DOM/CSS/Playwright-specific so either side can
 * import it without pulling in the other's runtime.
 */
import type { Message } from '../../src/shared/types.js';

export const SELF_USER_ID = 'self-user';
export const PEER_USER_ID = 'peer-user';
export const CONVERSATION_KEY = `dm:user:${SELF_USER_ID}:user:${PEER_USER_ID}`;
export const PROJECT_A = 'proj-alpha';
export const PROJECT_B = 'proj-beta';

export const IMAGE_ATTACHMENT_ID = 'att-image-1';
export const TEXT_ATTACHMENT_ID = 'att-text-1';
export const BINARY_ATTACHMENT_ID = 'att-bin-1';

/** A message referencing an in-prose container path, in project A. */
export const PATH_MESSAGE_A: Message = {
  id: 'msg-path-a',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'see /workspace/notes.md for the plan',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:01Z',
  senderProjectId: PROJECT_A,
};

/** The *same* file name, in a different project — proves no cross-project reuse. */
export const PATH_MESSAGE_B: Message = {
  id: 'msg-path-b',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'different notes: /workspace/notes.md',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:02Z',
  senderProjectId: PROJECT_B,
};

/** A message referencing an image path, in project A. */
export const PATH_IMAGE_MESSAGE: Message = {
  id: 'msg-path-image',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'screenshot: /workspace/diagram.png',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:04Z',
  senderProjectId: PROJECT_A,
};

/** A message referencing a path whose real content exceeds the 512 KiB inline-preview limit. */
export const PATH_OVERSIZE_MESSAGE: Message = {
  id: 'msg-path-oversize',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'full log: /workspace/huge.log',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:05Z',
  senderProjectId: PROJECT_A,
};

export const ATTACHMENT_MESSAGE: Message = {
  id: 'msg-attachments',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'here are the files',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:03Z',
  senderProjectId: PROJECT_A,
};

export const TEXT_ATTACHMENT_BODY = '# Plan\n\nShip the extracted preview.\n';
export const ALPHA_NOTES_CONTENT = 'Alpha project notes content.';
export const BETA_NOTES_CONTENT = 'Beta project notes content — a different file entirely.';
