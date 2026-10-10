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
 * SSE Client
 *
 * Manages an EventSource connection to the server's /events endpoint.
 * Subscriptions are declared as query parameters at connection time and
 * are immutable for the connection lifetime. To change subscriptions,
 * disconnect and reconnect with different subjects.
 *
 * Provides automatic reconnection with exponential backoff and
 * Last-Event-ID resume support (handled natively by EventSource).
 *
 * Reconnection retries indefinitely; a tab that becomes visible reconnects
 * immediately.
 *
 * A stream can also die without the browser noticing: after a mobile PWA
 * returns from the background, EventSource may still report OPEN although
 * nothing will ever arrive on it. The client therefore tracks when it last
 * heard from the server and replaces a connection that has gone silent for
 * longer than staleAfterMs (see onResume and isStale).
 */

import { dispatchTeardown } from '../utils/auth.js';
import { setPreferredTimeZone } from '../utils/time.js';
import type { AuthMeResponse } from '../shared/types.js';

/** Data shape for SSE 'update' events from the server */
export interface SSEUpdateEvent {
  subject: string;
  data: unknown;
}

type SSEClientEventMap = {
  update: CustomEvent<SSEUpdateEvent>;
  connected: CustomEvent<{ connectionId: string; subjects: string[] }>;
  disconnected: CustomEvent<void>;
  'handshake-failed': CustomEvent<void>;
  reconnecting: CustomEvent<{ attempt: number }>;
};

export class SSEClient extends EventTarget {
  private eventSource: EventSource | null = null;
  private reconnectAttempts = 0;
  /**
   * Bumped whenever the connection lifecycle changes. An async continuation
   * that captured an older value has been superseded and must not act.
   */
  private generation = 0;
  private baseReconnectDelay = 1000;
  /** Backoff cap while a drop still looks transient. */
  private fastReconnectCap = 30_000;
  /**
   * Backoff cap after fastReconnectAttempts failures.
   */
  private slowReconnectCap = 300_000;
  /** Attempts spent at the fast cap before the slow one takes over. */
  private fastReconnectAttempts = 10;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private subjects: string[] = [];
  /**
   * Whether a connection has been live since the last drop was reported.
   * Guards the 'disconnected' event so listeners hear one drop per outage
   * rather than one per failed retry.
   */
  private connectionOpen = false;
  private onVisibilityChange: (() => void) | null = null;
  private onPageShow: (() => void) | null = null;
  private onOnline: (() => void) | null = null;
  private onOffline: (() => void) | null = null;
  private onPageHide: (() => void) | null = null;

  /**
   * How long a connection may stay silent before it is presumed dead. The
   * hub writes a heartbeat every 30s, so this allows two missed heartbeats
   * plus slack.
   */
  private staleAfterMs = 75_000;
  /**
   * The shortest absence after which a resume treats the stream as suspect.
   * Quick app switches (glancing at a notification, copying a code) last a
   * few seconds and the browser keeps the socket; past this, a mobile OS
   * may already have suspended the page and silently lost the stream.
   * Gating on the trip, not on when traffic last arrived, keeps a brief
   * switch from ever reconnecting a healthy idle feed.
   */
  private minSuspendMs = 15_000;
  /**
   * How often a visible tab checks the current connection for staleness.
   * The check only acts on a connection that has delivered a named
   * 'heartbeat' event, so it stays inert until the hub sends its heartbeat
   * as an event rather than a comment.
   */
  private staleCheckIntervalMs = 15_000;
  private staleCheckTimer: ReturnType<typeof setInterval> | null = null;
  /**
   * One deferred check, armed on a resume that came too soon to judge: the
   * connection was suspended but had not yet been silent for staleAfterMs.
   * It fires when that much silence would have passed, and is cancelled by
   * any new activity, by hiding again, and by disconnect().
   */
  private resumeCheckTimer: ReturnType<typeof setTimeout> | null = null;
  /** When anything last arrived from the server (open, event or heartbeat). */
  private lastActivityAt = 0;
  /**
   * Whether the current connection has delivered a 'heartbeat' event. The
   * hub's heartbeat is an SSE comment, which EventSource does not expose, so
   * silence on its own may just mean an idle stream. Only a server known to
   * send heartbeat events lets silence alone, while visible, condemn it.
   */
  private heartbeatSeen = false;
  /**
   * When the tab was last hidden or the device went offline, cleared once a
   * resume has been checked. Being away longer than staleAfterMs with no
   * traffic since is the resume-time signal that the stream may be dead.
   */
  private suspendedAt: number | null = null;

