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
 * Autosave and concurrency controller for durable artifact review drafts
 * (ptone/scion#7116, ptone/scion#8471).
 *
 * Rules:
 * - Nothing is written until the first change after entering Review mode.
 * - The first change after the last acknowledged save starts one ~30 s timer;
 *   subsequent typing does NOT reset it.
 * - One PUT is in flight at a time; edits made while a save is in flight
 *   coalesce into the next save.
 * - Also saves on `visibilitychange -> hidden` (with `keepalive: true` only
 *   when the serialized payload is under 60 KiB) and immediately on Save Draft.
 * - Arms a `beforeunload` guard whenever the buffer or note differs from the
 *   last state acknowledged by the hub (or while a save is in flight).
 * - Never writes draft data to localStorage, sessionStorage, IndexedDB, or
 *   caches (security condition C8).
 */

import {
  DRAFT_KEEPALIVE_LIMIT_BYTES,
  DraftApiError,
  DraftConflictError,
  MAX_DRAFT_BODY_BYTES,
  deleteReviewDraft,
  getReviewDraft,
  putReviewDraft,
  type CarriedReviewDraft,
  type PutReviewDraftRequest,
  type PutReviewDraftResponse,
  type ReviewDraft,
  type ReviewDraftResponse,
} from './artifacts.js';

/** Default autosave delay after the first unsaved change (~30 s). */
export const REVIEW_DRAFT_AUTOSAVE_MS = 30_000;

/** Minimum retry backoff for transient / 429 failures (2 s). */
export const REVIEW_DRAFT_RETRY_MIN_MS = 2_000;

/** Maximum retry backoff for transient / 429 failures (60 s). */
export const REVIEW_DRAFT_RETRY_MAX_MS = 60_000;

export type DraftStatusKind =
  | 'empty'
  | 'unsaved'
  | 'saving'
  | 'saved'
  | 'retrying'
  | 'conflict'
  | 'too_large'
  | 'too_many_drafts'
  | 'no_access';

export interface DraftConflictInfo {
  rev: number;
  savedAt: string | null;
  base: number;
}

export interface DraftSnapshot {
  status: DraftStatusKind;
  rev: number;
  savedAt: string | null;
  inFlight: boolean;
  dirty: boolean;
  conflict: DraftConflictInfo | null;
}

export interface ReviewDraftBuffer {
  body: string;
  note: string;
}

export interface ReviewDraftControllerOptions {
  artifactId: string;
  base: number;
  entry: string;
  initialBody: string;
  initialNote: string;
  initialRev?: number;
  initialSavedAt?: string | null;
  carried?: CarriedReviewDraft | null;
  /** Reads the current editor buffer and note (after flushing any editor debounce). */
  readState: () => ReviewDraftBuffer;
  /** Called whenever the snapshot changes so the hosting component can re-render. */
  onChange: (snapshot: DraftSnapshot) => void;
  autosaveMs?: number;
  retryMinMs?: number;
  retryMaxMs?: number;
  /** Test seams; default to the hub wrappers and browser globals. */
  get?: (artifactId: string, opts?: { signal?: AbortSignal }) => Promise<ReviewDraftResponse>;
  put?: (
    artifactId: string,
    req: PutReviewDraftRequest,
    opts?: { keepalive?: boolean; signal?: AbortSignal }
  ) => Promise<PutReviewDraftResponse>;
  del?: (artifactId: string, opts?: { rev?: number; signal?: AbortSignal }) => Promise<void>;
  doc?: Document;
  win?: Window;
  now?: () => Date;
}

const utf8Encoder = new TextEncoder();

/** Formats an ISO timestamp as local 24-hour HH:MM for the status line. */
export function formatDraftTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  return `${hh}:${mm}`;
}

/** Returns the status-line copy for a snapshot (empty before any save). */
export function draftStatusText(snapshot: DraftSnapshot): string {
  switch (snapshot.status) {
    case 'empty':
      return '';
    case 'unsaved':
      return 'Unsaved changes';
    case 'saving':
      return 'Saving…';
    case 'saved': {
      const hhmm = snapshot.savedAt ? formatDraftTime(snapshot.savedAt) : '';
      return hhmm ? `Draft saved ${hhmm}` : 'Draft saved';
    }
    case 'retrying':
      return 'Not saved: retrying';
    case 'conflict':
      return 'Changed on another device or tab';
    case 'too_large':
      return 'This review is too large to save as a draft (over 1 MB). Download it to keep a copy.';
    case 'too_many_drafts':
      return 'You have 200 saved draft reviews. Submit or discard some to save this one.';
    case 'no_access':
      return 'You no longer have access to review this artifact.';
  }
}

