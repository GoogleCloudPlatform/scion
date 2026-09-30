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
 * Identity-scoped module singleton tracking the 50 most recent distinct
 * files (attachments and detected container paths) encountered in chat, for
 * the "Documents" quadrant of the quick command palette.
 *
 * A module-level singleton, not component state: `chat-thread`/the chat page
 * are recreated on conversation switch and `navigateTo`, but this index must
 * survive both, so it cannot live inside either one's own component state.
 *
 * Ptone's decision: only files, never HTTP(S) URLs — callers must resolve
 * paths with `utils/chat-file-links.ts`'s `extractContainerPaths`, which
 * already excludes URL-embedded paths. The cap (50) is shared by attachments
 * and detected paths together.
 */

import {
  attachmentIdentityKey,
  extractContainerPaths,
  hasOnlySafePathSegments,
  hasOnlySafeSingleSegment,
  isRecognizedFilePath,
  parseContainerPath,
  pathIdentityKey,
  type PathLinkTarget,
} from '../utils/chat-file-links.js';

/** Attachment identity + display fields needed to index it; structurally compatible with chat-message.ts's `AttachmentRefInfo`. */
export interface AttachmentRefLike {
  id: string;
  name: string;
  mime: string;
  size: number;
}

/** Where a recorded file came from — enough to dedupe and (later) jump back to it. */
export interface RecentFileSource {
  conversationKey: string;
  messageId: string;
  /** ISO timestamp; message send time, never viewing/load time. */
  sentAt: string;
  projectId?: string;
  projectName?: string;
}

/** What a recorded file resolves to — enough to preview or download it. */
export type RecentFileTarget =
  | { kind: 'attachment'; id: string; mime: string; size: number }
  | { kind: 'path'; projectId: string; containerPath: string; location: PathLinkTarget };

/** One entry in the recent-files index. */
export interface RecentFile {
  /** Stable identity tuple — see `attachmentIdentityKey` / `pathIdentityKey`. */
  key: string;
  name: string;
  source: RecentFileSource;
  target: RecentFileTarget;
}

/** The version-1 persisted envelope. */
export interface RecentFilesEnvelope {
  version: 1;
  records: RecentFile[];
}

/** Identity a store is scoped to: current origin, app base path, and user. */
export interface RecentFilesScope {
  origin: string;
  baseUrl: string;
  userId: string;
}

/** One message's ingestible content. */
export interface IngestMessageInput {
  /** The server-assigned message ID — never an optimistic/idempotency-key ID. */
  id: string;
  conversationKey: string;
  /** ISO timestamp: the message's real send time. */
  sentAt: string;
  /** Raw message text, scanned for inline container-path mentions. */
  text?: string;
  /** Wave-1 `Message.attachments` (legacy uploaded-file paths). */
  legacyAttachmentPaths?: string[];
}

/** Context needed to resolve a message's file paths and mark provisional records. */
export interface IngestContext {
  /** The resolved project ID for this message's paths (per `resolveMessageProjectId`). Omitted paths are dropped, not guessed. */
  projectId?: string;
  projectName?: string;
  /**
   * True only for an own-send success response that lacks the server's
   * authoritative timestamp: the record uses the client's original send time
   * until a later authoritative copy (SSE echo or backfill) corrects it, even
   * if the corrected time is earlier.
   */
  provisional?: boolean;
}

/** Options for a single `ingest` call. */
export interface IngestOptions {
  /**
   * The scope generation captured before starting the async work that
   * produced this ingest call. If provided and no longer current, the call
   * is dropped — an in-flight response from a superseded (logged-out/
   * switched) identity must never repopulate a different account's store.
   */
  scopeGeneration?: number;
}

/** What subscribers receive. */
export interface RecentFilesSnapshot {
  records: RecentFile[];
  /** False once persistence has failed for this scope (quota / private mode). */
  persistent: boolean;
}

/** Cap shared by attachments and detected paths together (ptone's decision). */
export const RECENT_FILES_CAP = 50;

const STORAGE_VERSION = 1;
const MAX_TEXT_LEN = 4096;
const MAX_ID_LEN = 512;

function storageKey(scope: RecentFilesScope): string {
  return 'scion.chat.recentFiles.v1:' + JSON.stringify([scope.origin, scope.baseUrl, scope.userId]);
}

/** A Go zero-time serializes as `0001-01-01T00:00:00Z`; treat it, and any unparseable string, as unknown (0). */
function sentAtMs(sentAt: string): number {
  if (!sentAt || sentAt.startsWith('0001-01-01')) return 0;
  const ms = Date.parse(sentAt);
  return Number.isFinite(ms) ? ms : 0;
}