  constructor(private readonly endpoint = '/events') {
    super();
  }

  /**
   * Build the SSE URL with subscription subjects as query parameters.
   * Maps to the WatchRequest pattern.
   */
  private buildUrl(subjects: string[]): string {
    const params = subjects.map((s) => `sub=${encodeURIComponent(s)}`).join('&');
    return `${this.endpoint}?${params}`;
  }

  /**
   * Open a connection scoped to the given subjects.
   * Closes any existing connection first.
   */
  connect(subjects: string[]): void {
    this.disconnect();
    this.subjects = subjects;
    this.reconnectAttempts = 0;
    this.openConnection();
  }

  /** Close and forget the current EventSource, if any. */
  private closeEventSource(): void {
    if (!this.eventSource) {
      return;
    }
    this.eventSource.close();
    this.eventSource = null;
  }

  private openConnection(): void {
    if (this.subjects.length === 0) {
      return;
    }

    this.watchVisibility();

    // Any connection still held here would be orphaned by the assignment
    // below: nothing else tracks it, so disconnect() could not close it and
    // its onerror would tear down its own replacement.
    this.closeEventSource();

    this.generation++;
    const url = this.buildUrl(this.subjects);
    const es = new EventSource(url);
    this.eventSource = es;
    this.heartbeatSeen = false;
    this.startStaleCheck();

    // Each handler bails unless es is still the client's current connection,
    // so a superseded connection cannot mutate state it no longer owns.
    es.onopen = () => {
      if (es !== this.eventSource) return;
      this.reconnectAttempts = 0;
      this.connectionOpen = true;
      this.markActivity();
      console.info('[SSE] Connected');
      // The browser's own open signal. The server-sent "connected" event this
      // client also listens for is never emitted by the hub, so consumers that
      // need to know the stream is live (state.connected, the chat thread's
      // catch-up refetch) would otherwise never hear anything.
      this.dispatchEvent(
        new CustomEvent('connected', {
          detail: { connectionId: '', subjects: [...this.subjects] },
        })
      );
    };

    es.onerror = () => {
      if (es !== this.eventSource) return;
      const wasOpen = this.connectionOpen;
      this.connectionOpen = false;
      this.closeEventSource();

      // Once per live connection, not once per failed retry.
      if (wasOpen) {
        this.dispatchEvent(new CustomEvent('disconnected'));
      }

      // A handshake that never opened may have been rejected rather than
      // dropped - a 401, or a redirect to the login page after the session
      // was invalidated - so probe auth before retrying it forever. A
      // connection that was live and then failed is a network drop and goes
      // straight to backoff. readyState cannot make this distinction: per
      // spec a rejected handshake lands CLOSED and a mid-stream drop lands
      // CONNECTING, which is the opposite way round.
      if (!wasOpen) {
        this.dispatchEvent(new CustomEvent('handshake-failed'));
        void this.checkAuthAndReconnect();
      } else {
        this.scheduleReconnect();
      }
    };

    // Handle state update events from the server
    this.eventSource.addEventListener('update', (event) => {
      if (es !== this.eventSource) return;
      this.markActivity();
      try {
        const data = JSON.parse(event.data as string) as SSEUpdateEvent;
        this.dispatchEvent(new CustomEvent('update', { detail: data }));
      } catch (err) {
        console.error('[SSE] Failed to parse update event:', err);
      }
    });

    // Handle server-initiated reconnect (e.g. before a clean shutdown).
    // Report the gap immediately so snapshot consumers invalidate in-flight work.
    this.eventSource.addEventListener('reconnect', () => {
      if (es !== this.eventSource) return;
      if (this.connectionOpen) {
        this.connectionOpen = false;
        this.dispatchEvent(new CustomEvent('disconnected'));
      }
      this.reconnectAttempts = 0;
      es.close();
      this.eventSource = null;
      this.openConnection();
    });

    // A heartbeat sent as a named event (not a comment) is visible here and
    // proves the stream is alive while idle.
    this.eventSource.addEventListener('heartbeat', () => {
      if (es !== this.eventSource) return;
      this.heartbeatSeen = true;
      this.markActivity();
    });

    // Handle initial connection acknowledgement
    this.eventSource.addEventListener('connected', (event) => {
      if (es !== this.eventSource) return;
      this.markActivity();
      try {
        const data = JSON.parse(event.data as string) as {
          connectionId: string;
          subjects: string[];
        };
        console.info('[SSE] Connection established:', data.connectionId);
        this.dispatchEvent(new CustomEvent('connected', { detail: data }));
      } catch (err) {
        console.error('[SSE] Failed to parse connected event:', err);
      }
    });
  }