export class ReviewDraftController {
  readonly artifactId: string;
  private base: number;
  private entry: string;
  private carried: CarriedReviewDraft | null;
  private readonly opts: ReviewDraftControllerOptions;
  private readonly getFn: NonNullable<ReviewDraftControllerOptions['get']>;
  private readonly putFn: NonNullable<ReviewDraftControllerOptions['put']>;
  private readonly delFn: NonNullable<ReviewDraftControllerOptions['del']>;
  private readonly doc: Document;
  private readonly win: Window;
  private readonly nowFn: () => Date;
  private readonly autosaveMs: number;
  private readonly retryMinMs: number;
  private readonly retryMaxMs: number;

  private rev: number;
  private savedAt: string | null;
  private ackedBody: string;
  private ackedNote: string;
  private status: DraftStatusKind;
  private inFlight = false;
  private saveAgainImmediate = false;
  private pausedReason: 'conflict' | 'too_large' | 'too_many_drafts' | 'no_access' | null = null;
  private conflict: DraftConflictInfo | null = null;
  private retryDelayMs: number;
  private autosaveTimer: ReturnType<typeof setTimeout> | null = null;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private abortController: AbortController | null = null;
  private disposed = false;

  constructor(opts: ReviewDraftControllerOptions) {
    this.opts = opts;
    this.artifactId = opts.artifactId;
    this.base = opts.base;
    this.entry = opts.entry;
    this.carried = opts.carried ?? null;
    this.getFn = opts.get ?? getReviewDraft;
    this.putFn = opts.put ?? putReviewDraft;
    this.delFn = opts.del ?? deleteReviewDraft;
    this.doc = opts.doc ?? document;
    this.win = opts.win ?? window;
    this.nowFn = opts.now ?? (() => new Date());
    this.autosaveMs = opts.autosaveMs ?? REVIEW_DRAFT_AUTOSAVE_MS;
    this.retryMinMs = opts.retryMinMs ?? REVIEW_DRAFT_RETRY_MIN_MS;
    this.retryMaxMs = opts.retryMaxMs ?? REVIEW_DRAFT_RETRY_MAX_MS;
    this.retryDelayMs = this.retryMinMs;

    this.rev = opts.initialRev ?? 0;
    this.savedAt = opts.initialSavedAt ?? null;
    this.ackedBody = opts.initialBody;
    this.ackedNote = opts.initialNote;
    this.status = this.rev > 0 && this.savedAt ? 'saved' : 'empty';

    this.doc.addEventListener('visibilitychange', this.onVisibility);
    this.win.addEventListener('beforeunload', this.onBeforeUnload);
  }

  /** Current revision acknowledged by the hub (0 when no draft exists yet). */
  get currentRev(): number {
    return this.rev;
  }

  /** Last body acknowledged by the hub (or the starting text before any save). */
  get lastAcknowledged(): ReviewDraftBuffer {
    return { body: this.ackedBody, note: this.ackedNote };
  }

  /** True when the current buffer or note differs from the last hub-acknowledged state. */
  get isDirty(): boolean {
    const cur = this.opts.readState();
    return cur.body !== this.ackedBody || cur.note !== this.ackedNote;
  }

  /** True when leaving the page or closing Review mode would lose unacknowledged edits. */
  get hasUnsavedChanges(): boolean {
    return this.isDirty || this.inFlight;
  }

  /** True when the user can click Save Draft right now. */
  get canSaveNow(): boolean {
    if (this.disposed || this.inFlight || !this.isDirty) return false;
    return (
      this.pausedReason !== 'conflict' &&
      this.pausedReason !== 'no_access' &&
      this.pausedReason !== 'too_large'
    );
  }

  getSnapshot(): DraftSnapshot {
    return {
      status: this.status,
      rev: this.rev,
      savedAt: this.savedAt,
      inFlight: this.inFlight,
      dirty: this.isDirty,
      conflict: this.conflict ? { ...this.conflict } : null,
    };
  }

  /**
   * Called whenever the review text or note changes. Starts one ~30 s timer
   * on the first unsaved change; subsequent typing does not reset the timer.
   */
  notifyChange(): void {
    if (this.disposed) return;
    const cur = this.opts.readState();

    // If autosave paused for 413 draft_too_large, resume once the text shrinks back under the cap.
    if (
      this.pausedReason === 'too_large' &&
      utf8Encoder.encode(cur.body).length <= MAX_DRAFT_BODY_BYTES
    ) {
      this.pausedReason = null;
    }

    const dirty = cur.body !== this.ackedBody || cur.note !== this.ackedNote;
    if (!dirty) {
      this.clearAutosaveTimer();
      this.clearRetryTimer();
      if (!this.inFlight && !this.pausedReason) {
        this.status = this.rev > 0 && this.savedAt ? 'saved' : 'empty';
      }
      this.emit();
      return;
    }

    if (this.pausedReason) {
      this.emit();
      return;
    }

    if (this.inFlight) {
      // Newer edits coalesce into the next save after the in-flight PUT finishes.
      this.emit();
      return;
    }

    if (!this.autosaveTimer && !this.retryTimer) {
      this.autosaveTimer = setTimeout(() => {
        this.autosaveTimer = null;
        void this.performSave(false);
      }, this.autosaveMs);
      this.status = 'unsaved';
    }
    this.emit();
  }

