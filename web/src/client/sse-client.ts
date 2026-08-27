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
 */

/** Data shape for SSE 'update' events from the server */
export interface SSEUpdateEvent {
  subject: string;
  data: unknown;
}

type SSEClientEventMap = {
  update: CustomEvent<SSEUpdateEvent>;
  connected: CustomEvent<{ connectionId: string; subjects: string[] }>;
  disconnected: CustomEvent<void>;
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

  /**
   * Build the SSE URL with subscription subjects as query parameters.
   * Maps to the WatchRequest pattern.
   */
  private buildUrl(subjects: string[]): string {
    const params = subjects.map((s) => `sub=${encodeURIComponent(s)}`).join('&');
    return `/events?${params}`;
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

    // Each handler bails unless es is still the client's current connection,
    // so a superseded connection cannot mutate state it no longer owns.
    es.onopen = () => {
      if (es !== this.eventSource) return;
      this.reconnectAttempts = 0;
      this.connectionOpen = true;
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
        void this.checkAuthAndReconnect();
      } else {
        this.scheduleReconnect();
      }
    };

    // Handle state update events from the server
    this.eventSource.addEventListener('update', (event) => {
      if (es !== this.eventSource) return;
      try {
        const data = JSON.parse((event as MessageEvent).data) as SSEUpdateEvent;
        this.dispatchEvent(new CustomEvent('update', { detail: data }));
      } catch (err) {
        console.error('[SSE] Failed to parse update event:', err);
      }
    });

    // Handle server-initiated reconnect (e.g. before a clean shutdown).
    // connectionOpen is left alone: the feed was live a moment ago, so if the
    // replacement connection never lands, that still counts as a drop.
    this.eventSource.addEventListener('reconnect', () => {
      if (es !== this.eventSource) return;
      this.reconnectAttempts = 0;
      es.close();
      this.eventSource = null;
      this.openConnection();
    });

    // Handle initial connection acknowledgement
    this.eventSource.addEventListener('connected', (event) => {
      if (es !== this.eventSource) return;
      try {
        const data = JSON.parse((event as MessageEvent).data) as {
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
      const resp = await fetch('/auth/me', { credentials: 'include' });
      if (resp.status === 401 || resp.redirected) {
        console.warn('[SSE] Session expired, redirecting to login');
        const returnTo = encodeURIComponent(window.location.pathname);
        window.location.href = `/login?error=session_expired&returnTo=${returnTo}`;
        return;
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

  /**
   * Reconnect as soon as the tab is shown, not after whatever backoff was
   * pending when it was hidden. Mobile browsers suspend a backgrounded tab and
   * close its connections, so the feed is usually dead on return.
   */
  private watchVisibility(): void {
    if (this.onVisibilityChange || typeof document === 'undefined') {
      return;
    }
    this.onVisibilityChange = (): void => {
      if (document.visibilityState !== 'visible') return;
      if (this.connected || this.subjects.length === 0) return;
      // Restart the backoff from the shortest delay.
      this.reconnectAttempts = 0;
      this.reconnectNow();
    };
    document.addEventListener('visibilitychange', this.onVisibilityChange);
  }

  /**
   * Close the SSE connection and cancel any pending reconnection.
   *
   * Also drops the visibility listener: it holds a reference to this client,
   * and left behind it would reopen a connection the caller just tore down.
   * connect() re-registers it, so a disconnected client stays reusable.
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