  /**
   * Check whether the session is still valid before reconnecting.
   * If the session was invalidated (e.g. signing key rotation),
   * redirect to the login page instead of retrying.
   */
  private async checkAuthAndReconnect(): Promise<void> {
    const generation = this.generation;
    try {
      const authUrl = this.endpoint.startsWith('/')
        ? '/auth/me'
        : new URL('auth/me', this.endpoint).href;
      const resp = await fetch(authUrl, { credentials: 'include' });
      if (generation !== this.generation) return;
      if (resp.status === 401 || resp.redirected) {
        console.warn('[SSE] Session expired, redirecting to login');
        // Dispose terminal sessions before navigating to login so no hidden
        // active terminals survive the transition.
        dispatchTeardown('auth-expired');
        const returnTo = encodeURIComponent(window.location.pathname);
        window.location.href = `/login?error=session_expired&returnTo=${returnTo}`;
        return;
      }
      // Review R1-6: this reconnect check already fetches /auth/me live, so
      // it is also the place a preference changed in another tab or on
      // another device reaches a long-lived tab — refresh the effective-zone
      // store from the same response instead of leaving it stale until some
      // unrelated render.
      if (resp.ok) {
        try {
          const data = (await resp.json()) as AuthMeResponse;
          setPreferredTimeZone(data.preferences?.timezone);
        } catch {
          // Malformed/non-JSON body — not this check's concern.
        }
      }
    } catch {
      // Network error — fall through to normal reconnect.
    }
    // A connect(), reconnectNow() or disconnect() during the await has already
    // decided what happens next; scheduling here would race it.
    if (generation !== this.generation) {
      return;
    }
    this.scheduleReconnect();
  }

