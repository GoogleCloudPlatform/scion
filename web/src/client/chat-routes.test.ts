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

import { describe, it, expect } from 'vitest';
import {
  buildAgentDMKey,
  conversationChatPath,
  dmPeerFromKey,
  isChatConversationPath,
  messageFilterFromSearch,
  parseThreadConversationExternalRef,
  searchWithMessageFilter,
} from './chat-routes.js';

describe('dmPeerFromKey', () => {
  it('names the agent of an agent DM, whoever is asking', () => {
    const key = buildAgentDMKey('agent-1', 'user-me')!;
    expect(dmPeerFromKey(key, 'user-me')).toEqual({ peerId: 'agent-1', peerKind: 'agent' });
    expect(dmPeerFromKey(key, '')).toEqual({ peerId: 'agent-1', peerKind: 'agent' });
  });

  it('names the other user of a user DM, on either side of the key', () => {
    expect(dmPeerFromKey('dm:user:user-a:user:user-me', 'user-me')).toEqual({
      peerId: 'user-a',
      peerKind: 'user',
    });
    expect(dmPeerFromKey('dm:user:user-me:user:user-z', 'user-me')).toEqual({
      peerId: 'user-z',
      peerKind: 'user',
    });
  });

  it('names the user themselves in a DM with themselves', () => {
    expect(dmPeerFromKey('dm:user:user-me:user:user-me', 'user-me')).toEqual({
      peerId: 'user-me',
      peerKind: 'user',
    });
  });

  it('cannot tell a user DM peer without the current user, or when they are not in it', () => {
    expect(dmPeerFromKey('dm:user:user-a:user:user-b', '')).toBeNull();
    expect(dmPeerFromKey('dm:user:user-a:user:user-b', 'user-me')).toBeNull();
  });

  it('rejects what is not a two-party DM key', () => {
    for (const key of [
      '',
      'topic-1',
      'agent-1',
      'dm:agent:agent-1',
      'dm:agent::user:user-me',
      'dm:bot:b1:user:user-me',
      'dm:agent:a:user:u:extra',
      'xx:agent:a:user:u',
    ]) {
      expect(dmPeerFromKey(key, 'user-me'), key).toBeNull();
    }
  });
});

describe('center message filter', () => {
  it('reads only the literal starred value', () => {
    expect(messageFilterFromSearch('?messages=starred')).toBe('starred');
    expect(messageFilterFromSearch('?a=1&messages=starred')).toBe('starred');
    expect(messageFilterFromSearch('')).toBe('');
    expect(messageFilterFromSearch('?messages=all')).toBe('');
    expect(messageFilterFromSearch('?messages=STARRED')).toBe('');
  });

  it('applies to conversation routes only', () => {
    expect(isChatConversationPath('/chat/space/p1/thread/t1')).toBe(true);
    expect(isChatConversationPath('/chat/my-project/t1')).toBe(true);
    expect(isChatConversationPath('/chat/dm/dm%3Aagent%3Aa%3Auser%3Ab')).toBe(true);
    expect(isChatConversationPath('/chat')).toBe(false);
    expect(isChatConversationPath('/chat/my-project')).toBe(false);
    expect(isChatConversationPath('/chat/space/p1')).toBe(false);
    expect(isChatConversationPath('/agents/x')).toBe(false);
  });

  it('sets and removes the filter, keeping other parameters', () => {
    expect(searchWithMessageFilter('', 'starred')).toBe('?messages=starred');
    expect(searchWithMessageFilter('?messages=starred', '')).toBe('');
    expect(searchWithMessageFilter('?a=1&messages=starred', '')).toBe('?a=1');
    expect(searchWithMessageFilter('?a=1', 'starred')).toBe('?a=1&messages=starred');
  });
});

describe('parseThreadConversationExternalRef', () => {
  it('extracts projectId and topicId from thread:<projectId>:<topicId>', () => {
    expect(parseThreadConversationExternalRef('thread:proj-1:topic-42')).toEqual({
      projectId: 'proj-1',
      topicId: 'topic-42',
    });
  });

  it('rejects non-thread refs, empty parts, and dm-prefixed topic IDs', () => {
    for (const ref of [
      undefined,
      '',
      'dm:agent:a1:user:u1',
      'thread:',
      'thread:proj-1',
      'thread::topic-1',
      'thread:proj-1:',
      'thread:proj-1:dm:agent:a1:user:u1',
    ]) {
      expect(parseThreadConversationExternalRef(ref), String(ref)).toBeNull();
    }
  });
});

describe('conversationChatPath', () => {
  it('builds a DM route for native direct conversations', () => {
    expect(
      conversationChatPath({
        kind: 'direct',
        surface: 'native',
        externalRef: 'dm:agent:a-1:user:u-1',
      })
    ).toBe('/chat/dm/dm%3Aagent%3Aa-1%3Auser%3Au-1');
  });

  it('builds a space thread route for native group conversations with thread externalRef', () => {
    expect(
      conversationChatPath({
        kind: 'group',
        surface: 'native',
        projectId: 'proj-1',
        externalRef: 'thread:proj-1:topic-99',
      })
    ).toBe('/chat/space/proj-1/thread/topic-99');
  });

  it('returns null for external-surface or unlinked group conversations', () => {
    expect(
      conversationChatPath({
        kind: 'group',
        surface: 'discord',
        projectId: 'proj-1',
        externalRef: 'thread:proj-1:topic-99',
      })
    ).toBeNull();
    expect(
      conversationChatPath({
        kind: 'group',
        surface: 'native',
        projectId: 'proj-1',
        externalRef: '',
      })
    ).toBeNull();
    expect(
      conversationChatPath({
        kind: 'direct',
        surface: 'native',
        externalRef: 'not-a-dm-key',
      })
    ).toBeNull();
  });
});
