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
import { buildAgentDMKey, dmPeerFromKey } from './chat-routes.js';

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