  /**
   * Queue the next connection attempt with exponential backoff.
   */
  private scheduleReconnect(): void {
    // A retry already queued, or a deliberate disconnect, cancels this one.
    if (this.reconnectTimer !== null || this.subjects.length === 0) {
      return;
    }

    const cap =
      this.reconnectAttempts < this.fastReconnectAttempts
        ? this.fastReconnectCap
        : this.slowReconnectCap;
    const base = Math.min(cap, this.baseReconnectDelay * 2 ** this.reconnectAttempts);
    // Proportional jitter (up to 10 percent either way), so open tabs spread
    // out at every cap rather than clustering within a fixed 500ms window.
    const delay = base + (Math.random() * 2 - 1) * base * 0.1;
    this.reconnectAttempts++;

    console.info(
      `[SSE] Reconnecting in ${Math.round(delay)}ms (attempt ${this.reconnectAttempts})`
    );
    this.dispatchEvent(
      new CustomEvent('reconnecting', { detail: { attempt: this.reconnectAttempts } })
    );

    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.openConnection();
    }, delay);
  }

  /**
   * Cancel any pending backoff and connect immediately.
   */
  private reconnectNow(): void {
    if (this.subjects.length === 0) {
      return;
    }

    // Leave an open connection or in-flight handshake to resolve on its own.
    const readyState = this.eventSource?.readyState;
    if (readyState === EventSource.OPEN || readyState === EventSource.CONNECTING) {
      return;
    }

    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    this.openConnection();
  }

  private markActivity(): void {
    this.lastActivityAt = Date.now();
    this.clearResumeCheck();
  }

  private clearResumeCheck(): void {
    if (this.resumeCheckTimer !== null) {
      clearTimeout(this.resumeCheckTimer);
      this.resumeCheckTimer = null;
    }
  }

  /**
   * Re-check a resumed connection once it has been silent for staleAfterMs.
   * The suspension already makes it suspect, so silence alone then condemns
   * it; any traffic before then clears this check (markActivity).
   */
  private scheduleResumeCheck(): void {
    this.clearResumeCheck();
    const delay = Math.max(0, this.lastActivityAt + this.staleAfterMs - Date.now()) + 1;
    this.resumeCheckTimer = setTimeout(() => {
      this.resumeCheckTimer = null;
      if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return;
      if (this.isSilent()) this.reconnectStale();
    }, delay);
  }

  /** Whether nothing has arrived for longer than staleAfterMs. */
  private isSilent(): boolean {
    return Date.now() - this.lastActivityAt > this.staleAfterMs;
  }

  /**
   * Whether silence alone condemns the open connection: only once it is
   * known to send heartbeat events, so an idle stream in a tab that stayed
   * in view is left alone.
   */
  private isStale(): boolean {
    return this.heartbeatSeen && this.isSilent();
  }

  /**
   * Replace a connection the browser still reports as open but that has
   * gone silent. Reported as a drop so listeners resync once it reopens.
   * The replacement starts CONNECTING, which every trigger here skips, so
   * at most one reconnect is in flight; a failed handshake falls back to
   * the normal auth probe and backoff.
   */
  private reconnectStale(): void {
    if (!this.connected || this.reconnectTimer !== null) return;
    console.info('[SSE] Connection silent, reconnecting');
    if (this.connectionOpen) {
      this.connectionOpen = false;
      this.dispatchEvent(new CustomEvent('disconnected'));
    }
    this.clearResumeCheck();
    this.closeEventSource();
    this.openConnection();
  }

  /**
   * Called when the tab is shown, the page is restored, or the network
   * returns. A dead connection reconnects immediately, not after whatever
   * backoff was pending. An open one is replaced if it has gone stale, or,
   * after an absence long enough that the page may have been suspended, if
   * it has been silent for staleAfterMs - now, or once that much silence
   * has passed.
   */
  private onResume(): void {
    if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return;
    if (this.subjects.length === 0) return;
    const tripMs = this.suspendedAt === null ? 0 : Date.now() - this.suspendedAt;
    this.suspendedAt = null;
    if (this.connected) {
      if (this.isStale()) {
        this.reconnectStale();
      } else if (tripMs >= this.minSuspendMs) {
        if (this.isSilent()) {
          this.reconnectStale();
        } else {
          // Too soon to tell: traffic arrived shortly before or during the
          // absence. Look again once the silence would count as stale.
          this.scheduleResumeCheck();
        }
      }
    } else {
      // A stream the browser closed while the page was frozen (a bfcache
      // restore) never fired an error, so the drop was never reported.
      // Report it, as onerror would have, so listeners resync on reopen.
      if (this.connectionOpen) {
        this.connectionOpen = false;
        this.dispatchEvent(new CustomEvent('disconnected'));
      }
      // Restart the backoff from the shortest delay.
      this.reconnectAttempts = 0;
      this.reconnectNow();
    }
  }

  private markSuspended(): void {
    this.clearResumeCheck();
    if (this.suspendedAt === null) this.suspendedAt = Date.now();
  }

  /** Check a visible tab's connection for staleness every so often. */
  private startStaleCheck(): void {
    if (this.staleCheckTimer !== null) return;
    this.staleCheckTimer = setInterval(() => {
      if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return;
      if (this.isStale()) this.reconnectStale();
    }, this.staleCheckIntervalMs);
  }

  /**
   * Reconnect as soon as the tab is shown, not after whatever backoff was
   * pending when it was hidden. Mobile browsers suspend a backgrounded tab and
   * close its connections, so the feed is usually dead on return - or, worse,
   * still reports OPEN while nothing arrives, which onResume catches.
   */
  private watchVisibility(): void {
    if (this.onVisibilityChange || typeof document === 'undefined') {
      return;
    }
    this.onVisibilityChange = (): void => {
      if (document.visibilityState === 'visible') {
        this.onResume();
      } else {
        this.markSuspended();
      }
    };
    this.onPageShow = (): void => this.onResume();
    this.onOnline = (): void => this.onResume();
    this.onOffline = (): void => this.markSuspended();
    this.onPageHide = (): void => this.markSuspended();
    document.addEventListener('visibilitychange', this.onVisibilityChange);
    if (typeof window !== 'undefined') {
      window.addEventListener('pageshow', this.onPageShow);
      window.addEventListener('pagehide', this.onPageHide);
      window.addEventListener('online', this.onOnline);
      window.addEventListener('offline', this.onOffline);
    }
  }

  /**
   * Close the SSE connection and cancel any pending reconnection.
   *
   * Also drops the resume listeners (visibilitychange, pageshow, pagehide,
   * online, offline) and the staleness timers: they hold a reference to this
   * client, and left behind they would reopen a connection the caller just
   * tore down. connect() re-registers them, so a disconnected client stays
   * reusable.
   */
  disconnect(): void {
    // Supersede any in-flight auth probe, which would otherwise schedule a
    // reconnect for a client the caller just tore down.
    this.generation++;

    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }

    if (this.onVisibilityChange) {
      document.removeEventListener('visibilitychange', this.onVisibilityChange);
      this.onVisibilityChange = null;
    }
    if (typeof window !== 'undefined') {
      if (this.onPageShow) window.removeEventListener('pageshow', this.onPageShow);
      if (this.onPageHide) window.removeEventListener('pagehide', this.onPageHide);
      if (this.onOnline) window.removeEventListener('online', this.onOnline);
      if (this.onOffline) window.removeEventListener('offline', this.onOffline);
    }
    this.onPageShow = null;
    this.onPageHide = null;
    this.onOnline = null;
    this.onOffline = null;

    if (this.staleCheckTimer !== null) {
      clearInterval(this.staleCheckTimer);
      this.staleCheckTimer = null;
    }
    this.clearResumeCheck();
    this.heartbeatSeen = false;
    this.suspendedAt = null;

    this.closeEventSource();

    this.subjects = [];
    this.reconnectAttempts = 0;
    this.connectionOpen = false;
  }

  /** Whether the connection is currently open */
  get connected(): boolean {
    return this.eventSource?.readyState === EventSource.OPEN;
  }

  /** Current subscription subjects */
  get currentSubjects(): string[] {
    return this.subjects;
  }

  /** Number of reconnect attempts since last successful connection */
  get reconnectAttemptCount(): number {
    return this.reconnectAttempts;
  }

  // Typed addEventListener overloads
  addEventListener<K extends keyof SSEClientEventMap>(
    type: K,
    listener: (ev: SSEClientEventMap[K]) => void,
    options?: boolean | AddEventListenerOptions
  ): void;
  addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject,
    options?: boolean | AddEventListenerOptions
  ): void;
  addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject | ((ev: CustomEvent) => void),
    options?: boolean | AddEventListenerOptions
  ): void {
    super.addEventListener(type, listener as EventListenerOrEventListenerObject, options);
  }
}