/** Newest `source.sentAt` wins; ties break by messageId then conversationKey (lexicographic, deterministic — not a claim about which "should" logically win). */
function isNewerOccurrence(candidate: RecentFile, current: RecentFile): boolean {
  const candMs = sentAtMs(candidate.source.sentAt);
  const curMs = sentAtMs(current.source.sentAt);
  if (candMs !== curMs) return candMs > curMs;
  if (candidate.source.messageId !== current.source.messageId) {
    return candidate.source.messageId > current.source.messageId;
  }
  return candidate.source.conversationKey > current.source.conversationKey;
}

function isNonEmptyString(v: unknown, maxLen: number): v is string {
  return typeof v === 'string' && v.length > 0 && v.length <= maxLen;
}

/** Validate and normalize one candidate record from storage/another tab; discard anything malformed. */
function sanitizeRecord(candidate: unknown): RecentFile | null {
  if (!candidate || typeof candidate !== 'object') return null;
  const c = candidate as Record<string, unknown>;
  if (!isNonEmptyString(c.key, MAX_TEXT_LEN) || !isNonEmptyString(c.name, MAX_TEXT_LEN))
    return null;

  const source = c.source as Record<string, unknown> | undefined;
  if (
    !source ||
    !isNonEmptyString(source.conversationKey, MAX_TEXT_LEN) ||
    !isNonEmptyString(source.messageId, MAX_ID_LEN) ||
    !isNonEmptyString(source.sentAt, MAX_TEXT_LEN)
  ) {
    return null;
  }
  if (
    source.projectId !== undefined &&
    (!isNonEmptyString(source.projectId, MAX_ID_LEN) || !hasOnlySafeSingleSegment(source.projectId))
  )
    return null;
  if (source.projectName !== undefined && !isNonEmptyString(source.projectName, MAX_TEXT_LEN)) {
    return null;
  }
  const sanitizedSource: RecentFileSource = {
    conversationKey: source.conversationKey,
    messageId: source.messageId,
    sentAt: source.sentAt,
    ...(typeof source.projectId === 'string' ? { projectId: source.projectId } : {}),
    ...(typeof source.projectName === 'string' ? { projectName: source.projectName } : {}),
  };

  const target = c.target as Record<string, unknown> | undefined;
  if (!target || typeof target.kind !== 'string') return null;

  let sanitizedTarget: RecentFileTarget;
  if (target.kind === 'attachment') {
    if (
      !isNonEmptyString(target.id, MAX_ID_LEN) ||
      !hasOnlySafeSingleSegment(target.id) ||
      typeof target.mime !== 'string' ||
      target.mime.length > MAX_TEXT_LEN ||
      typeof target.size !== 'number' ||
      !Number.isFinite(target.size) ||
      target.size < 0
    ) {
      return null;
    }
    sanitizedTarget = { kind: 'attachment', id: target.id, mime: target.mime, size: target.size };
  } else if (target.kind === 'path') {
    const location = target.location as Record<string, unknown> | undefined;
    if (
      !isNonEmptyString(target.projectId, MAX_ID_LEN) ||
      !hasOnlySafeSingleSegment(target.projectId) ||
      !isNonEmptyString(target.containerPath, MAX_TEXT_LEN) ||
      !location ||
      (location.kind !== 'workspace' && location.kind !== 'shared-dir') ||
      typeof location.filePath !== 'string' ||
      location.filePath.length > MAX_TEXT_LEN ||
      !hasOnlySafePathSegments(location.filePath)
    ) {
      return null;
    }
    if (
      location.kind === 'shared-dir' &&
      (!isNonEmptyString(location.dirName, MAX_TEXT_LEN) ||
        !hasOnlySafePathSegments(location.dirName))
    ) {
      return null;
    }
    sanitizedTarget = {
      kind: 'path',
      projectId: target.projectId,
      containerPath: target.containerPath,
      location: {
        kind: location.kind,
        filePath: location.filePath,
        ...(location.kind === 'shared-dir' ? { dirName: location.dirName as string } : {}),
      },
    };
  } else {
    return null;
  }

  // Integrity check: a tampered/corrupted persisted record (or a crafted
  // cross-tab envelope) could carry a `key` that doesn't actually match its
  // own `target` — recompute the identity from the target itself and reject
  // any mismatch, rather than trusting the stored key at face value.
  const recomputedKey =
    sanitizedTarget.kind === 'attachment'
      ? attachmentIdentityKey(sanitizedTarget.id)
      : pathIdentityKey(sanitizedTarget.projectId, sanitizedTarget.location);
  if (recomputedKey !== c.key) return null;

  return {
    key: c.key,
    name: c.name,
    source: sanitizedSource,
    target: sanitizedTarget,
  };
}