  /**
   * Immediately saves the current buffer (Save Draft button or Save draft and
   * close). Resets the autosave timer. Resolves true if the buffer is now
   * acknowledged by the hub, false if saving failed or hit a conflict.
   */
  async saveNow(): Promise<boolean> {
    if (this.disposed) return false;
    if (this.pausedReason === 'conflict' || this.pausedReason === 'no_access') {
      return false;
    }
    if (this.pausedReason === 'too_large') {
      const cur = this.opts.readState();
      if (utf8Encoder.encode(cur.body).length > MAX_DRAFT_BODY_BYTES) {
        return false;
      }
      this.pausedReason = null;
    }
    if (this.pausedReason === 'too_many_drafts') {
      // Allow manual retry after the user discards other drafts.
      this.pausedReason = null;
    }
    this.clearAutosaveTimer();
    this.clearRetryTimer();
    this.retryDelayMs = this.retryMinMs;

    if (!this.isDirty && !this.inFlight) {
      return true;
    }
    if (this.inFlight) {
      this.saveAgainImmediate = true;
      return false;
    }
    return this.performSave(false);
  }

  /**
   * Resolves a 409 conflict by overwriting the hub draft with the local buffer
   * using the server's latest revision.
   */
  async resolveConflictKeep(): Promise<boolean> {
    if (this.disposed || !this.conflict) return false;
    this.rev = this.conflict.rev;
    this.conflict = null;
    this.pausedReason = null;
    this.clearAutosaveTimer();
    this.clearRetryTimer();
    this.retryDelayMs = this.retryMinMs;
    return this.performSave(false);
  }

  /**
   * Resolves a 409 conflict by fetching the newer draft from the hub and
   * adopting its revision, body, note, and savedAt.
   */
  async resolveConflictLoad(): Promise<ReviewDraft | null> {
    if (this.disposed) return null;
    const res = await this.getFn(this.artifactId);
    if (this.disposed) return null;
    if (!res.draft) {
      this.rev = 0;
      this.savedAt = null;
      this.conflict = null;
      this.pausedReason = null;
      this.status = 'empty';
      this.emit();
      return null;
    }
    const d = res.draft;
    this.rev = d.rev;
    this.base = d.base;
    this.entry = d.entry;
    this.carried = d.carried;
    this.ackedBody = d.body;
    this.ackedNote = d.note;
    this.savedAt = d.savedAt;
    this.conflict = null;
    this.pausedReason = null;
    this.clearAutosaveTimer();
    this.clearRetryTimer();
    this.retryDelayMs = this.retryMinMs;
    this.status = 'saved';
    this.emit();
    return d;
  }

  /**
   * Conditionally deletes the saved draft after a successful Submit Review
   * (`DELETE /review-draft?rev=<rev>`). Returns:
   * - `'none'` when no draft was ever saved on the hub (rev === 0)
   * - `'deleted'` when the hub deleted the draft (204)
   * - `'conflict'` when another device/tab saved a newer draft (409)
   * - `'failed'` on other errors (harmless; stale draft offered on next visit)
   */
  async deleteAfterSubmit(): Promise<'none' | 'deleted' | 'conflict' | 'failed'> {
    this.clearAutosaveTimer();
    this.clearRetryTimer();
    if (this.rev <= 0) return 'none';
    const expectedRev = this.rev;
    try {
      await this.delFn(this.artifactId, { rev: expectedRev });
      this.rev = 0;
      this.savedAt = null;
      this.conflict = null;
      this.pausedReason = null;
      this.status = 'empty';
      this.emit();
      return 'deleted';
    } catch (err) {
      if (err instanceof DraftConflictError) {
        return 'conflict';
      }
      return 'failed';
    }
  }

