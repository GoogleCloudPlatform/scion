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

import { describe, expect, it } from 'vitest';
import {
  badgeCountFromLists,
  dmPeerDisplayName,
  isBadgeUnreadDM,
  railUnreadDMs,
  type ChatDMListEntry,
} from './chat-unread-dms.js';

const dm = (over: Partial<ChatDMListEntry>): ChatDMListEntry => ({
  conversationKey: 'dm:key',
  peerId: 'peer',
  peerKind: 'agent',
  peerName: 'peer-name',
  hasUnread: true,
  muted: false,
  ...over,
});

describe('railUnreadDMs', () => {
  it('lists exactly the DMs the badge counts', () => {
    const dms = [
      dm({ conversationKey: 'a', peerKind: 'user', peerName: 'Alice' }),
      dm({ conversationKey: 'b', peerName: 'other-project-agent' }),
      dm({ conversationKey: 'c', peerName: '' }),
      dm({ conversationKey: 'd', muted: true }),
      dm({ conversationKey: 'e', hasUnread: false }),
      dm({ conversationKey: 'f', peerName: 'deleted-agent', peerDeleted: true }),
    ];
    const rows = railUnreadDMs(dms);
    expect(rows.map((r) => r.conversationKey)).toEqual(['a', 'b', 'c']);
    expect(rows.length).toBe(dms.filter(isBadgeUnreadDM).length);
    expect(rows.every((r) => r.unread)).toBe(true);
    expect(rows[0]).toMatchObject({ peerKind: 'user', displayName: 'Alice' });
  });

  it('keeps the open DM once read, without counting it', () => {
    const dms = [dm({ conversationKey: 'open', hasUnread: false }), dm({ conversationKey: 'x' })];
    const rows = railUnreadDMs(dms, 'open');
    expect(rows.map((r) => [r.conversationKey, r.unread])).toEqual([
      ['open', false],
      ['x', true],
    ]);
    expect(rows.filter((r) => r.unread).length).toBe(1);
  });

  it('does not keep a muted open DM as unread', () => {
    const rows = railUnreadDMs([dm({ conversationKey: 'm', muted: true })], 'm');
    expect(rows).toEqual([expect.objectContaining({ conversationKey: 'm', unread: false })]);
  });
});

describe('dmPeerDisplayName', () => {
  it('falls back from name to slug to a placeholder', () => {
    expect(dmPeerDisplayName(dm({ peerName: 'N', peerSlug: 's' }))).toBe('N');
    expect(dmPeerDisplayName(dm({ peerName: '', peerSlug: 's' }))).toBe('s');
    expect(dmPeerDisplayName(dm({ peerName: '', peerSlug: '' }))).toBe('Unknown agent');
    expect(dmPeerDisplayName({ conversationKey: 'k', peerId: 'p', peerKind: 'user' })).toBe(
      'Unknown user'
    );
  });
});

describe('badgeCountFromLists', () => {
  it('sums the space rollups and the badge-counted DMs', () => {
    expect(
      badgeCountFromLists(
        { spaces: [{ unreadCount: 2 }, { unreadCount: 0 }, { unreadCount: 1 }] },
        {
          dms: [
            dm({}),
            dm({ muted: true }),
            dm({ hasUnread: false }),
            dm({}),
            dm({ peerDeleted: true }),
          ],
        }
      )
    ).toBe(5);
  });

  it('ignores malformed entries', () => {
    expect(
      badgeCountFromLists(
        { spaces: [{ unreadCount: 'x' }, null, { unreadCount: -3 }, {}] },
        { dms: [null] }
      )
    ).toBe(0);
  });

  it('cannot answer without both lists', () => {
    expect(badgeCountFromLists(null, { dms: [] })).toBeNull();
    expect(badgeCountFromLists({ spaces: [] }, null)).toBeNull();
  });
});

describe('deleted-agent DMs', () => {
  it('are left out of the badge and the list, but the open one stays shown', () => {
    const gone = dm({ conversationKey: 'gone', peerDeleted: true });
    expect(isBadgeUnreadDM(gone)).toBe(false);
    expect(railUnreadDMs([gone])).toEqual([]);
    expect(railUnreadDMs([gone], 'gone')).toEqual([
      expect.objectContaining({ conversationKey: 'gone', unread: false }),
    ]);
    expect(badgeCountFromLists({ spaces: [] }, { dms: [gone] })).toBe(0);
  });
});