/**
 * Owns the recent-files index for the current identity: validation,
 * dedupe/cap, localStorage persistence with cross-tab sync, and the
 * provisional-send timestamp correction.
 */
export class ChatRecentFilesStore {
  private scope: RecentFilesScope | null = null;
  private records: RecentFile[] = [];
  private persistentFlag = true;
  private suspended = false;
  private generationCounter = 0;
  /** identity key -> messageId, only while that occurrence's timestamp is still the client's own guess. */
  private provisionalByKey = new Map<string, string>();
  private subscribers = new Set<(snapshot: RecentFilesSnapshot) => void>();
  private lastWrittenSerialized: string | null = null;

  private readonly boundStorageListener = (e: StorageEvent): void => this.handleStorageEvent(e);

  constructor() {
    if (typeof window !== 'undefined') {
      window.addEventListener('storage', this.boundStorageListener);
    }
  }

  /** Detach the cross-tab listener. Only needed for test instances; the module singleton lives for the page's lifetime. */
  dispose(): void {
    if (typeof window !== 'undefined') {
      window.removeEventListener('storage', this.boundStorageListener);
    }
  }

  /** The current scope generation. Callers starting async work should capture this and pass it back via {@link IngestOptions.scopeGeneration}. */
  get scopeGeneration(): number {
    return this.generationCounter;
  }

  /**
   * Switch to a new identity (or none). Aborts old work by advancing the
   * generation, resets in-memory state, and — for a real identity — hydrates
   * from localStorage. Call only once the user's identity is known.
   */
  setScope(scope: RecentFilesScope | null): void {
    this.generationCounter++;
    this.suspended = false;
    this.provisionalByKey.clear();

    if (!scope) {
      this.scope = null;
      this.records = [];
      this.persistentFlag = true;
      this.lastWrittenSerialized = null;
      this.notify();
      return;
    }

    this.scope = scope;
    const hydrated = this.readFromStorage(scope);
    this.records = hydrated.records;
    this.persistentFlag = hydrated.persistent;
    this.lastWrittenSerialized = hydrated.persistent ? this.canonicalize(this.records) : null;
    this.notify();
  }

  /**
   * Record a message's attachments and detected container paths. A no-op
   * once suspended (post-logout) or with no active scope, and a no-op if
   * `opts.scopeGeneration` names a generation that is no longer current.
   */
  ingest(
    message: IngestMessageInput,
    attachmentRefs: readonly AttachmentRefLike[],
    context: IngestContext = {},
    opts: IngestOptions = {}
  ): void {
    if (this.suspended || !this.scope) return;
    if (opts.scopeGeneration !== undefined && opts.scopeGeneration !== this.generationCounter)
      return;
    // Redundant with sanitizeRecord (which buildCandidates already funnels
    // every fresh candidate through, immediately below, not just on a later
    // reload): an empty message.id or conversationKey fails sanitizeRecord's
    // own non-empty checks on source.messageId/conversationKey regardless of
    // this guard, so removing this line alone changes no test outcome. Left
    // in place as a purely defensive, early-exit mirror of that check, not
    // as independent protection.
    if (!message.id || !message.conversationKey) return;

    const candidates = this.buildCandidates(message, attachmentRefs, context);
    if (candidates.length === 0) return;

    let changed = false;
    for (const candidate of candidates) {
      if (this.applyOne(candidate, message.id, !!context.provisional)) changed = true;
    }
    if (!changed) return;

    this.records = this.capRecords(this.records);
    this.persist();
    this.notify();
  }

  /** Current records and whether persistence is available, for rendering/notices. */
  snapshot(): RecentFilesSnapshot {
    return { records: [...this.records], persistent: this.persistentFlag };
  }

  /** Subscribe to snapshot changes; returns an unsubscribe function. */
  subscribe(callback: (snapshot: RecentFilesSnapshot) => void): () => void {
    this.subscribers.add(callback);
    return () => {
      this.subscribers.delete(callback);
    };
  }

  /**
   * Explicit logout: suspend ingestion first, then clear this account's
   * persisted key and memory. Call before the actual logout network request
   * runs, so nothing races an authenticated response into a cleared store.
   */
  clearForLogout(): void {
    this.suspended = true;
    this.generationCounter++;
    this.provisionalByKey.clear();
    const scope = this.scope;
    this.records = [];
    this.lastWrittenSerialized = null;
    this.scope = null;
    if (scope) {
      try {
        localStorage.removeItem(storageKey(scope));
      } catch {
        // Private mode / no storage — nothing persisted to clear.
      }
    }
    this.notify();
  }