  /** Stops all timers, aborts any in-flight save, and removes window/document listeners. */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.clearAutosaveTimer();
    this.clearRetryTimer();
    this.abortController?.abort();
    this.abortController = null;
    this.inFlight = false;
    this.doc.removeEventListener('visibilitychange', this.onVisibility);
    this.win.removeEventListener('beforeunload', this.onBeforeUnload);
  }

  private async performSave(keepalive: boolean): Promise<boolean> {
    if (this.disposed || this.inFlight) return false;
    const snap = this.opts.readState();
    const req: PutReviewDraftRequest = {
      rev: this.rev,
      base: this.base,
      entry: this.entry,
      body: snap.body,
      note: snap.note,
      ...(this.carried ? { carried: this.carried } : {}),
      clientSavedAt: this.nowFn().toISOString(),
    };

    if (keepalive) {
      const payloadBytes = utf8Encoder.encode(JSON.stringify(req)).length;
      if (payloadBytes > DRAFT_KEEPALIVE_LIMIT_BYTES) {
        return false;
      }
    }

    this.clearAutosaveTimer();
    this.clearRetryTimer();
    this.inFlight = true;
    this.saveAgainImmediate = false;
    this.status = 'saving';
    const ac = new AbortController();
    this.abortController = ac;
    this.emit();

    let ok = false;
    try {
      const res = await this.putFn(this.artifactId, req, {
        keepalive,
        signal: ac.signal,
      });
      if (this.disposed) return false;
      this.rev = res.rev;
      this.savedAt = res.savedAt;
      this.ackedBody = snap.body;
      this.ackedNote = snap.note;
      this.retryDelayMs = this.retryMinMs;
      this.pausedReason = null;
      this.conflict = null;
      ok = true;
    } catch (err) {
      if (this.disposed || ac.signal.aborted) return false;
      this.handleSaveError(err);
    } finally {
      if (this.abortController === ac) this.abortController = null;
      this.inFlight = false;
    }

    if (this.disposed) return false;

    if (ok) {
      if (this.isDirty) {
        if (this.saveAgainImmediate) {
          this.saveAgainImmediate = false;
          return this.performSave(false);
        }
        // Coalesced edits made while the PUT was in flight: start one fresh 30 s timer.
        this.status = 'unsaved';
        this.autosaveTimer = setTimeout(() => {
          this.autosaveTimer = null;
          void this.performSave(false);
        }, this.autosaveMs);
      } else {
        this.saveAgainImmediate = false;
        this.status = 'saved';
      }
      this.emit();
    } else {
      this.saveAgainImmediate = false;
      this.emit();
    }
    return ok;
  }

  private handleSaveError(err: unknown): void {
    if (err instanceof DraftConflictError) {
      this.pausedReason = 'conflict';
      this.conflict = { rev: err.rev, savedAt: err.savedAt, base: err.base };
      this.status = 'conflict';
      return;
    }
    if (err instanceof DraftApiError) {
      if (err.status === 413 || err.code === 'draft_too_large') {
        this.pausedReason = 'too_large';
        this.status = 'too_large';
        return;
      }
      if (err.status === 429 && err.code === 'too_many_drafts') {
        this.pausedReason = 'too_many_drafts';
        this.status = 'too_many_drafts';
        return;
      }
      if (err.status === 403 || err.status === 404) {
        this.pausedReason = 'no_access';
        this.status = 'no_access';
        return;
      }
      if (err.status === 429 && err.retryAfterMs !== undefined) {
        this.status = 'retrying';
        this.scheduleRetry(err.retryAfterMs);
        return;
      }
    }
    // Network error, 5xx, or 429 without Retry-After: exponential backoff 2 s -> 60 s.
    this.status = 'retrying';
    const delay = this.retryDelayMs;
    this.retryDelayMs = Math.min(this.retryDelayMs * 2, this.retryMaxMs);
    this.scheduleRetry(delay);
  }

  private scheduleRetry(delayMs: number): void {
    this.clearRetryTimer();
    this.retryTimer = setTimeout(
      () => {
        this.retryTimer = null;
        if (this.disposed || this.pausedReason || !this.isDirty) return;
        void this.performSave(false);
      },
      Math.max(0, delayMs)
    );
  }

  private clearAutosaveTimer(): void {
    if (this.autosaveTimer !== null) {
      clearTimeout(this.autosaveTimer);
      this.autosaveTimer = null;
    }
  }

  private clearRetryTimer(): void {
    if (this.retryTimer !== null) {
      clearTimeout(this.retryTimer);
      this.retryTimer = null;
    }
  }

  private emit(): void {
    if (!this.disposed) this.opts.onChange(this.getSnapshot());
  }

  private onVisibility = (): void => {
    if (this.disposed || this.doc.visibilityState !== 'hidden') return;
    if (this.pausedReason || this.inFlight || !this.isDirty) return;
    void this.performSave(true);
  };

  private onBeforeUnload = (e: BeforeUnloadEvent): void => {
    if (this.disposed || !this.hasUnsavedChanges) return;
    e.preventDefault();
    e.returnValue = '';
  };
}
