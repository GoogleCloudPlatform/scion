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

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  ChatRecentFilesStore,
  RECENT_FILES_CAP,
  type IngestMessageInput,
  type AttachmentRefLike,
} from './chat-recent-files.js';

const SCOPE = { origin: 'https://hub.example', baseUrl: '/', userId: 'user-1' };
const OTHER_USER = { origin: 'https://hub.example', baseUrl: '/', userId: 'user-2' };
const OTHER_BASE = { origin: 'https://hub.example', baseUrl: '/sub/', userId: 'user-1' };

function msg(overrides: Partial<IngestMessageInput> = {}): IngestMessageInput {
  return {
    id: 'm1',
    conversationKey: 'dm:agent:coder:user:u1',
    sentAt: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

function att(overrides: Partial<AttachmentRefLike> = {}): AttachmentRefLike {
  return { id: 'att-1', name: 'notes.txt', mime: 'text/plain', size: 100, ...overrides };
}

describe('ChatRecentFilesStore', () => {
  let store: ChatRecentFilesStore;

  beforeEach(() => {
    localStorage.clear();
    store = new ChatRecentFilesStore();
  });

  afterEach(() => {
    store.dispose();
    localStorage.clear();
  });

  // -- basic ingest / identity / dedupe --------------------------

  it('indexes an attachment by id', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    expect(store.snapshot().records).toHaveLength(1);
    expect(store.snapshot().records[0].target).toEqual({
      kind: 'attachment',
      id: 'att-1',
      mime: 'text/plain',
      size: 100,
    });
  });

  // The full unsafe-id vector table, applied at the ingest layer.
  describe('never indexes an attachment with an unsafe id', () => {
    const unsafeIds = [
      '.',
      '..',
      '%2e%2e',
      '%2E%2E',
      'a/b',
      'a?b',
      'a#b',
      'a%2fb',
      'a%5cb',
      'a\\b',
    ];

    it.each(unsafeIds)('id=%s', (badId) => {
      store.setScope(SCOPE);
      store.ingest(msg(), [att({ id: badId })], {});
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  it('indexes a detected container path when a project is resolved', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ text: 'see /workspace/a.md' }), [], { projectId: 'proj-1' });
    const records = store.snapshot().records;
    expect(records).toHaveLength(1);
    expect(records[0].target).toEqual({
      kind: 'path',
      projectId: 'proj-1',
      containerPath: '/workspace/a.md',
      location: { kind: 'workspace', filePath: 'a.md' },
    });
  });

  it('omits a detected path when no project can be resolved, but still indexes attachments', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ text: 'see /workspace/a.md' }), [att()], {});
    const records = store.snapshot().records;
    expect(records).toHaveLength(1);
    expect(records[0].target.kind).toBe('attachment');
  });

  it('indexes a historical (wave-1) attachment path the same way as a detected one', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ legacyAttachmentPaths: ['/workspace/legacy.md'] }), [], {
      projectId: 'proj-1',
    });
    expect(store.snapshot().records[0].target).toMatchObject({
      kind: 'path',
      containerPath: '/workspace/legacy.md',
    });
  });

  it('resolves a supported extensionless historical path', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ legacyAttachmentPaths: ['/workspace/Dockerfile'] }), [], {
      projectId: 'proj-1',
    });
    expect(store.snapshot().records).toHaveLength(1);
  });

  // A legacy (wave-1) attachment path is a raw string straight from the
  // message, not text scanned by the restrictive extractContainerPaths
  // pattern — it must go through the same validation independently.
  it('never indexes a legacy path containing a traversal segment', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ legacyAttachmentPaths: ['/workspace/../../agents.txt'] }), [], {
      projectId: 'proj-1',
    });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('never indexes a legacy path containing an encoded traversal segment', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ legacyAttachmentPaths: ['/workspace/%2e%2e/agents.txt'] }), [], {
      projectId: 'proj-1',
    });
    expect(store.snapshot().records).toHaveLength(0);
  });

  // The full vector table at the legacy-attachment-path
  // ingest layer. The ?/# vectors deliberately contain no `..`, so they can
  // only be caught by the `?`/`#` rule itself.
  describe('never indexes a legacy path from the full unsafe-path vector table', () => {
    const unsafeLegacyPaths: Array<[string, string]> = [
      ['a raw .. segment', '/workspace/../secret.txt'],
      ['a raw . segment', '/workspace/./secret.txt'],
      ['%2e%2e lowercase', '/workspace/%2e%2e/secret.txt'],
      ['%2E%2E uppercase', '/workspace/%2E%2E/secret.txt'],
      ['%2f encoded slash lowercase', '/workspace/foo%2fsecret.txt'],
      ['%2F encoded slash uppercase', '/workspace/foo%2Fsecret.txt'],
      ['%5c encoded backslash lowercase', '/workspace/foo%5csecret.txt'],
      ['%5C encoded backslash uppercase', '/workspace/foo%5Csecret.txt'],
      ['a literal backslash', '/workspace/foo\\secret.txt'],
      ['a ? query delimiter (no ..)', '/workspace/notes.txt?x'],
      ['a # fragment delimiter (no ..)', '/workspace/notes.txt#x'],
    ];

    it.each(unsafeLegacyPaths)('%s: %s', (_label, path) => {
      store.setScope(SCOPE);
      store.ingest(msg({ legacyAttachmentPaths: [path] }), [], { projectId: 'proj-1' });
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  it('never indexes a legacy path that is a bare directory reference', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ legacyAttachmentPaths: ['/workspace/src'] }), [], { projectId: 'proj-1' });
    expect(store.snapshot().records).toHaveLength(0);
  });

  // A resolved project ID is placed directly into the file
  // API route (see buildFileApiUrl) — a `.`/`..`/route-changing project ID
  // must never reach a candidate record, whether it comes from a detected
  // in-text path or a legacy attachment path.
  describe('never indexes a path record for an unsafe resolved project ID', () => {
    const unsafeProjectIds = [
      '.',
      '..',
      '%2e%2e',
      '%2E%2E',
      'a/b',
      'a?b',
      'a#b',
      'a%2fb',
      'a%5cb',
      'a\\b',
    ];

    it.each(unsafeProjectIds)('detected path, projectId=%s', (projectId) => {
      store.setScope(SCOPE);
      store.ingest(msg({ text: 'see /workspace/a.md' }), [], { projectId });
      expect(store.snapshot().records).toHaveLength(0);
    });

    it.each(unsafeProjectIds)('legacy attachment path, projectId=%s', (projectId) => {
      store.setScope(SCOPE);
      store.ingest(msg({ legacyAttachmentPaths: ['/workspace/legacy.md'] }), [], { projectId });
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  it('dedupes the same attachment id ingested twice', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    store.ingest(msg({ id: 'm2', sentAt: '2026-01-02T00:00:00Z' }), [att()], {});
    expect(store.snapshot().records).toHaveLength(1);
  });

  it('dedupes the same path in the same project, keeping the newest occurrence name', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ text: '/workspace/a.md' }), [], { projectId: 'proj-1' });
    store.ingest(msg({ id: 'm2', sentAt: '2026-01-02T00:00:00Z', text: '/workspace/a.md' }), [], {
      projectId: 'proj-1',
    });
    const records = store.snapshot().records;
    expect(records).toHaveLength(1);
    expect(records[0].source.messageId).toBe('m2');
  });

  it('treats the two equivalent shared-dir path spellings as the same file', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ text: '/scion-volumes/scratchpad/a.md' }), [], { projectId: 'proj-1' });
    store.ingest(
      msg({
        id: 'm2',
        sentAt: '2026-01-02T00:00:00Z',
        text: '/workspace/.scion-volumes/scratchpad/a.md',
      }),
      [],
      { projectId: 'proj-1' }
    );
    expect(store.snapshot().records).toHaveLength(1);
  });

  it('treats the same path in two different projects as different files', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ text: '/workspace/a.md' }), [], { projectId: 'proj-1' });
    store.ingest(msg({ id: 'm2', text: '/workspace/a.md' }), [], { projectId: 'proj-2' });
    expect(store.snapshot().records).toHaveLength(2);
  });

  it('treats an attachment and a path with the same visible name as distinct entries', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ text: '/workspace/notes.txt' }), [att({ id: 'att-1', name: 'notes.txt' })], {
      projectId: 'proj-1',
    });
    expect(store.snapshot().records).toHaveLength(2);
  });

  it('is idempotent across a duplicate POST + SSE + history ingest of the same message', () => {
    store.setScope(SCOPE);
    const m = msg();
    store.ingest(m, [att()], {});
    store.ingest(m, [att()], {});
    store.ingest(m, [att()], {});
    expect(store.snapshot().records).toHaveLength(1);
  });

  it('does not persist or notify again on a genuinely no-op ingest', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    const setItemSpy = vi.spyOn(localStorage, 'setItem');
    const notifySpy = vi.fn();
    store.subscribe(notifySpy);
    // An exact duplicate of the same identity, sentAt, messageId and
    // conversationKey changes nothing — applyOne returns false for it, so
    // there is nothing to persist or notify about.
    store.ingest(msg(), [att()], {});
    expect(setItemSpy).not.toHaveBeenCalled();
    expect(notifySpy).not.toHaveBeenCalled();
    setItemSpy.mockRestore();
  });

  it('an older history load does not evict a newer record', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'newer', sentAt: '2026-01-05T00:00:00Z' }), [att({ id: 'att-1' })], {});
    // A later scroll-back loads an older copy of the same attachment id.
    store.ingest(msg({ id: 'older', sentAt: '2026-01-01T00:00:00Z' }), [att({ id: 'att-1' })], {});
    expect(store.snapshot().records[0].source.messageId).toBe('newer');
  });

  it('treats a Go zero-time sentAt as unknown (0), tying with an unparseable sentAt rather than sorting by its literal (ancient) date value', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'a', sentAt: 'not-a-real-date' }), [att({ id: 'att-1' })], {});
    store.ingest(msg({ id: 'b', sentAt: '0001-01-01T00:00:00Z' }), [att({ id: 'att-1' })], {});
    // Both sentAt values should map to "unknown" (ms=0): an unparseable
    // string via the NaN-to-0 fallback, and the Go zero-time via its own
    // explicit check. The tie is then broken by messageId, and 'b' > 'a'
    // lexicographically, so 'b' should win — it must not be treated as
    // sorting *before* 'a' by virtue of literally parsing to the year 1.
    expect(store.snapshot().records[0].source.messageId).toBe('b');
  });

  it('treats an unparseable sentAt as unknown (0) rather than letting NaN silently lose every ordering comparison', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'a', sentAt: 'not-a-real-date' }), [att({ id: 'att-1' })], {});
    store.ingest(msg({ id: 'b', sentAt: '1990-01-01T00:00:00Z' }), [att({ id: 'att-1' })], {});
    // A real (if old) timestamp must be treated as newer than "unknown" (0).
    // If the NaN produced by parsing 'not-a-real-date' were compared
    // directly instead of being normalized to 0 first, every comparison
    // against it (in both directions) is false, so 'b' would wrongly fail to
    // replace 'a'.
    expect(store.snapshot().records[0].source.messageId).toBe('b');
  });

  it('breaks an exact sentAt tie by messageId, deterministically', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'a', sentAt: '2026-01-01T00:00:00Z' }), [att({ id: 'att-1' })], {});
    store.ingest(msg({ id: 'b', sentAt: '2026-01-01T00:00:00Z' }), [att({ id: 'att-1' })], {});
    // 'b' > 'a' lexicographically, so it wins the tie.
    expect(store.snapshot().records[0].source.messageId).toBe('b');
  });

  it('breaks a tied sentAt AND messageId by conversationKey, deterministically', () => {
    store.setScope(SCOPE);
    store.ingest(
      msg({ id: 'same-id', sentAt: '2026-01-01T00:00:00Z', conversationKey: 'conv-a' }),
      [att({ id: 'att-1' })],
      {}
    );
    store.ingest(
      msg({ id: 'same-id', sentAt: '2026-01-01T00:00:00Z', conversationKey: 'conv-b' }),
      [att({ id: 'att-1' })],
      {}
    );
    // 'conv-b' > 'conv-a' lexicographically.
    expect(store.snapshot().records[0].source.conversationKey).toBe('conv-b');
  });

  it('caps at 50 entries after ingesting 51 distinct attachments, keeping the newest 50', () => {
    store.setScope(SCOPE);
    for (let i = 0; i < 51; i++) {
      store.ingest(
        msg({ id: `m${i}`, sentAt: new Date(2026, 0, i + 1).toISOString() }),
        [att({ id: `att-${i}` })],
        {}
      );
    }
    const records = store.snapshot().records;
    expect(records).toHaveLength(RECENT_FILES_CAP);
    expect(records.some((r) => r.target.kind === 'attachment' && r.target.id === 'att-0')).toBe(
      false
    );
    expect(records.some((r) => r.target.kind === 'attachment' && r.target.id === 'att-50')).toBe(
      true
    );
  });

  it('breaks a cap-eviction tie by key when every record shares the same sentAt', () => {
    store.setScope(SCOPE);
    // Zero-padded ids so string comparison of the JSON-tuple key matches
    // numeric order: key = '["attachment","a00"]' < '["attachment","a01"]' < ...
    // Inserted in *descending* key order — the opposite of insertion order —
    // so a naive stable-order implementation would also pass without this:
    // it would evict "a00" (the last one inserted) instead of "a50" (the
    // largest key), which only the explicit tie-break gets right.
    for (let i = 50; i >= 0; i--) {
      const id = `a${String(i).padStart(2, '0')}`;
      store.ingest(msg({ id: `m${i}`, sentAt: '2026-01-01T00:00:00Z' }), [att({ id })], {});
    }
    const ids = store
      .snapshot()
      .records.map((r) => (r.target.kind === 'attachment' ? r.target.id : ''))
      .sort();
    expect(ids).toHaveLength(50);
    // The lexicographically largest key ("a50") is the one evicted.
    expect(ids).not.toContain('a50');
    expect(ids).toContain('a00');
    expect(ids).toContain('a49');
  });

  it('shares one cap across attachments and paths together', () => {
    store.setScope(SCOPE);
    for (let i = 0; i < 30; i++) {
      store.ingest(
        msg({ id: `a${i}`, sentAt: new Date(2026, 0, i + 1).toISOString() }),
        [att({ id: `att-${i}` })],
        {}
      );
    }
    for (let i = 0; i < 30; i++) {
      store.ingest(
        msg({
          id: `p${i}`,
          sentAt: new Date(2026, 1, i + 1).toISOString(),
          text: `/workspace/f${i}.md`,
        }),
        [],
        { projectId: 'proj-1' }
      );
    }
    expect(store.snapshot().records).toHaveLength(RECENT_FILES_CAP);
  });

  it('survives being reconstructed (simulating page/thread recreation) via localStorage', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});

    const restarted = new ChatRecentFilesStore();
    restarted.setScope(SCOPE);
    expect(restarted.snapshot().records).toHaveLength(1);
    restarted.dispose();
  });

  it('previewing/reopening a record never promotes it (ingest is the only write path)', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'old', sentAt: '2020-01-01T00:00:00Z' }), [att({ id: 'att-1' })], {});
    // Snapshot/subscribe are read-only; nothing here should touch source.sentAt.
    void store.snapshot();
    expect(store.snapshot().records[0].source.sentAt).toBe('2020-01-01T00:00:00Z');
  });

  // -- timestamp correction (provisional own-send) -------------------------

  it('records an own-send provisionally with the client send time', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'srv-1', sentAt: '2026-01-01T00:00:05Z' }), [att()], {
      provisional: true,
    });
    expect(store.snapshot().records[0].source.sentAt).toBe('2026-01-01T00:00:05Z');
  });

  it('a later authoritative copy corrects a provisional record even to an earlier timestamp', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'srv-1', sentAt: '2026-01-01T00:00:05Z' }), [att()], {
      provisional: true,
    });
    // The SSE echo/backfill delivers the *real* createdAt, which happens to
    // be earlier than the client's optimistic guess.
    store.ingest(msg({ id: 'srv-1', sentAt: '2026-01-01T00:00:01Z' }), [att()], {});
    expect(store.snapshot().records[0].source.sentAt).toBe('2026-01-01T00:00:01Z');
  });

  it('once corrected, the record is no longer provisional and behaves like any other', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'srv-1', sentAt: '2026-01-01T00:00:05Z' }), [att()], {
      provisional: true,
    });
    store.ingest(msg({ id: 'srv-1', sentAt: '2026-01-01T00:00:01Z' }), [att()], {});
    // A later, unrelated, older ingest of the same identity must NOT override
    // the (already-corrected) record just because it once was provisional.
    store.ingest(msg({ id: 'srv-0', sentAt: '2025-01-01T00:00:00Z' }), [att()], {});
    expect(store.snapshot().records[0].source.messageId).toBe('srv-1');
  });

  it('a different, older, unrelated message does not overwrite a still-pending provisional record', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'srv-1', sentAt: '2026-01-01T00:00:05Z' }), [att()], {
      provisional: true,
    });
    // A different message (not the pending correction's own id) referencing
    // the same file, older than the provisional guess: isCorrection must
    // require messageId equality, not just "some provisional entry exists"
    // for this identity, or this would wrongly overwrite srv-1's record.
    store.ingest(msg({ id: 'srv-unrelated', sentAt: '2020-01-01T00:00:00Z' }), [att()], {});
    const record = store.snapshot().records[0];
    expect(record.source.messageId).toBe('srv-1');
    expect(record.source.sentAt).toBe('2026-01-01T00:00:05Z');
  });

  it('a genuinely newer authoritative ingest of the same identity still wins over a stale provisional', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'srv-1', sentAt: '2026-01-01T00:00:05Z' }), [att()], {
      provisional: true,
    });
    // A brand-new send of the same file, unrelated to the pending correction.
    store.ingest(msg({ id: 'srv-2', sentAt: '2026-01-02T00:00:00Z' }), [att()], {});
    expect(store.snapshot().records[0].source.messageId).toBe('srv-2');
  });

  // -- scope / suspension / generation guard -------------------------------

  it('ingest is a no-op with no scope set', () => {
    store.ingest(msg(), [att()], {});
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('ingest is a no-op when the message id is empty', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: '' }), [att()], {});
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('ingest is a no-op when the conversationKey is empty', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ conversationKey: '' }), [att()], {});
    expect(store.snapshot().records).toHaveLength(0);
  });

  it("ingest rejects a candidate whose messageId exceeds sanitizeRecord's own bound", () => {
    // A record exceeding one of sanitizeRecord's bounds must never sit in
    // memory only to be silently dropped the next time it round-tripped
    // through storage.
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'm'.repeat(513) }), [att()], {});
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('ingest is a no-op once suspended by logout', () => {
    store.setScope(SCOPE);
    store.clearForLogout();
    store.ingest(msg(), [att()], {});
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('a stale scope generation is dropped, not applied', () => {
    store.setScope(SCOPE);
    const staleGen = store.scopeGeneration;
    store.setScope(OTHER_USER); // generation advances
    store.ingest(msg(), [att()], {}, { scopeGeneration: staleGen });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('the current scope generation is accepted', () => {
    store.setScope(SCOPE);
    const gen = store.scopeGeneration;
    store.ingest(msg(), [att()], {}, { scopeGeneration: gen });
    expect(store.snapshot().records).toHaveLength(1);
  });

  it('setScope(null) clears in-memory records without touching storage', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    store.setScope(null);
    expect(store.snapshot().records).toHaveLength(0);
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(1); // still on disk
  });

  // -- persistence: isolation, hydration, corruption --------------

  it('isolates records by user id under the same origin/base', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    store.setScope(OTHER_USER);
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('isolates records by base path under the same origin/user', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    store.setScope(OTHER_BASE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('hydrates a valid persisted envelope on setScope', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(
      key,
      JSON.stringify({
        version: 1,
        records: [
          {
            key: JSON.stringify(['attachment', 'att-x']),
            name: 'x.txt',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: { kind: 'attachment', id: 'att-x', mime: 'text/plain', size: 5 },
          },
        ],
      })
    );
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(1);
    expect(store.snapshot().persistent).toBe(true);
  });

  it('discards a corrupt (unparseable) JSON payload safely', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(key, '{not json');
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
    expect(store.snapshot().persistent).toBe(true); // read failure isn't a write failure
  });

  it('ignores an unsupported future version, even with an otherwise-valid record', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    // The record itself must be one sanitizeRecord would otherwise accept —
    // otherwise this test cannot tell "version rejected" from "record
    // rejected on its own merits" apart, and removing the version check
    // entirely would still pass.
    const validRecord = {
      key: JSON.stringify(['attachment', 'att-x']),
      name: 'x.txt',
      source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
      target: { kind: 'attachment', id: 'att-x', mime: 'text/plain', size: 5 },
    };
    localStorage.setItem(key, JSON.stringify({ version: 2, records: [validRecord] }));
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('discards the whole envelope when records is not an array, even with a plausible-looking value', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(key, JSON.stringify({ version: 1, records: { length: 0 } }));
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('discards an individual malformed record but keeps the rest', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(
      key,
      JSON.stringify({
        version: 1,
        records: [
          { key: 'bad', name: 'bad' }, // missing source/target
          {
            key: JSON.stringify(['attachment', 'att-ok']),
            name: 'ok.txt',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: { kind: 'attachment', id: 'att-ok', mime: 'text/plain', size: 5 },
          },
        ],
      })
    );
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(1);
    expect(store.snapshot().records[0].name).toBe('ok.txt');
  });

  it('discards a record with a non-finite attachment size', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(
      key,
      JSON.stringify({
        version: 1,
        records: [
          {
            key: JSON.stringify(['attachment', 'att-x']),
            name: 'x',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: {
              kind: 'attachment',
              id: 'att-x',
              mime: 'text/plain',
              size: Number.POSITIVE_INFINITY,
            },
          },
        ],
      })
    );
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('discards a record with a negative attachment size', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(
      key,
      JSON.stringify({
        version: 1,
        records: [
          {
            key: JSON.stringify(['attachment', 'att-x']),
            name: 'x',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: { kind: 'attachment', id: 'att-x', mime: 'text/plain', size: -1 },
          },
        ],
      })
    );
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  // -- sanitizeRecord bounds, one violation per test against an otherwise
  // fully valid record, so each check is independently discriminated rather
  // than several being exercised together, where an earlier-checked field
  // failing would mask a later one. ------------

  function validAttachmentEnvelope(): {
    key: string;
    name: string;
    source: {
      conversationKey: string;
      messageId: string;
      sentAt: string;
      projectId?: string;
      projectName?: string;
    };
    target: { kind: string; id: string; mime: unknown; size: unknown };
  } {
    return {
      key: JSON.stringify(['attachment', 'att-x']),
      name: 'x.txt',
      source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
      target: { kind: 'attachment', id: 'att-x', mime: 'text/plain', size: 5 },
    };
  }

  function validPathEnvelope(): {
    key: string;
    name: string;
    source: { conversationKey: string; messageId: string; sentAt: string };
    target: {
      kind: string;
      projectId: unknown;
      containerPath: unknown;
      location: Record<string, unknown>;
    };
  } {
    return {
      key: JSON.stringify(['path', 'proj-1', 'workspace', '', 'a.md']),
      name: 'a.md',
      source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
      target: {
        kind: 'path',
        projectId: 'proj-1',
        containerPath: '/workspace/a.md',
        location: { kind: 'workspace', filePath: 'a.md' },
      },
    };
  }

  function hydrateWith(record: unknown): void {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(key, JSON.stringify({ version: 1, records: [record] }));
    store.setScope(SCOPE);
  }

  it('sanity: the valid attachment envelope fixture is actually accepted', () => {
    hydrateWith(validAttachmentEnvelope());
    expect(store.snapshot().records).toHaveLength(1);
  });

  it('sanity: the valid path envelope fixture is actually accepted', () => {
    hydrateWith(validPathEnvelope());
    expect(store.snapshot().records).toHaveLength(1);
  });

  it('rejects a key longer than the bound', () => {
    hydrateWith({ ...validAttachmentEnvelope(), key: 'k'.repeat(4097) });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a name longer than the bound', () => {
    hydrateWith({ ...validAttachmentEnvelope(), name: 'n'.repeat(4097) });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a messageId longer than the bound', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, source: { ...envelope.source, messageId: 'm'.repeat(513) } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects an empty conversationKey', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, source: { ...envelope.source, conversationKey: '' } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects an empty sentAt', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, source: { ...envelope.source, sentAt: '' } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a source.projectId longer than the bound', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, source: { ...envelope.source, projectId: 'p'.repeat(513) } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a source.projectName longer than the bound', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, source: { ...envelope.source, projectName: 'n'.repeat(4097) } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a non-string mime', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, target: { ...envelope.target, mime: 123 } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a mime longer than the bound', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, target: { ...envelope.target, mime: 'm'.repeat(4097) } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects an attachment record whose key does not match its own id', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, key: JSON.stringify(['attachment', 'not-att-x']) });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a path record whose key does not match its own target', () => {
    const envelope = validPathEnvelope();
    hydrateWith({
      ...envelope,
      key: JSON.stringify(['path', 'proj-1', 'workspace', '', 'wrong.md']),
    });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a non-number size — the typeof check is redundant with Number.isFinite, which already rejects any non-number type, but both are kept as belt-and-suspenders', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, target: { ...envelope.target, size: '5' } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  // The `!Number.isFinite(target.size)` bound is unreachable through
  // any real caller: buildCandidates already normalizes a non-finite size to
  // 0 before a fresh candidate ever reaches sanitizeRecord, and Infinity/NaN
  // have no JSON representation, so a hydrate/storage-event payload can never
  // carry one either (JSON.stringify(Infinity) is `null`, which the typeof
  // check catches first). Mock JSON.parse to inject the value directly, as
  // the only way to exercise this defensive check at all.
  it('rejects a non-finite attachment size that bypasses the JSON boundary entirely', () => {
    const envelope = validAttachmentEnvelope();
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(key, 'placeholder — overridden by the JSON.parse mock below');
    const parseSpy = vi.spyOn(JSON, 'parse').mockReturnValueOnce({
      version: 1,
      records: [{ ...envelope, target: { ...envelope.target, size: Infinity } }],
    });
    store.setScope(SCOPE);
    parseSpy.mockRestore();
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects an attachment target.id longer than the bound, even with a matching key', () => {
    const badId = 'x'.repeat(513);
    hydrateWith({
      key: JSON.stringify(['attachment', badId]),
      name: 'x',
      source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
      target: { kind: 'attachment', id: badId, mime: 'text/plain', size: 5 },
    });
    expect(store.snapshot().records).toHaveLength(0);
  });

  // An attachment id is placed directly into the attachment
  // API route (see buildAttachmentApiUrl) — a `.`/`..`/route-changing id
  // must never hydrate, even with a matching recomputed key.
  describe('rejects an attachment target.id that would change the API route, even with a matching key', () => {
    const unsafeIds = [
      '.',
      '..',
      '%2e%2e',
      '%2E%2E',
      'a/b',
      'a?b',
      'a#b',
      'a%2fb',
      'a%5cb',
      'a\\b',
    ];

    it.each(unsafeIds)('id=%s', (badId) => {
      hydrateWith({
        key: JSON.stringify(['attachment', badId]),
        name: 'x',
        source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
        target: { kind: 'attachment', id: badId, mime: 'text/plain', size: 5 },
      });
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  it('rejects a path target.projectId longer than the bound, even with a matching key', () => {
    const badProjectId = 'p'.repeat(513);
    hydrateWith({
      key: JSON.stringify(['path', badProjectId, 'workspace', '', 'a.md']),
      name: 'a.md',
      source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
      target: {
        kind: 'path',
        projectId: badProjectId,
        containerPath: '/workspace/a.md',
        location: { kind: 'workspace', filePath: 'a.md' },
      },
    });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects a source.projectId containing a traversal segment', () => {
    const envelope = validAttachmentEnvelope();
    hydrateWith({ ...envelope, source: { ...envelope.source, projectId: '..' } });
    expect(store.snapshot().records).toHaveLength(0);
  });

  // The recomputed-key identity check (which rejects a
  // record whose stored key doesn't match its own target) must not be the
  // only thing standing between an unsafe projectId and a persisted record —
  // each fixture below recomputes `key` from the same bad projectId, so the
  // identity check alone would accept it; only a dedicated projectId check
  // can reject it.
  describe('rejects a path target.projectId that would change the API route, even with a matching key', () => {
    const unsafeProjectIds = [
      '.',
      '..',
      '%2e%2e',
      '%2E%2E',
      'a/b',
      'a?b',
      'a#b',
      'a%2fb',
      'a%5cb',
      'a\\b',
    ];

    it.each(unsafeProjectIds)('projectId=%s', (badProjectId) => {
      const envelope = validPathEnvelope();
      hydrateWith({
        ...envelope,
        key: JSON.stringify(['path', badProjectId, 'workspace', '', 'a.md']),
        target: { ...envelope.target, projectId: badProjectId },
      });
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  it('rejects a path target.containerPath longer than the bound', () => {
    const envelope = validPathEnvelope();
    hydrateWith({
      ...envelope,
      target: { ...envelope.target, containerPath: '/workspace/' + 'a'.repeat(4090) },
    });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('rejects an unknown location.kind, even with a matching key', () => {
    const envelope = validPathEnvelope();
    hydrateWith({
      ...envelope,
      key: JSON.stringify(['path', 'proj-1', 'bogus', '', 'a.md']),
      target: { ...envelope.target, location: { kind: 'bogus', filePath: 'a.md' } },
    });
    expect(store.snapshot().records).toHaveLength(0);
  });

  // The filePath length bound (`location.filePath.length > MAX_TEXT_LEN`)
  // cannot be isolated from the recomputed-key identity check through this
  // public API: the key embeds the literal filePath (pathIdentityKey), so any
  // envelope whose key matches an over-length filePath is itself over
  // MAX_TEXT_LEN and is rejected by the outer `c.key` bound first, before
  // target validation even runs. This is structural, not a live gap — the
  // key bound already provides the same protection transitively. Left as a
  // mismatched-key fixture, which still proves an over-length filePath is
  // never accepted.
  it('rejects a location.filePath longer than the bound, necessarily via the outer key-length bound rather than the filePath bound itself', () => {
    const envelope = validPathEnvelope();
    hydrateWith({
      ...envelope,
      target: { ...envelope.target, location: { kind: 'workspace', filePath: 'a'.repeat(4097) } },
    });
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('discards a shared-dir path record missing its dirName', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(
      key,
      JSON.stringify({
        version: 1,
        records: [
          {
            key: JSON.stringify(['path', 'proj-1', 'shared-dir', '', 'a.md']),
            name: 'a.md',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: {
              kind: 'path',
              projectId: 'proj-1',
              containerPath: '/scion-volumes/x/a.md',
              location: { kind: 'shared-dir', filePath: 'a.md' }, // dirName missing
            },
          },
        ],
      })
    );
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('discards a persisted path record whose filePath contains a traversal segment', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(
      key,
      JSON.stringify({
        version: 1,
        records: [
          {
            key: JSON.stringify(['path', 'proj-1', 'workspace', '', '../../agents']),
            name: 'agents',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: {
              kind: 'path',
              projectId: 'proj-1',
              containerPath: '/workspace/../../agents',
              location: { kind: 'workspace', filePath: '../../agents' },
            },
          },
        ],
      })
    );
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('discards a persisted path record whose filePath contains an encoded traversal segment', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(
      key,
      JSON.stringify({
        version: 1,
        records: [
          {
            key: JSON.stringify(['path', 'proj-1', 'workspace', '', '%2e%2e/agents']),
            name: 'agents',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: {
              kind: 'path',
              projectId: 'proj-1',
              containerPath: '/workspace/%2e%2e/agents',
              location: { kind: 'workspace', filePath: '%2e%2e/agents' },
            },
          },
        ],
      })
    );
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('discards a persisted shared-dir record whose dirName contains a traversal segment', () => {
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    localStorage.setItem(
      key,
      JSON.stringify({
        version: 1,
        records: [
          {
            key: JSON.stringify(['path', 'proj-1', 'shared-dir', '..', 'passwd']),
            name: 'passwd',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: {
              kind: 'path',
              projectId: 'proj-1',
              containerPath: '/scion-volumes/../passwd',
              location: { kind: 'shared-dir', dirName: '..', filePath: 'passwd' },
            },
          },
        ],
      })
    );
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(0);
  });

  // The full vector table at the hydrate layer, for both the
  // workspace filePath and the shared-dir dirName positions. Each fixture's
  // `key` is recomputed from the same bad value, so only the targeted check
  // — not the recomputed-key identity check — can reject it.
  describe('discards a hydrated path record from the full unsafe-path vector table', () => {
    const unsafeSegments: Array<[string, string]> = [
      ['a raw .. segment', '../secret'],
      ['a raw . segment', './secret'],
      ['%2e%2e lowercase', '%2e%2e/secret'],
      ['%2E%2E uppercase', '%2E%2E/secret'],
      ['%2f encoded slash lowercase', 'foo%2fsecret'],
      ['%2F encoded slash uppercase', 'foo%2Fsecret'],
      ['%5c encoded backslash lowercase', 'foo%5csecret'],
      ['%5C encoded backslash uppercase', 'foo%5Csecret'],
      ['a literal backslash', 'foo\\secret'],
      ['a ? query delimiter (no ..)', 'notes.txt?x'],
      ['a # fragment delimiter (no ..)', 'notes.txt#x'],
    ];

    it.each(unsafeSegments)('workspace filePath: %s', (_label, filePath) => {
      hydrateWith({
        key: JSON.stringify(['path', 'proj-1', 'workspace', '', filePath]),
        name: 'x',
        source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
        target: {
          kind: 'path',
          projectId: 'proj-1',
          containerPath: '/workspace/' + filePath,
          location: { kind: 'workspace', filePath },
        },
      });
      expect(store.snapshot().records).toHaveLength(0);
    });

    it.each(unsafeSegments)('shared-dir dirName: %s', (_label, dirName) => {
      hydrateWith({
        key: JSON.stringify(['path', 'proj-1', 'shared-dir', dirName, 'x.txt']),
        name: 'x',
        source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
        target: {
          kind: 'path',
          projectId: 'proj-1',
          containerPath: '/scion-volumes/' + dirName + '/x.txt',
          location: { kind: 'shared-dir', dirName, filePath: 'x.txt' },
        },
      });
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  it('ignores a cross-tab storage event carrying a record with a traversal filePath', () => {
    store.setScope(SCOPE);
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    const envelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['path', 'proj-1', 'workspace', '', '../../agents']),
          name: 'agents',
          source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
          target: {
            kind: 'path',
            projectId: 'proj-1',
            containerPath: '/workspace/../../agents',
            location: { kind: 'workspace', filePath: '../../agents' },
          },
        },
      ],
    };
    window.dispatchEvent(
      new StorageEvent('storage', { key, newValue: JSON.stringify(envelope), oldValue: null })
    );
    expect(store.snapshot().records).toHaveLength(0);
  });

  // The full unsafe-id vector table, applied to a path record's project id
  // at the storage-event layer.
  describe('ignores a cross-tab storage event carrying a path record with an unsafe projectId', () => {
    const unsafeProjectIds = [
      '.',
      '..',
      '%2e%2e',
      '%2E%2E',
      'a/b',
      'a?b',
      'a#b',
      'a%2fb',
      'a%5cb',
      'a\\b',
    ];

    it.each(unsafeProjectIds)('projectId=%s', (badProjectId) => {
      store.setScope(SCOPE);
      const key =
        'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
      const envelope = {
        version: 1,
        records: [
          {
            key: JSON.stringify(['path', badProjectId, 'workspace', '', 'a.md']),
            name: 'a.md',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: {
              kind: 'path',
              projectId: badProjectId,
              containerPath: '/workspace/a.md',
              location: { kind: 'workspace', filePath: 'a.md' },
            },
          },
        ],
      };
      window.dispatchEvent(
        new StorageEvent('storage', { key, newValue: JSON.stringify(envelope), oldValue: null })
      );
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  // The full unsafe-id vector table, applied to an attachment record's id at
  // the storage-event layer.
  describe('ignores a cross-tab storage event carrying an attachment record with an unsafe id', () => {
    const unsafeIds = [
      '.',
      '..',
      '%2e%2e',
      '%2E%2E',
      'a/b',
      'a?b',
      'a#b',
      'a%2fb',
      'a%5cb',
      'a\\b',
    ];

    it.each(unsafeIds)('id=%s', (badId) => {
      store.setScope(SCOPE);
      const key =
        'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
      const envelope = {
        version: 1,
        records: [
          {
            key: JSON.stringify(['attachment', badId]),
            name: 'x',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: { kind: 'attachment', id: badId, mime: 'text/plain', size: 5 },
          },
        ],
      };
      window.dispatchEvent(
        new StorageEvent('storage', { key, newValue: JSON.stringify(envelope), oldValue: null })
      );
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  // The full unsafe-path vector table, applied at the storage-event layer.
  describe('ignores a cross-tab storage event carrying a path record from the full unsafe-path vector table', () => {
    const unsafeSegments: Array<[string, string]> = [
      ['a raw .. segment', '../secret'],
      ['a raw . segment', './secret'],
      ['%2e%2e lowercase', '%2e%2e/secret'],
      ['%2E%2E uppercase', '%2E%2E/secret'],
      ['%2f encoded slash lowercase', 'foo%2fsecret'],
      ['%2F encoded slash uppercase', 'foo%2Fsecret'],
      ['%5c encoded backslash lowercase', 'foo%5csecret'],
      ['%5C encoded backslash uppercase', 'foo%5Csecret'],
      ['a literal backslash', 'foo\\secret'],
      ['a ? query delimiter (no ..)', 'notes.txt?x'],
      ['a # fragment delimiter (no ..)', 'notes.txt#x'],
    ];

    it.each(unsafeSegments)('workspace filePath: %s', (_label, filePath) => {
      store.setScope(SCOPE);
      const key =
        'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
      const envelope = {
        version: 1,
        records: [
          {
            key: JSON.stringify(['path', 'proj-1', 'workspace', '', filePath]),
            name: 'x',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: {
              kind: 'path',
              projectId: 'proj-1',
              containerPath: '/workspace/' + filePath,
              location: { kind: 'workspace', filePath },
            },
          },
        ],
      };
      window.dispatchEvent(
        new StorageEvent('storage', { key, newValue: JSON.stringify(envelope), oldValue: null })
      );
      expect(store.snapshot().records).toHaveLength(0);
    });

    it.each(unsafeSegments)('shared-dir dirName: %s', (_label, dirName) => {
      store.setScope(SCOPE);
      const key =
        'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
      const envelope = {
        version: 1,
        records: [
          {
            key: JSON.stringify(['path', 'proj-1', 'shared-dir', dirName, 'x.txt']),
            name: 'x',
            source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
            target: {
              kind: 'path',
              projectId: 'proj-1',
              containerPath: '/scion-volumes/' + dirName + '/x.txt',
              location: { kind: 'shared-dir', dirName, filePath: 'x.txt' },
            },
          },
        ],
      };
      window.dispatchEvent(
        new StorageEvent('storage', { key, newValue: JSON.stringify(envelope), oldValue: null })
      );
      expect(store.snapshot().records).toHaveLength(0);
    });
  });

  it('has no message body, credentials, or preview content in what it persists', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ text: 'secret body text /workspace/a.md' }), [], { projectId: 'proj-1' });
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([SCOPE.origin, SCOPE.baseUrl, SCOPE.userId]);
    const raw = localStorage.getItem(key)!;
    expect(raw).not.toContain('secret body text');
  });

  // -- quota / private-mode fallback ---------------------------------------

  it('falls back to memory-only mode when localStorage.setItem throws', () => {
    store.setScope(SCOPE);
    const spy = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new DOMException('QuotaExceededError');
    });
    try {
      store.ingest(msg(), [att()], {});
      expect(store.snapshot().records).toHaveLength(1); // still usable in memory
      expect(store.snapshot().persistent).toBe(false);
    } finally {
      spy.mockRestore();
    }
  });

  it('does not attempt to write again after persistence has failed once', () => {
    store.setScope(SCOPE);
    const throwingSpy = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new DOMException('QuotaExceededError');
    });
    store.ingest(msg(), [att()], {});
    expect(store.snapshot().persistent).toBe(false);
    throwingSpy.mockRestore();

    const setItemSpy = vi.spyOn(localStorage, 'setItem');
    store.ingest(msg({ id: 'm2', sentAt: '2026-01-02T00:00:00Z' }), [att({ id: 'att-2' })], {});
    expect(setItemSpy).not.toHaveBeenCalled();
    setItemSpy.mockRestore();
  });

  it('starts persistent:false when localStorage.getItem throws on setScope (private mode)', () => {
    const spy = vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new DOMException('SecurityError');
    });
    try {
      store.setScope(SCOPE);
      expect(store.snapshot().persistent).toBe(false);
    } finally {
      spy.mockRestore();
    }
  });

  // -- cross-tab storage-event sync -------------------------------

  function keyFor(scope: typeof SCOPE): string {
    return (
      'scion.chat.recentFiles.v1:' + JSON.stringify([scope.origin, scope.baseUrl, scope.userId])
    );
  }

  it('merges a storage event for this key from another tab', () => {
    store.setScope(SCOPE);
    const envelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['attachment', 'att-other-tab']),
          name: 'other.txt',
          source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
          target: { kind: 'attachment', id: 'att-other-tab', mime: 'text/plain', size: 5 },
        },
      ],
    };
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: keyFor(SCOPE),
        newValue: JSON.stringify(envelope),
        oldValue: null,
      })
    );
    expect(store.snapshot().records).toHaveLength(1);
    expect(store.snapshot().records[0].name).toBe('other.txt');
  });

  it('an incoming storage event for an existing key wins when it is genuinely newer', () => {
    store.setScope(SCOPE);
    store.ingest(
      msg({ id: 'm-old', sentAt: '2020-01-01T00:00:00Z' }),
      [att({ id: 'att-1', name: 'old.txt' })],
      {}
    );
    const envelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['attachment', 'att-1']),
          name: 'new.txt',
          source: { conversationKey: 'c1', messageId: 'm-new', sentAt: '2026-01-01T00:00:00Z' },
          target: { kind: 'attachment', id: 'att-1', mime: 'text/plain', size: 9 },
        },
      ],
    };
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: keyFor(SCOPE),
        newValue: JSON.stringify(envelope),
        oldValue: null,
      })
    );
    expect(store.snapshot().records).toHaveLength(1);
    expect(store.snapshot().records[0].name).toBe('new.txt');
  });

  it('a stale incoming storage event for an existing key does not overwrite the newer local record (the other direction of the newest-wins comparison)', () => {
    store.setScope(SCOPE);
    store.ingest(
      msg({ id: 'm-new', sentAt: '2026-01-01T00:00:00Z' }),
      [att({ id: 'att-1', name: 'new.txt' })],
      {}
    );
    const envelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['attachment', 'att-1']),
          name: 'old.txt',
          source: { conversationKey: 'c1', messageId: 'm-old', sentAt: '2020-01-01T00:00:00Z' },
          target: { kind: 'attachment', id: 'att-1', mime: 'text/plain', size: 9 },
        },
      ],
    };
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: keyFor(SCOPE),
        newValue: JSON.stringify(envelope),
        oldValue: null,
      })
    );
    expect(store.snapshot().records[0].name).toBe('new.txt');
  });

  it('a concurrent tab’s disk-only addition survives this tab’s own write (two-tab interleaving)', () => {
    // Simulates two tabs sharing one localStorage without live storage-event
    // delivery (e.g. this test's own writes happen synchronously and
    // dispatchEvent is same-tab-only in a real browser too — a tab never
    // receives its own storage events). Tab A writes record X directly to
    // disk; tab B (this store) then ingests a different record Y and
    // persists — its own read-merge-write must not drop X.
    store.setScope(SCOPE);
    const key = keyFor(SCOPE);
    const tabAEnvelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['attachment', 'att-tab-a']),
          name: 'from-tab-a.txt',
          source: { conversationKey: 'c1', messageId: 'm-a', sentAt: '2026-01-01T00:00:00Z' },
          target: { kind: 'attachment', id: 'att-tab-a', mime: 'text/plain', size: 5 },
        },
      ],
    };
    localStorage.setItem(key, JSON.stringify(tabAEnvelope));

    store.ingest(msg({ id: 'm-b' }), [att({ id: 'att-tab-b', name: 'from-tab-b.txt' })], {});

    const onDisk = JSON.parse(localStorage.getItem(key)!) as { records: Array<{ name: string }> };
    const names = onDisk.records.map((r) => r.name).sort();
    expect(names).toEqual(['from-tab-a.txt', 'from-tab-b.txt']);
  });

  it('caps to 50 on hydrate even if a corrupt/oversized envelope somehow persisted more', () => {
    const key = keyFor(SCOPE);
    const records = Array.from({ length: 60 }, (_, i) => ({
      key: JSON.stringify(['attachment', `att-${i}`]),
      name: `f${i}.txt`,
      source: { conversationKey: 'c1', messageId: `m${i}`, sentAt: '2026-01-01T00:00:00Z' },
      target: { kind: 'attachment', id: `att-${i}`, mime: 'text/plain', size: 5 },
    }));
    localStorage.setItem(key, JSON.stringify({ version: 1, records }));
    store.setScope(SCOPE);
    expect(store.snapshot().records).toHaveLength(50);
  });

  it('re-authenticating via setScope un-suspends ingestion after a cross-tab logout', () => {
    store.setScope(SCOPE);
    // Cross-tab logout: another tab removed this account's key.
    window.dispatchEvent(
      new StorageEvent('storage', { key: keyFor(SCOPE), newValue: null, oldValue: '{}' })
    );
    store.ingest(msg(), [att()], {});
    expect(store.snapshot().records).toHaveLength(0); // still suspended

    store.setScope(SCOPE); // re-authenticate (e.g. logged back in)
    store.ingest(msg(), [att()], {});
    expect(store.snapshot().records).toHaveLength(1);
  });

  it('ignores a storage event for a different key, even with a valid envelope', () => {
    store.setScope(SCOPE);
    const envelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['attachment', 'att-wrong-key']),
          name: 'wrong.txt',
          source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
          target: { kind: 'attachment', id: 'att-wrong-key', mime: 'text/plain', size: 5 },
        },
      ],
    };
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: 'some.other.key',
        newValue: JSON.stringify(envelope),
        oldValue: null,
      })
    );
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('ignores a storage event when no scope is set', () => {
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: keyFor(SCOPE),
        newValue: '{"version":1,"records":[]}',
        oldValue: null,
      })
    );
    // No throw, no crash — nothing to assert on besides survival.
    expect(true).toBe(true);
  });

  it('ignores a storage event carrying an invalid envelope', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    window.dispatchEvent(
      new StorageEvent('storage', { key: keyFor(SCOPE), newValue: 'not json', oldValue: null })
    );
    expect(store.snapshot().records).toHaveLength(1); // unchanged
  });

  it('a key-removal storage event (cross-tab logout) suspends and clears this store', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    window.dispatchEvent(
      new StorageEvent('storage', { key: keyFor(SCOPE), newValue: null, oldValue: '{}' })
    );
    expect(store.snapshot().records).toHaveLength(0);
    // Suspended: a pending write from before the cross-tab logout must not repopulate it.
    store.ingest(msg(), [att()], {});
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('ignores a later storage event for the same key while still suspended', () => {
    // handleStorageEvent's own newValue===null branch suspends but does not
    // null out this.scope, so a subsequent event for that same key would
    // otherwise still pass the key-match check and merge stale data back in.
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    window.dispatchEvent(
      new StorageEvent('storage', { key: keyFor(SCOPE), newValue: null, oldValue: '{}' })
    );
    expect(store.snapshot().records).toHaveLength(0);

    const staleEnvelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['attachment', 'att-stale']),
          name: 'stale.txt',
          source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
          target: { kind: 'attachment', id: 'att-stale', mime: 'text/plain', size: 5 },
        },
      ],
    };
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: keyFor(SCOPE),
        newValue: JSON.stringify(staleEnvelope),
        oldValue: null,
      })
    );
    expect(store.snapshot().records).toHaveLength(0);
  });

  it('does not write back to storage when the incoming envelope already matches the merged result', () => {
    store.setScope(SCOPE);
    const setItemSpy = vi.spyOn(localStorage, 'setItem');
    const envelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['attachment', 'att-x']),
          name: 'x.txt',
          source: { conversationKey: 'c1', messageId: 'm1', sentAt: '2026-01-01T00:00:00Z' },
          target: { kind: 'attachment', id: 'att-x', mime: 'text/plain', size: 5 },
        },
      ],
    };
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: keyFor(SCOPE),
        newValue: JSON.stringify(envelope),
        oldValue: null,
      })
    );
    expect(setItemSpy).not.toHaveBeenCalled();
    setItemSpy.mockRestore();
  });

  it('writes back a reconciled union when this tab has content beyond the incoming envelope', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'local' }), [att({ id: 'att-local' })], {});
    const envelope = {
      version: 1,
      records: [
        {
          key: JSON.stringify(['attachment', 'att-remote']),
          name: 'remote.txt',
          source: { conversationKey: 'c1', messageId: 'm-remote', sentAt: '2026-01-01T00:00:00Z' },
          target: { kind: 'attachment', id: 'att-remote', mime: 'text/plain', size: 5 },
        },
      ],
    };
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: keyFor(SCOPE),
        newValue: JSON.stringify(envelope),
        oldValue: null,
      })
    );
    expect(
      store
        .snapshot()
        .records.map((r) => r.name)
        .sort()
    ).toEqual(['notes.txt', 'remote.txt']);

    // The in-memory union must actually have been written back to disk too —
    // not just held in memory — so a third tab reading the key next sees
    // both records, not only the one the second tab sent.
    const persistedRaw = localStorage.getItem(keyFor(SCOPE));
    const persisted = JSON.parse(persistedRaw!) as { records: Array<{ name: string }> };
    expect(persisted.records.map((r) => r.name).sort()).toEqual(['notes.txt', 'remote.txt']);
  });

  // -- subscribe ------------------------------------------------------------

  it('notifies subscribers on ingest', () => {
    store.setScope(SCOPE);
    const cb = vi.fn();
    store.subscribe(cb);
    store.ingest(msg(), [att()], {});
    expect(cb).toHaveBeenCalled();
    expect(cb.mock.calls.at(-1)?.[0].records).toHaveLength(1);
  });

  // -- change-detection (applyOne's persist/notify gate), one field at a
  // time. `this.records` updates unconditionally regardless of this check —
  // only persist()/notify() are gated by it — so these must assert via the
  // subscriber callback, not snapshot(), to actually discriminate it. -----

  it('notifies when a correction changes only sentAt', () => {
    store.setScope(SCOPE);
    store.ingest(
      msg({ id: 'm1', sentAt: '2026-01-01T00:00:05Z' }),
      [att({ id: 'att-1', name: 'a.txt' })],
      {
        provisional: true,
      }
    );
    const cb = vi.fn();
    store.subscribe(cb);
    store.ingest(
      msg({ id: 'm1', sentAt: '2026-01-01T00:00:01Z' }),
      [att({ id: 'att-1', name: 'a.txt' })],
      {}
    );
    expect(cb).toHaveBeenCalled();
  });

  it('notifies when a correction changes only the name', () => {
    store.setScope(SCOPE);
    const sentAt = '2026-01-01T00:00:00Z';
    store.ingest(msg({ id: 'm1', sentAt }), [att({ id: 'att-1', name: 'old.txt' })], {
      provisional: true,
    });
    const cb = vi.fn();
    store.subscribe(cb);
    store.ingest(msg({ id: 'm1', sentAt }), [att({ id: 'att-1', name: 'new.txt' })], {});
    expect(cb).toHaveBeenCalled();
  });

  it('notifies when a correction changes only the target (mime)', () => {
    store.setScope(SCOPE);
    const sentAt = '2026-01-01T00:00:00Z';
    store.ingest(msg({ id: 'm1', sentAt }), [att({ id: 'att-1', mime: 'text/plain' })], {
      provisional: true,
    });
    const cb = vi.fn();
    store.subscribe(cb);
    store.ingest(
      msg({ id: 'm1', sentAt }),
      [att({ id: 'att-1', mime: 'application/octet-stream' })],
      {}
    );
    expect(cb).toHaveBeenCalled();
  });

  it('notifies when only messageId differs (tied sentAt, messageId tie-break decides the replacement)', () => {
    store.setScope(SCOPE);
    store.ingest(msg({ id: 'm1', sentAt: '2026-01-01T00:00:00Z' }), [att({ id: 'att-1' })], {});
    const cb = vi.fn();
    store.subscribe(cb);
    // 'm2' > 'm1' lexicographically, so isNewerOccurrence accepts it purely
    // on the messageId tie-break; only messageId differs from the stored record.
    store.ingest(msg({ id: 'm2', sentAt: '2026-01-01T00:00:00Z' }), [att({ id: 'att-1' })], {});
    expect(cb).toHaveBeenCalled();
  });

  it('notifies when only conversationKey differs (tied sentAt and messageId, conversationKey tie-break decides)', () => {
    store.setScope(SCOPE);
    store.ingest(
      msg({ id: 'same-id', sentAt: '2026-01-01T00:00:00Z', conversationKey: 'conv-a' }),
      [att({ id: 'att-1' })],
      {}
    );
    const cb = vi.fn();
    store.subscribe(cb);
    store.ingest(
      msg({ id: 'same-id', sentAt: '2026-01-01T00:00:00Z', conversationKey: 'conv-b' }),
      [att({ id: 'att-1' })],
      {}
    );
    expect(cb).toHaveBeenCalled();
  });

  it('unsubscribe stops further notifications', () => {
    store.setScope(SCOPE);
    const cb = vi.fn();
    const unsubscribe = store.subscribe(cb);
    unsubscribe();
    store.ingest(msg(), [att()], {});
    expect(cb).not.toHaveBeenCalled();
  });

  // -- explicit logout ------------------------------------------------------

  it('clearForLogout removes the persisted key', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    const key = keyFor(SCOPE);
    expect(localStorage.getItem(key)).not.toBeNull();
    store.clearForLogout();
    expect(localStorage.getItem(key)).toBeNull();
  });

  it('clearForLogout clears in-memory records immediately', () => {
    store.setScope(SCOPE);
    store.ingest(msg(), [att()], {});
    store.clearForLogout();
    expect(store.snapshot().records).toHaveLength(0);
  });
});