  // -- internals -------------------------------------------------------

  private notify(): void {
    const snap = this.snapshot();
    for (const cb of this.subscribers) cb(snap);
  }

  private buildCandidates(
    message: IngestMessageInput,
    attachmentRefs: readonly AttachmentRefLike[],
    context: IngestContext
  ): RecentFile[] {
    const projectId = context.projectId || '';
    const source: RecentFileSource = {
      conversationKey: message.conversationKey,
      messageId: message.id,
      sentAt: message.sentAt,
      ...(projectId ? { projectId } : {}),
      ...(context.projectName ? { projectName: context.projectName } : {}),
    };

    const out: RecentFile[] = [];

    for (const ref of attachmentRefs) {
      if (!ref || !ref.id) continue;
      out.push({
        key: attachmentIdentityKey(ref.id),
        name: ref.name || ref.id,
        source,
        target: {
          kind: 'attachment',
          id: ref.id,
          mime: ref.mime || '',
          size: Number.isFinite(ref.size) ? ref.size : 0,
        },
      });
    }

    // Paths need a resolved project to route the file API; without one, the
    // path is omitted (not guessed) — a later authoritative ingest can still
    // add it once resolution succeeds. Attachments are indexed regardless.
    if (projectId) {
      const pathStrings = new Set<string>();
      // Legacy (wave-1) attachment paths are raw strings straight from the
      // message, not text scanned by extractContainerPaths's restrictive
      // pattern — run them through the same "is this actually a file" check
      // before they're eligible at all.
      for (const p of message.legacyAttachmentPaths ?? []) {
        if (isRecognizedFilePath(p)) pathStrings.add(p);
      }
      for (const p of extractContainerPaths(message.text ?? '')) pathStrings.add(p);

      for (const containerPath of pathStrings) {
        // parseContainerPath itself rejects traversal/encoded/injection
        // segments and a bare directory reference (see its doc comment) —
        // this is the second, independent layer for legacy paths, which
        // skip extractContainerPaths's character-class restriction entirely.
        const location = parseContainerPath(containerPath);
        if (!location) continue;
        out.push({
          key: pathIdentityKey(projectId, location),
          name: containerPath.split('/').pop() || containerPath,
          source,
          target: { kind: 'path', projectId, containerPath, location },
        });
      }
    }

    // Route every freshly-built candidate through the same validator hydrate
    // and storage events use: a record that skipped sanitizeRecord's full set
    // of bounds here (e.g. an unusually long messageId) would sit in memory
    // only to be silently dropped the next time it round-tripped storage.
    const sanitized: RecentFile[] = [];
    for (const candidate of out) {
      const record = sanitizeRecord(candidate);
      if (record) sanitized.push(record);
    }
    return sanitized;
  }

  /** Apply one candidate against current in-memory records. Returns true if it changed anything. */
  private applyOne(candidate: RecentFile, messageId: string, provisional: boolean): boolean {
    const key = candidate.key;
    const existingIdx = this.records.findIndex((r) => r.key === key);
    const provisionalMessageId = this.provisionalByKey.get(key);
    const isCorrection = provisionalMessageId !== undefined && provisionalMessageId === messageId;

    if (existingIdx === -1) {
      this.records = [...this.records, candidate];
      this.setProvisional(key, messageId, provisional);
      return true;
    }

    const current = this.records[existingIdx];
    if (!isCorrection && !isNewerOccurrence(candidate, current)) {
      return false;
    }

    const next = [...this.records];
    next[existingIdx] = candidate;
    this.records = next;
    this.setProvisional(key, messageId, provisional);
    return (
      current.source.sentAt !== candidate.source.sentAt ||
      current.source.messageId !== candidate.source.messageId ||
      current.source.conversationKey !== candidate.source.conversationKey ||
      current.name !== candidate.name ||
      JSON.stringify(current.target) !== JSON.stringify(candidate.target)
    );
  }

  private setProvisional(key: string, messageId: string, provisional: boolean): void {
    if (provisional) {
      this.provisionalByKey.set(key, messageId);
    } else {
      this.provisionalByKey.delete(key);
    }
  }

  private capRecords(records: readonly RecentFile[]): RecentFile[] {
    return [...records]
      .sort((a, b) => {
        const diff = sentAtMs(b.source.sentAt) - sentAtMs(a.source.sentAt);
        if (diff !== 0) return diff;
        return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
      })
      .slice(0, RECENT_FILES_CAP);
  }

