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
 * The group member editor announces successful membership edits with the
 * window-level membership-changed event, and stays silent on failure.
 */

// @vitest-environment happy-dom

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { addMember, removeMember, listMembers, GroupsApiError } from '../../client/groups-api.js';
import { showConfirm } from './confirm-dialog.js';
import { MEMBERSHIP_CHANGED_EVENT } from '../../utils/membership-events.js';
import { ScionGroupMemberEditor } from './group-member-editor.js';
import type { GroupMember } from '../../shared/types.js';

vi.mock('../../client/groups-api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/groups-api.js')>();
  return {
    ...actual,
    addMember: vi.fn(),
    removeMember: vi.fn(),
    listMembers: vi.fn(),
  };
});
vi.mock('./confirm-dialog.js', () => ({ showConfirm: vi.fn() }));

interface EditorInternals {
  addMemberType: string;
  addMemberInput: string;
  addMemberRole: string;
  handleAddMember(e: Event): Promise<void>;
  handleRemoveMember(m: GroupMember): Promise<void>;
}

const BOB: GroupMember = {
  groupId: 'g-1',
  memberType: 'user',
  memberId: 'u-bob',
  displayName: 'Bob',
  role: 'member',
  addedAt: '2026-01-01T00:00:00Z',
};

function editor(): EditorInternals {
  const el = new ScionGroupMemberEditor();
  el.groupId = 'g-1';
  return el as unknown as EditorInternals;
}

let heard: ReturnType<typeof vi.fn<(detail: unknown) => void>>;
const onChanged = (e: Event): void => {
  heard((e as CustomEvent).detail);
};

beforeEach(() => {
  heard = vi.fn<(detail: unknown) => void>();
  window.addEventListener(MEMBERSHIP_CHANGED_EVENT, onChanged);
  vi.mocked(listMembers).mockResolvedValue({ members: [] } as unknown as Awaited<
    ReturnType<typeof listMembers>
  >);
  vi.mocked(showConfirm).mockResolvedValue(true);
});

afterEach(() => {
  window.removeEventListener(MEMBERSHIP_CHANGED_EVENT, onChanged);
  vi.clearAllMocks();
});

describe('group member editor membership-changed event', () => {
  it('is dispatched once a member is added', async () => {
    vi.mocked(addMember).mockResolvedValue(
      undefined as unknown as Awaited<ReturnType<typeof addMember>>
    );
    const el = editor();
    el.addMemberType = 'user';
    el.addMemberInput = 'u-new';
    el.addMemberRole = 'member';

    await el.handleAddMember(new Event('submit'));

    expect(addMember).toHaveBeenCalledTimes(1);
    expect(heard).toHaveBeenCalledWith({ kind: 'group', id: 'g-1' });
  });

  it('is not dispatched when adding a member fails', async () => {
    vi.mocked(addMember).mockRejectedValue(new GroupsApiError('quota', 'limit reached', 400));
    const el = editor();
    el.addMemberType = 'user';
    el.addMemberInput = 'u-new';
    el.addMemberRole = 'member';

    await el.handleAddMember(new Event('submit'));

    expect(heard).not.toHaveBeenCalled();
  });

  it('is dispatched once a member is removed', async () => {
    vi.mocked(removeMember).mockResolvedValue({ outcome: 'ok' } as Awaited<
      ReturnType<typeof removeMember>
    >);

    await editor().handleRemoveMember(BOB);

    expect(removeMember).toHaveBeenCalledWith('g-1', 'user', 'u-bob');
    expect(heard).toHaveBeenCalledWith({ kind: 'group', id: 'g-1' });
  });

  it('is not dispatched when a removal is refused', async () => {
    vi.mocked(removeMember).mockResolvedValue({
      outcome: 'lockout',
      detail: 'last owner',
      rawBody: {},
    } as Awaited<ReturnType<typeof removeMember>>);

    await editor().handleRemoveMember(BOB);

    expect(heard).not.toHaveBeenCalled();
  });
});