  /** Identity-keyed union of two record lists using the standard (non-provisional-aware) newest-wins rule, then capped. Used to reconcile against a `storage` event from another tab, where neither side has "authority" over the other. */
  private mergeRecords(base: readonly RecentFile[], incoming: readonly RecentFile[]): RecentFile[] {
    const byKey = new Map<string, RecentFile>();
    for (const r of base) byKey.set(r.key, r);
    for (const r of incoming) {
      const cur = byKey.get(r.key);
      if (!cur || isNewerOccurrence(r, cur)) byKey.set(r.key, r);
    }
    return this.capRecords([...byKey.values()]);
  }

  /**
   * Reconcile this store's own in-memory truth with whatever is currently on
   * disk before writing. Unlike {@link mergeRecords}, `this.records` always
   * wins for a key it already tracks — it is the outcome of `applyOne`'s
   * provisional-correction logic, which a naive newest-sentAt comparison
   * against our own earlier (provisional) write would silently undo (a
   * corrected timestamp can be *earlier* than the guess it replaces). Only a
   * key this store has never seen — genuinely new content from a concurrent
   * write this tab hasn't merged yet — is pulled in from disk.
   */
  private reconcileOwnWrite(onDisk: readonly RecentFile[]): RecentFile[] {
    const byKey = new Map<string, RecentFile>();
    for (const r of onDisk) byKey.set(r.key, r);
    for (const r of this.records) byKey.set(r.key, r);
    return this.capRecords([...byKey.values()]);
  }

  private canonicalize(records: readonly RecentFile[]): string {
    return JSON.stringify(this.capRecords(records));
  }

  private parseEnvelope(raw: string): RecentFile[] | null {
    let data: unknown;
    try {
      data = JSON.parse(raw);
    } catch {
      return null;
    }
    if (!data || typeof data !== 'object') return null;
    const env = data as { version?: unknown; records?: unknown };
    if (env.version !== STORAGE_VERSION || !Array.isArray(env.records)) return null;
    const out: RecentFile[] = [];
    for (const candidate of env.records) {
      const sanitized = sanitizeRecord(candidate);
      if (sanitized) out.push(sanitized);
    }
    return out;
  }

  private readFromStorage(scope: RecentFilesScope): { records: RecentFile[]; persistent: boolean } {
    let raw: string | null;
    try {
      raw = localStorage.getItem(storageKey(scope));
    } catch {
      return { records: [], persistent: false };
    }
    if (raw === null) return { records: [], persistent: true };
    const parsed = this.parseEnvelope(raw);
    return { records: parsed ? this.capRecords(parsed) : [], persistent: true };
  }

  /** Read-merge-write so a concurrent write from another tab is not lost. No-op (and disables persistence) on quota/private-mode failure. */
  private persist(): void {
    if (!this.scope || !this.persistentFlag) return;
    const scope = this.scope;
    try {
      const onDisk = this.readFromStorage(scope);
      const merged = this.reconcileOwnWrite(onDisk.records);
      this.records = merged;
      const serialized = this.canonicalize(merged);
      if (serialized === this.lastWrittenSerialized) return; // avoid a redundant write-back loop
      const envelope: RecentFilesEnvelope = { version: STORAGE_VERSION, records: merged };
      localStorage.setItem(storageKey(scope), JSON.stringify(envelope));
      this.lastWrittenSerialized = serialized;
    } catch {
      this.persistentFlag = false;
    }
  }

  private handleStorageEvent(e: StorageEvent): void {
    // Once suspended (post-logout, before re-authentication), a storage
    // event for the old key must not be merged back in — there is no active
    // identity for it to belong to.
    if (this.suspended || !this.scope || e.key !== storageKey(this.scope)) return;

    if (e.newValue === null) {
      // Another tab cleared this account's key (logout). Suspend/clear here
      // too — never merge a pre-logout pending write back into it.
      this.suspended = true;
      this.generationCounter++;
      this.provisionalByKey.clear();
      this.records = [];
      this.lastWrittenSerialized = null;
      this.notify();
      return;
    }

    const incoming = this.parseEnvelope(e.newValue);
    if (!incoming) return;

    const merged = this.mergeRecords(this.records, incoming);
    const mergedSerialized = this.canonicalize(merged);
    const incomingSerialized = this.canonicalize(incoming);
    this.records = merged;
    if (mergedSerialized !== incomingSerialized) {
      // Our union carries content beyond what's on disk — reconcile it back.
      this.persist();
    } else {
      this.lastWrittenSerialized = mergedSerialized;
    }
    this.notify();
  }
}

/** The module-level singleton — see the class doc for why this can't be page/component state. */
export const chatRecentFiles = new ChatRecentFilesStore();
